// Package api is Headmesh's public REST API (OpenAPI 3.1, RFC 9457 errors), a thin layer over package service.
//
// Every operation either declares the "bearer" security requirement — enforced by middleware, which turns the
// API key into a service.Principal — or is on the short public allowlist (device enrollment, which carries its own
// one-time credentials). TestEveryOperationIsAuthenticatedOrAllowlisted keeps it that way.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/headmesh-dev/headmesh/internal/service"
	"github.com/headmesh-dev/headmesh/internal/store"
)

// PublicOperations are reachable without an API key.
var PublicOperations = map[string]bool{"enroll": true, "complete-enrollment": true}

type principalKey struct{}

func principalFrom(ctx context.Context) (service.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(service.Principal)
	return p, ok
}

// Options configure the API.
type Options struct {
	Service *service.Service
	Logger  *slog.Logger
	// TrustForwardedFor makes the API take the client address from X-Forwarded-For (set by Caddy). Enable only
	// when every request arrives through the trusted proxy.
	TrustForwardedFor bool
	// EnrollPerMinute limits enrollment calls per client address (default 30).
	EnrollPerMinute int
	Version         string
}

// New registers every operation on router and returns the Huma API (for the OpenAPI document).
func New(router chi.Router, opt Options) huma.API {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.EnrollPerMinute == 0 {
		opt.EnrollPerMinute = 30
	}
	cfg := huma.DefaultConfig("Headmesh API", opt.Version)
	cfg.Info.Description = "Multi-tenant control layer for Headscale meshes."
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {Type: "http", Scheme: "bearer", Description: "API key (hmk_…)"},
	}
	a := humachi.New(router, cfg)
	h := &handlers{
		svc: opt.Service, log: opt.Logger, trustXFF: opt.TrustForwardedFor,
		limiter: newLimiter(opt.EnrollPerMinute, time.Minute),
	}
	a.UseMiddleware(h.authenticate(a))
	h.register(a)
	return a
}

type handlers struct {
	svc      *service.Service
	log      *slog.Logger
	trustXFF bool
	limiter  *limiter
}

var bearer = []map[string][]string{{"bearer": {}}}

func (h *handlers) authenticate(a huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		if op == nil || len(op.Security) == 0 {
			next(ctx)
			return
		}
		raw, ok := strings.CutPrefix(ctx.Header("Authorization"), "Bearer ")
		if !ok {
			_ = huma.WriteErr(a, ctx, http.StatusUnauthorized, "missing bearer API key")
			return
		}
		p, err := h.svc.Authenticate(ctx.Context(), strings.TrimSpace(raw))
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, service.ErrForbidden) {
				status = http.StatusForbidden
			} else if !errors.Is(err, service.ErrUnauthorized) {
				h.log.Error("authenticate", "error", err)
				status = http.StatusInternalServerError
			}
			_ = huma.WriteErr(a, ctx, status, http.StatusText(status))
			return
		}
		next(huma.WithValue(ctx, principalKey{}, p))
	}
}

// fail maps service errors to HTTP errors without leaking internals.
func (h *handlers) fail(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalid):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, service.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, service.ErrUnauthorized), errors.Is(err, service.ErrTokenRejected):
		return huma.Error401Unauthorized(err.Error())
	case errors.Is(err, service.ErrForbidden):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, service.ErrConflict):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, service.ErrPreconditionFailed):
		return huma.Error412PreconditionFailed(err.Error())
	case errors.Is(err, service.ErrUnavailable):
		h.log.Warn("cell unavailable", "error", err)
		return huma.Error503ServiceUnavailable("the network's control server is temporarily unavailable; retry shortly")
	default:
		h.log.Error("request failed", "error", err)
		return huma.Error500InternalServerError("internal error")
	}
}

func (h *handlers) principal(ctx context.Context) (service.Principal, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return service.Principal{}, huma.Error401Unauthorized("missing credentials")
	}
	return p, nil
}

// clientIP returns the caller's address, honouring X-Forwarded-For only when configured.
func (h *handlers) clientIP(ctx huma.Context) *netip.Addr {
	raw := ctx.RemoteAddr()
	if h.trustXFF {
		if xff := ctx.Header("X-Forwarded-For"); xff != "" {
			raw = strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return nil
	}
	return &addr
}

// limiter is a fixed-window per-key rate limiter for unauthenticated endpoints.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	counts map[string]int
	reset  time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, counts: map[string]int{}, reset: time.Now().Add(window)}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := time.Now(); now.After(l.reset) {
		l.counts, l.reset = map[string]int{}, now.Add(l.window)
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}

// Health mounts /healthz (process up) and /readyz (database reachable and not recovering) on router.
func Health(router chi.Router, st *store.Store, rec *Recovery) {
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if rec.Active() {
			w.Header().Set("Retry-After", "15")
			http.Error(w, "recovering", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Pool().Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
