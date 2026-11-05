// Package oauth implements Headmesh's MCP authorization server. Browser identity
// comes from package login; explicit resource grants determine sandbox access.
package oauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const AccessLifetime = 15 * time.Minute

var ResourceScopes = []string{"sandbox:read", "sandbox:exec", "sandbox:write"}
var AllowedScopes = []string{"sandbox:read", "sandbox:exec", "sandbox:write", "offline_access", "openid", "profile", "email"}

func secret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("cannot generate OAuth credential")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hash(value string) []byte { b := sha256.Sum256([]byte(value)); return b[:] }

type AuthRequest struct {
	ID              string
	Request         oidc.AuthRequest
	Resource        string
	UserID          string
	AuthenticatedAt time.Time
	Consented       bool
}

func (a *AuthRequest) GetID() string          { return a.ID }
func (a *AuthRequest) GetACR() string         { return "" }
func (a *AuthRequest) GetAMR() []string       { return []string{"federated"} }
func (a *AuthRequest) GetAudience() []string  { return []string{a.Resource} }
func (a *AuthRequest) GetAuthTime() time.Time { return a.AuthenticatedAt }
func (a *AuthRequest) GetClientID() string    { return a.Request.ClientID }
func (a *AuthRequest) GetCodeChallenge() *oidc.CodeChallenge {
	return &oidc.CodeChallenge{Challenge: a.Request.CodeChallenge, Method: a.Request.CodeChallengeMethod}
}
func (a *AuthRequest) GetNonce() string                   { return a.Request.Nonce }
func (a *AuthRequest) GetRedirectURI() string             { return a.Request.RedirectURI }
func (a *AuthRequest) GetResponseType() oidc.ResponseType { return a.Request.ResponseType }
func (a *AuthRequest) GetResponseMode() oidc.ResponseMode { return a.Request.ResponseMode }
func (a *AuthRequest) GetScopes() []string                { return slices.Clone(a.Request.Scopes) }
func (a *AuthRequest) GetState() string                   { return a.Request.State }
func (a *AuthRequest) GetSubject() string                 { return a.UserID }
func (a *AuthRequest) Done() bool                         { return a.Consented && a.UserID != "" }

type RefreshRequest struct {
	UserID, ClientID, Resource, FamilyID string
	Scopes                               []string
	AuthTime                             time.Time
}

func (r *RefreshRequest) GetAMR() []string                 { return []string{"federated"} }
func (r *RefreshRequest) GetAudience() []string            { return []string{r.Resource} }
func (r *RefreshRequest) GetAuthTime() time.Time           { return r.AuthTime }
func (r *RefreshRequest) GetClientID() string              { return r.ClientID }
func (r *RefreshRequest) GetScopes() []string              { return slices.Clone(r.Scopes) }
func (r *RefreshRequest) GetSubject() string               { return r.UserID }
func (r *RefreshRequest) SetCurrentScopes(scopes []string) { r.Scopes = slices.Clone(scopes) }

// ClientMetadata is the persisted, validated subset of DCR/CIMD metadata.
type ClientMetadata struct {
	ID        string   `json:"client_id"`
	Name      string   `json:"client_name"`
	Redirects []string `json:"redirect_uris"`
	Auth      string   `json:"token_endpoint_auth_method"`
	Grants    []string `json:"grant_types,omitempty"`
	Responses []string `json:"response_types,omitempty"`
}

func (c *ClientMetadata) GetID() string                       { return c.ID }
func (c *ClientMetadata) RedirectURIs() []string              { return slices.Clone(c.Redirects) }
func (c *ClientMetadata) PostLogoutRedirectURIs() []string    { return nil }
func (c *ClientMetadata) ApplicationType() op.ApplicationType { return op.ApplicationTypeNative }
func (c *ClientMetadata) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodNone }
func (c *ClientMetadata) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *ClientMetadata) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}
func (c *ClientMetadata) LoginURL(id string) string           { return "/oauth/consent?request=" + id }
func (c *ClientMetadata) AccessTokenType() op.AccessTokenType { return op.AccessTokenTypeJWT }
func (c *ClientMetadata) IDTokenLifetime() time.Duration      { return AccessLifetime }
func (c *ClientMetadata) DevMode() bool                       { return false }
func (c *ClientMetadata) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return slices.Clone[[]string]
}
func (c *ClientMetadata) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return slices.Clone[[]string]
}
func (c *ClientMetadata) IsScopeAllowed(scope string) bool {
	return slices.Contains(ResourceScopes, scope)
}
func (c *ClientMetadata) IDTokenUserinfoClaimsAssertion() bool { return false }
func (c *ClientMetadata) ClockSkew() time.Duration             { return 0 }

type signingKey struct {
	private ed25519.PrivateKey
	id      string
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.EdDSA }
func (k *signingKey) Key() any                                    { return k.private }
func (k *signingKey) ID() string                                  { return k.id }

type publicKey struct{ jwk jose.JSONWebKey }

func (k publicKey) ID() string                         { return k.jwk.KeyID }
func (k publicKey) Algorithm() jose.SignatureAlgorithm { return jose.EdDSA }
func (k publicKey) Use() string                        { return "sig" }
func (k publicKey) Key() any                           { return k.jwk.Key }

func resourceScopeSubset(requested, granted []string) bool {
	for _, scope := range requested {
		if !slices.Contains(AllowedScopes, scope) {
			return false
		}
		if strings.HasPrefix(scope, "sandbox:") && !slices.Contains(granted, scope) {
			return false
		}
	}
	return slices.Contains(requested, "sandbox:read")
}
