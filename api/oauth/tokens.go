package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

var _ op.Storage = (*Storage)(nil)

func tokenRequest(req op.TokenRequest) (RefreshRequest, error) {
	switch req := req.(type) {
	case *AuthRequest:
		if !req.Done() {
			return RefreshRequest{}, oidc.ErrAccessDenied()
		}
		return RefreshRequest{UserID: req.UserID, ClientID: req.GetClientID(), Resource: req.Resource, Scopes: req.GetScopes(), AuthTime: req.GetAuthTime()}, nil
	case *RefreshRequest:
		copy := *req
		copy.Scopes = slices.Clone(req.Scopes)
		return copy, nil
	default:
		return RefreshRequest{}, oidc.ErrUnsupportedGrantType()
	}
}

func (s *Storage) CreateAccessToken(ctx context.Context, req op.TokenRequest) (string, time.Time, error) {
	id, _, expires, err := s.issue(ctx, req, "", false)
	return id, expires, err
}
func (s *Storage) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	return s.issue(ctx, req, current, true)
}

func (s *Storage) issue(ctx context.Context, req op.TokenRequest, current string, refresh bool) (string, string, time.Time, error) {
	request, err := tokenRequest(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	binding, _ := ctx.Value(requestKey{}).(requestBinding)
	if request.Resource == "" || binding.Resource != request.Resource || binding.ClientID != request.ClientID {
		return "", "", time.Time{}, oidc.ErrInvalidGrant().WithDescription("Token request binding did not match")
	}
	id, err := secret()
	if err != nil {
		return "", "", time.Time{}, err
	}
	next := ""
	if refresh {
		next, err = secret()
		if err != nil {
			return "", "", time.Time{}, err
		}
		next = "hmr_" + next
	}
	expires := time.Now().Add(AccessLifetime)
	reused := false
	err = s.userTx(ctx, request.UserID, func(tx pgx.Tx) error {
		grants, err := grantScopes(ctx, tx, request.UserID, request.Resource)
		if err != nil {
			return err
		}
		if !resourceScopeSubset(request.Scopes, grants) {
			return oidc.ErrInvalidScope()
		}
		if refresh && current != "" {
			var data []byte
			var used, revoked *time.Time
			var familyExpiry time.Time
			err = tx.QueryRow(ctx, `SELECT r.data,r.used_at,f.revoked_at,f.expires_at FROM oauth_refresh_tokens r JOIN oauth_families f ON f.id=r.family_id
				WHERE r.token_hash=$1 AND f.user_id=$2::uuid AND f.client_id=$3 AND f.resource=$4 FOR UPDATE OF r,f`, hash(current), request.UserID, request.ClientID, request.Resource).
				Scan(&data, &used, &revoked, &familyExpiry)
			if err != nil || revoked != nil || !familyExpiry.After(time.Now()) {
				return op.ErrInvalidRefreshToken
			}
			var previous RefreshRequest
			if json.Unmarshal(data, &previous) != nil {
				return op.ErrInvalidRefreshToken
			}
			request.FamilyID = previous.FamilyID
			if used != nil {
				// Commit this revocation even though the caller receives an error.
				// Returning an error from the transaction would roll it back.
				reused = true
				_, err = tx.Exec(ctx, `UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now()) WHERE id=$1`, request.FamilyID)
				return err
			}
			for _, scope := range request.Scopes {
				if !slices.Contains(previous.Scopes, scope) {
					return oidc.ErrInvalidScope()
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET used_at=now() WHERE token_hash=$1`, hash(current)); err != nil {
				return err
			}
		} else if refresh {
			request.FamilyID, err = secret()
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO oauth_families(id,user_id,client_id,resource,expires_at) VALUES($1,$2::uuid,$3,$4,now()+interval '30 days')`, request.FamilyID, request.UserID, request.ClientID, request.Resource); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO oauth_access_tokens(id,user_id,client_id,resource,family_id,scopes,expires_at) VALUES($1,$2::uuid,$3,$4,NULLIF($5,''),$6,$7)`, id, request.UserID, request.ClientID, request.Resource, request.FamilyID, request.Scopes, expires)
		if err != nil {
			return err
		}
		if refresh {
			data, err := json.Marshal(request)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO oauth_refresh_tokens(token_hash,family_id,data) VALUES($1,$2,$3)`, hash(next), request.FamilyID, data)
			return err
		}
		return nil
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	if reused {
		return "", "", time.Time{}, op.ErrInvalidRefreshToken
	}
	return id, next, expires, nil
}

func (s *Storage) TokenRequestByRefreshToken(ctx context.Context, token string) (op.RefreshTokenRequest, error) {
	if !strings.HasPrefix(token, "hmr_") || len(token) != 47 {
		return nil, op.ErrInvalidRefreshToken
	}
	var data []byte
	var used *time.Time
	var family string
	err := s.Pool.QueryRow(ctx, `SELECT r.data,r.used_at,f.id FROM oauth_refresh_tokens r JOIN oauth_families f ON f.id=r.family_id
		JOIN users u ON u.id=f.user_id WHERE r.token_hash=$1 AND f.revoked_at IS NULL AND f.expires_at>now() AND u.disabled_at IS NULL`, hash(token)).Scan(&data, &used, &family)
	if err != nil {
		return nil, op.ErrInvalidRefreshToken
	}
	if used != nil {
		if _, err = s.Pool.Exec(ctx, `UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now()) WHERE id=$1`, family); err != nil {
			return nil, errors.New("cannot invalidate reused refresh family")
		}
		return nil, op.ErrInvalidRefreshToken
	}
	var request RefreshRequest
	if json.Unmarshal(data, &request) != nil {
		return nil, op.ErrInvalidRefreshToken
	}
	binding, _ := ctx.Value(requestKey{}).(requestBinding)
	if request.Resource != binding.Resource || request.ClientID != binding.ClientID {
		return nil, op.ErrInvalidRefreshToken
	}
	return &request, nil
}

func (s *Storage) GetRefreshTokenInfo(ctx context.Context, client, token string) (string, string, error) {
	if !strings.HasPrefix(token, "hmr_") {
		return "", "", op.ErrInvalidRefreshToken
	}
	var user string
	err := s.Pool.QueryRow(ctx, `SELECT f.user_id::text FROM oauth_refresh_tokens r JOIN oauth_families f ON f.id=r.family_id WHERE r.token_hash=$1 AND f.client_id=$2`, hash(token), client).Scan(&user)
	if err != nil {
		return "", "", op.ErrInvalidRefreshToken
	}
	return user, "refresh:" + base64.RawURLEncoding.EncodeToString(hash(token)), nil
}

func (s *Storage) RevokeToken(ctx context.Context, token, _, client string) *oidc.Error {
	var err error
	if strings.HasPrefix(token, "refresh:") {
		digest, decodeErr := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "refresh:"))
		if decodeErr != nil || len(digest) != 32 {
			return nil
		}
		_, err = s.Pool.Exec(ctx, `UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now()) WHERE client_id=$1 AND id IN (SELECT family_id FROM oauth_refresh_tokens WHERE token_hash=$2)`, client, digest)
	} else {
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now()) WHERE client_id=$1 AND id IN(SELECT family_id FROM oauth_access_tokens WHERE id=$2 AND client_id=$1)`, client, token); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE oauth_access_tokens SET revoked_at=coalesce(revoked_at,now()) WHERE id=$1 AND client_id=$2`, token, client)
			return err
		})
	}
	if err != nil {
		return oidc.ErrServerError().WithDescription("Cannot revoke token")
	}
	return nil
}

func (s *Storage) TerminateSession(ctx context.Context, user, client string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now()) WHERE user_id=$1::uuid AND client_id=$2`, user, client); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE oauth_access_tokens SET revoked_at=coalesce(revoked_at,now()) WHERE user_id=$1::uuid AND client_id=$2`, user, client)
		return err
	})
}
