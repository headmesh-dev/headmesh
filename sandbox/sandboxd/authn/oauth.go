package authn

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// OAuthResourceServer validates only Headmesh access JWTs, then checks their
// current grant/family status. The status check is deliberate: JWT expiry alone
// would leave revoked access usable for up to fifteen minutes.
type OAuthResourceServer struct {
	issuer, resource, jwksURL string
	client                    *http.Client
	mu                        sync.Mutex
	keys                      map[string]ed25519.PublicKey
	fetched                   time.Time
	attempted                 time.Time
}

func NewOAuthResourceServer(issuer, resource, jwksURL string) (*OAuthResourceServer, error) {
	iss, err := url.Parse(issuer)
	if err != nil || iss.Scheme != "https" || iss.Host == "" || iss.User != nil || iss.RawQuery != "" || iss.Fragment != "" || strings.TrimRight(issuer, "/") != issuer {
		return nil, errors.New("OAuth issuer must be an exact HTTPS URL without a trailing slash")
	}
	keys, err := url.Parse(jwksURL)
	if err != nil || keys.Scheme != "https" || keys.Host != iss.Host || keys.User != nil || keys.RawQuery != "" || keys.Fragment != "" {
		return nil, errors.New("OAuth JWKS must be on the configured issuer's HTTPS origin")
	}
	res, err := url.Parse(resource)
	if err != nil || res.Scheme != "https" || res.Host == "" || res.User != nil || res.Fragment != "" {
		return nil, errors.New("OAuth resource must be an exact HTTPS URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &OAuthResourceServer{issuer: issuer, resource: resource, jwksURL: jwksURL,
		client: &http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		keys:   make(map[string]ed25519.PublicKey)}, nil
}

func (s *OAuthResourceServer) Metadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource: s.resource, AuthorizationServers: []string{s.issuer}, JWKSURI: s.jwksURL,
		ScopesSupported: []string{ScopeRead, ScopeExec, ScopeWrite}, BearerMethodsSupported: []string{"header"},
		ResourceName: "Headmesh agent sandbox",
	}
}

type accessClaims struct {
	jwt.Claims
	Use   string `json:"token_use"`
	Scope string `json:"scope"`
}

func (s *OAuthResourceServer) VerifyAccessToken(ctx context.Context, token string) (*auth.TokenInfo, error) {
	if len(token) > 16<<10 {
		return nil, auth.ErrInvalidToken
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(parsed.Headers) != 1 || parsed.Headers[0].KeyID == "" {
		return nil, auth.ErrInvalidToken
	}
	key, err := s.key(ctx, parsed.Headers[0].KeyID)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	var claims accessClaims
	if err = parsed.Claims(key, &claims); err != nil {
		return nil, auth.ErrInvalidToken
	}
	now := time.Now()
	if claims.ValidateWithLeeway(jwt.Expected{Issuer: s.issuer, Time: now}, 0) != nil || claims.Use != "access" || claims.Subject == "" || claims.ID == "" || len(claims.Audience) != 1 || claims.Audience[0] != s.resource || claims.Expiry == nil || claims.IssuedAt == nil || claims.IssuedAt.Time().After(now.Add(30*time.Second)) || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) > 15*time.Minute {
		return nil, auth.ErrInvalidToken
	}
	scopes := strings.Fields(claims.Scope)
	if !slices.Contains(scopes, ScopeRead) {
		return nil, auth.ErrInvalidToken
	}
	for _, scope := range scopes {
		if !slices.Contains([]string{ScopeRead, ScopeExec, ScopeWrite, "openid", "profile", "email", "offline_access"}, scope) {
			return nil, auth.ErrInvalidToken
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.issuer+"/oauth/token-status", nil)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, auth.ErrInvalidToken
	}
	var status struct {
		Active bool   `json:"active"`
		User   string `json:"sub"`
		ID     string `json:"jti"`
		Aud    string `json:"aud"`
		Scope  string `json:"scope"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&status) != nil || !status.Active || status.User != claims.Subject || status.ID != claims.ID || status.Aud != s.resource || status.Scope != claims.Scope {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: claims.Subject, Scopes: scopes, Expiration: claims.Expiry.Time(), Extra: map[string]any{"token_id": claims.ID}}, nil
}

func (s *OAuthResourceServer) key(ctx context.Context, id string) (ed25519.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[id]
	if ok && time.Since(s.fetched) < 5*time.Minute {
		return slices.Clone(key), nil
	}
	// Unknown key IDs must not turn arbitrary unauthenticated tokens into an
	// unlimited request stream to the issuer. A rotated key can refresh shortly.
	if time.Since(s.attempted) < 2*time.Second {
		return nil, auth.ErrInvalidToken
	}
	s.attempted = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.jwksURL, nil)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, auth.ErrInvalidToken
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, auth.ErrInvalidToken
	}
	var set jose.JSONWebKeySet
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&set); err != nil || len(set.Keys) > 20 {
		return nil, auth.ErrInvalidToken
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, jwk := range set.Keys {
		public, ok := jwk.Key.(ed25519.PublicKey)
		if !ok || jwk.KeyID == "" || jwk.Algorithm != "EdDSA" || jwk.Use != "sig" || len(public) != ed25519.PublicKeySize {
			return nil, auth.ErrInvalidToken
		}
		if _, duplicate := keys[jwk.KeyID]; duplicate {
			return nil, auth.ErrInvalidToken
		}
		keys[jwk.KeyID] = slices.Clone(public)
	}
	s.keys, s.fetched = keys, time.Now()
	key, ok = keys[id]
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return slices.Clone(key), nil
}
