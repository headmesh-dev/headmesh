package authn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Caller is the authenticated identity of a request.
type Caller struct {
	Client string   `json:"client"`
	Route  string   `json:"route"`
	Scopes []string `json:"scopes"`
}

// Has reports whether the caller holds scope (sandbox:admin implies all).
func (c Caller) Has(scope string) bool {
	return slices.Contains(c.Scopes, scope) || slices.Contains(c.Scopes, ScopeAdmin)
}

// Tunnel reports whether the call came through the OpenAI tunnel route.
func (c Caller) Tunnel() bool { return c.Route == RouteTunnel }

// FromTokenInfo turns the SDK's TokenInfo into a Caller.
func FromTokenInfo(ti *auth.TokenInfo) Caller {
	if ti == nil {
		return Caller{Client: "anonymous"}
	}
	route, _ := ti.Extra["route"].(string)
	return Caller{Client: ti.UserID, Route: route, Scopes: slices.Clone(ti.Scopes)}
}

type unixConnKey struct{}

// UnixConnContext marks connections accepted on the unix socket. Use it as
// http.Server.ConnContext for the unix listener only.
func UnixConnContext(ctx context.Context, c net.Conn) context.Context {
	uid := -1
	if uc, ok := c.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				if cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
					uid = int(cred.Uid)
				}
			})
		}
	}
	return context.WithValue(ctx, unixConnKey{}, uid)
}

func unixPeer(ctx context.Context) (int, bool) {
	uid, ok := ctx.Value(unixConnKey{}).(int)
	return uid, ok
}

// unixBearer is what the unix listener injects; it only authenticates on
// connections marked by UnixConnContext.
const unixBearer = "unix-socket"

// InjectUnixBearer adds the placeholder bearer so unix-socket requests pass
// through the same RequireBearerToken middleware as TCP ones.
func InjectUnixBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+unixBearer)
		next.ServeHTTP(w, r)
	})
}

// ResourceServer validates OAuth 2.1 access tokens (M3). The Headmesh
// authorization server issues EdDSA JWTs whose audience is this sandbox's
// resource URL (RFC 8707).
type ResourceServer interface {
	VerifyAccessToken(ctx context.Context, token string) (*auth.TokenInfo, error)
	Metadata() *oauthex.ProtectedResourceMetadata
}

// ErrOAuthNotImplemented is returned by the M1 stub.
var ErrOAuthNotImplemented = errors.New("oauth resource server is not implemented until M3")

// StubResourceServer publishes RFC 9728 metadata but rejects every access
// token. It lets clients discover the authorization server today without
// granting access through it.
type StubResourceServer struct {
	Resource             string
	AuthorizationServers []string
	JWKSURL              string
}

// VerifyAccessToken always fails.
func (s StubResourceServer) VerifyAccessToken(context.Context, string) (*auth.TokenInfo, error) {
	return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, ErrOAuthNotImplemented)
}

// Metadata describes this resource per RFC 9728.
func (s StubResourceServer) Metadata() *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               s.Resource,
		AuthorizationServers:   s.AuthorizationServers,
		JWKSURI:                s.JWKSURL,
		ScopesSupported:        AllScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Headmesh agent sandbox",
	}
}

// Verifier builds the TokenVerifier for auth.RequireBearerToken: unix socket
// first, then hms_ bearer tokens, then the OAuth resource server.
func Verifier(store *Store, rs ResourceServer) auth.TokenVerifier {
	return func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		if uid, ok := unixPeer(ctx); ok {
			if token != unixBearer {
				return nil, auth.ErrInvalidToken
			}
			return &auth.TokenInfo{
				UserID: fmt.Sprintf("unix:%d", uid),
				Scopes: slices.Clone(AllScopes),
				Extra:  map[string]any{"route": RouteUnix},
			}, nil
		}
		if strings.HasPrefix(token, TokenPrefix) {
			rec, err := store.Verify(token)
			if err != nil {
				return nil, fmt.Errorf("%w: unknown or revoked token", auth.ErrInvalidToken)
			}
			return &auth.TokenInfo{
				UserID: rec.Name,
				Scopes: slices.Clone(rec.Scopes),
				Extra:  map[string]any{"route": rec.Route, "token_id": rec.ID},
			}, nil
		}
		if rs != nil {
			ti, err := rs.VerifyAccessToken(ctx, token)
			if err != nil {
				return nil, err
			}
			if ti.Extra == nil {
				ti.Extra = map[string]any{}
			}
			ti.Extra["route"] = RouteMCP
			return ti, nil
		}
		return nil, auth.ErrInvalidToken
	}
}

// Middleware wraps h with bearer authentication. 401 responses carry a
// WWW-Authenticate challenge with resource_metadata and the supported scopes.
func Middleware(store *Store, rs ResourceServer, resourceMetadataURL string, h http.Handler) http.Handler {
	inner := auth.RequireBearerToken(Verifier(store, rs), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL:    resourceMetadataURL,
		AllowMissingExpiration: true,
		ClockSkew:              time.Minute,
	})(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(&challengeWriter{ResponseWriter: w}, r)
	})
}

// challengeWriter appends scope="..." to the SDK's Bearer challenge.
type challengeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (c *challengeWriter) WriteHeader(code int) {
	if !c.wrote && code == http.StatusUnauthorized {
		h := c.Header()
		ch := h.Get("WWW-Authenticate")
		if ch == "" {
			ch = "Bearer"
		}
		if !strings.Contains(ch, "scope=") {
			sep := ", "
			if ch == "Bearer" {
				sep = " "
			}
			ch += sep + fmt.Sprintf("scope=%q", strings.Join(AllScopes, " "))
		}
		h.Set("WWW-Authenticate", ch)
	}
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *challengeWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b) //nolint:wrapcheck // pass-through writer
}

// Flush keeps streaming (SSE) responses working through the wrapper.
func (c *challengeWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *challengeWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// MetadataHandler serves RFC 9728 protected resource metadata.
func MetadataHandler(rs ResourceServer) http.Handler {
	return auth.ProtectedResourceMetadataHandler(rs.Metadata())
}
