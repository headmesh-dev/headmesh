package oauth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

type requestKey struct{}
type requestBinding struct{ Resource, ClientID string }

type Storage struct {
	Pool    *pgxpool.Pool
	Issuer  string
	key     signingKey
	Clients *ClientResolver
}

func NewStorage(ctx context.Context, pool *pgxpool.Pool, issuer string, private ed25519.PrivateKey) (*Storage, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid OAuth signing key")
	}
	public := private.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(public)
	id := base64.RawURLEncoding.EncodeToString(digest[:12])
	s := &Storage{Pool: pool, Issuer: issuer, key: signingKey{private: private, id: id}}
	s.Clients = NewClientResolver(pool)
	jwk, err := json.Marshal(jose.JSONWebKey{Key: public, KeyID: id, Algorithm: "EdDSA", Use: "sig"})
	if err != nil {
		return nil, errors.New("encode OAuth signing key")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE oauth_signing_keys SET expires_at=least(expires_at,now()+interval '16 minutes') WHERE id<>$1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO oauth_signing_keys(id,jwk,expires_at) VALUES($1,$2,now()+interval '365 days') ON CONFLICT(id) DO UPDATE SET expires_at=excluded.expires_at`, id, jwk)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("persist OAuth public key: %w", err)
	}
	return s, nil
}

// userTx sets a transaction-local identity for resource-grant row isolation.
// The identity must come from an authenticated browser session or stored grant,
// never from a request's query parameters.
func (s *Storage) userTx(ctx context.Context, user string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('headmesh.user_id',$1,true)`, user); err != nil {
			return err
		}
		return fn(tx)
	})
}

func grantScopes(ctx context.Context, tx pgx.Tx, user, resource string) ([]string, error) {
	var scopes []string
	err := tx.QueryRow(ctx, `SELECT g.scopes FROM oauth_resource_grants g JOIN users u ON u.id=g.user_id
		WHERE g.user_id=$1::uuid AND g.resource=$2 AND g.revoked_at IS NULL AND u.disabled_at IS NULL`, user, resource).Scan(&scopes)
	if err != nil {
		return nil, oidc.ErrAccessDenied().WithDescription("No access grant for this sandbox")
	}
	return scopes, nil
}

func (s *Storage) CreateAuthRequest(ctx context.Context, req *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	binding, _ := ctx.Value(requestKey{}).(requestBinding)
	if binding.Resource == "" || req.CodeChallengeMethod != oidc.CodeChallengeMethodS256 || len(req.CodeChallenge) != 43 {
		return nil, oidc.ErrInvalidRequest().WithDescription("A resource and S256 PKCE are required")
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_resources WHERE resource=$1)`, binding.Resource).Scan(&exists); err != nil || !exists {
		return nil, oidc.ErrInvalidRequest().WithDescription("Unknown resource")
	}
	if slices.Contains(req.Prompt, oidc.PromptNone) {
		return nil, oidc.ErrLoginRequired()
	}
	id, err := secret()
	if err != nil {
		return nil, err
	}
	a := &AuthRequest{ID: id, Request: *req, Resource: binding.Resource}
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO oauth_requests(id,data,expires_at) VALUES($1,$2,now()+interval '10 minutes')`, id, data)
	if err != nil {
		return nil, fmt.Errorf("save authorization request: %w", err)
	}
	return a, nil
}

func (s *Storage) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	var data []byte
	if err := s.Pool.QueryRow(ctx, `SELECT data FROM oauth_requests WHERE id=$1 AND expires_at>now()`, id).Scan(&data); err != nil {
		return nil, oidc.ErrInvalidRequest().WithDescription("Authorization request expired")
	}
	var a AuthRequest
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, errors.New("invalid stored authorization request")
	}
	return &a, nil
}

// Deletion is the one-use claim. Concurrent exchanges cannot both issue tokens.
func (s *Storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	var data []byte
	if err := s.Pool.QueryRow(ctx, `DELETE FROM oauth_requests WHERE code_hash=$1 AND expires_at>now() RETURNING data`, hash(code)).Scan(&data); err != nil {
		return nil, oidc.ErrInvalidGrant().WithDescription("Code invalid or already used")
	}
	var a AuthRequest
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, errors.New("invalid stored authorization request")
	}
	binding, _ := ctx.Value(requestKey{}).(requestBinding)
	if !a.Done() || binding.Resource != a.Resource {
		return nil, oidc.ErrInvalidGrant().WithDescription("Code resource did not match")
	}
	return &a, nil
}

func (s *Storage) SaveAuthCode(ctx context.Context, id, code string) error {
	result, err := s.Pool.Exec(ctx, `UPDATE oauth_requests SET code_hash=$2,expires_at=least(expires_at,now()+interval '1 minute') WHERE id=$1 AND code_hash IS NULL AND expires_at>now()`, id, hash(code))
	if err != nil || result.RowsAffected() != 1 {
		return oidc.ErrInvalidGrant().WithDescription("Authorization was already completed")
	}
	return nil
}
func (s *Storage) DeleteAuthRequest(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM oauth_requests WHERE id=$1`, id)
	return err
}
func (s *Storage) SigningKey(context.Context) (op.SigningKey, error) { return &s.key, nil }
func (s *Storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.EdDSA}, nil
}
func (s *Storage) KeySet(ctx context.Context) ([]op.Key, error) {
	rows, err := s.Pool.Query(ctx, `SELECT jwk FROM oauth_signing_keys WHERE expires_at>now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []op.Key
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var jwk jose.JSONWebKey
		if err := json.Unmarshal(data, &jwk); err != nil {
			return nil, err
		}
		keys = append(keys, publicKey{jwk: jwk})
	}
	return keys, rows.Err()
}
func (s *Storage) GetClientByClientID(ctx context.Context, id string) (op.Client, error) {
	return s.Clients.Resolve(ctx, id)
}
func (s *Storage) AuthorizeClientIDSecret(context.Context, string, string) error {
	return oidc.ErrInvalidClient().WithDescription("Only public clients with PKCE are supported")
}

// The library explicitly requires this deprecated hook to remain empty; user
// claims are populated through SetUserinfoFromRequest instead.
func (s *Storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}
func (s *Storage) SetUserinfoFromRequest(ctx context.Context, info *oidc.UserInfo, req op.IDTokenRequest, scopes []string) error {
	return s.userinfo(ctx, info, req.GetSubject(), scopes)
}
func (s *Storage) userinfo(ctx context.Context, info *oidc.UserInfo, user string, scopes []string) error {
	var name, email string
	if err := s.Pool.QueryRow(ctx, `SELECT coalesce(name,''),email FROM users WHERE id=$1::uuid AND disabled_at IS NULL`, user).Scan(&name, &email); err != nil {
		return oidc.ErrAccessDenied()
	}
	info.Subject = user
	if slices.Contains(scopes, oidc.ScopeProfile) {
		info.Name = name
		info.PreferredUsername = name
	}
	if slices.Contains(scopes, oidc.ScopeEmail) {
		info.Email = email
		info.EmailVerified = true
	}
	return nil
}
func (s *Storage) GetPrivateClaimsFromScopes(_ context.Context, _, _ string, scopes []string) (map[string]any, error) {
	return map[string]any{"token_use": "access", "scope": strings.Join(scopes, " ")}, nil
}
func (s *Storage) GetPrivateClaimsFromRequest(ctx context.Context, req op.TokenRequest, _ []string) (map[string]any, error) {
	return s.GetPrivateClaimsFromScopes(ctx, req.GetSubject(), "", req.GetScopes())
}
func (s *Storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, oidc.ErrUnauthorizedClient()
}
func (s *Storage) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, oidc.ErrUnsupportedGrantType()
}
func (s *Storage) Health(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Storage) SetUserinfoFromToken(ctx context.Context, info *oidc.UserInfo, id, subject, _ string) error {
	token, err := s.accessByID(ctx, id)
	if err != nil || token.UserID != subject {
		return oidc.ErrAccessDenied()
	}
	return s.userinfo(ctx, info, subject, token.Scopes)
}
func (s *Storage) SetIntrospectionFromToken(ctx context.Context, info *oidc.IntrospectionResponse, id, subject, client string) error {
	token, err := s.accessByID(ctx, id)
	if err != nil || token.UserID != subject || token.ClientID != client {
		return oidc.ErrAccessDenied()
	}
	info.Active = true
	info.Subject = subject
	info.ClientID = client
	info.Audience = []string{token.Resource}
	info.Scope = token.Scopes
	info.Expiration = oidc.FromTime(token.ExpiresAt)
	return nil
}

type accessRecord struct {
	ID, UserID, ClientID, Resource, FamilyID string
	Scopes                                   []string
	ExpiresAt                                time.Time
}

func (s *Storage) accessByID(ctx context.Context, id string) (accessRecord, error) {
	var token accessRecord
	err := s.Pool.QueryRow(ctx, `SELECT a.id,a.user_id::text,a.client_id,a.resource,coalesce(a.family_id,''),a.scopes,a.expires_at
		FROM oauth_access_tokens a JOIN users u ON u.id=a.user_id LEFT JOIN oauth_families f ON f.id=a.family_id
		WHERE a.id=$1 AND a.revoked_at IS NULL AND a.expires_at>now() AND u.disabled_at IS NULL
		AND (a.family_id IS NULL OR (f.revoked_at IS NULL AND f.expires_at>now()))`, id).
		Scan(&token.ID, &token.UserID, &token.ClientID, &token.Resource, &token.FamilyID, &token.Scopes, &token.ExpiresAt)
	if err != nil {
		return accessRecord{}, oidc.ErrAccessDenied()
	}
	err = s.userTx(ctx, token.UserID, func(tx pgx.Tx) error {
		grants, err := grantScopes(ctx, tx, token.UserID, token.Resource)
		if err != nil {
			return err
		}
		if !resourceScopeSubset(token.Scopes, grants) {
			return oidc.ErrAccessDenied()
		}
		return nil
	})
	return token, err
}
