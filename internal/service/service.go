package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/internal/store"
)

// Hub is the part of the cell hub services use.
type Hub interface {
	Refresh(ctx context.Context, cell string) error
	RefreshAndWait(ctx context.Context, cell string) error
	Do(ctx context.Context, cell string, op *cellv1.Op) (*cellv1.OpResult, error)
}

// Errors returned to callers; the API maps them to HTTP status codes.
var (
	ErrNotFound      = errors.New("not found")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrInvalid       = errors.New("invalid request")
	ErrConflict      = errors.New("conflict")
	ErrUnavailable   = errors.New("cell unavailable")
	ErrTokenRejected = errors.New("enrollment token is invalid, expired, revoked or used up")
)

// Principal is an authenticated API caller acting for one org.
type Principal struct {
	KeyID    pgtype.UUID
	OrgID    pgtype.UUID
	OrgShort string
	Scopes   []string
}

// Scopes.
const (
	ScopeAdmin        = "admin"
	ScopeDevicesRead  = "devices:read"
	ScopeEnrollWrite  = "enroll:write"
	ScopeGrantsWrite  = "grants:write"
	ScopeGrantsRead   = "grants:read"
	ScopeAPIKeysWrite = "apikeys:write"
	ScopeAuditRead    = "audit:read"
)

// Can reports whether the principal holds scope (admin implies every scope).
func (p Principal) Can(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}

func (p Principal) scope() store.Scope { return store.Scope{OrgIDs: []pgtype.UUID{p.OrgID}} }

// Service bundles the use cases.
type Service struct {
	Store *store.Store
	Hub   Hub
	Now   func() time.Time
	// MaxGrantTTL caps grant lifetimes unless an org setting is lower (default 30 days).
	MaxGrantTTL time.Duration
	// PublicURL is the API's external base URL, used in join links (for example https://api.headmesh.dev).
	PublicURL string
	// InstallURL is where the install script is served (for example https://get.headmesh.dev).
	InstallURL string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var systemScope = store.Scope{System: true}
