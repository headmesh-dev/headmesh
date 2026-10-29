package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

var knownScopes = []string{ScopeAdmin, ScopeDevicesRead, ScopeEnrollWrite, ScopeGrantsRead, ScopeGrantsWrite, ScopeAPIKeysWrite, ScopeAuditRead}

// Authenticate resolves an API key to a principal. Lookup runs in the system scope because the key is what
// establishes the tenant scope; nothing else is read there.
func (s *Service) Authenticate(ctx context.Context, rawKey string) (Principal, error) {
	if !strings.HasPrefix(rawKey, PrefixAPIKey) {
		return Principal{}, ErrUnauthorized
	}
	var p Principal
	err := s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		k, err := q.GetAPIKeyByHash(ctx, hashSecret(rawKey))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthorized
		} else if err != nil {
			return fmt.Errorf("look up api key: %w", err)
		}
		org, err := q.GetOrg(ctx, k.OrgID)
		if err != nil {
			return fmt.Errorf("load org: %w", err)
		}
		if org.Status != "active" {
			return ErrForbidden
		}
		if err := q.TouchAPIKey(ctx, k.ID); err != nil {
			return fmt.Errorf("touch api key: %w", err)
		}
		p = Principal{KeyID: k.ID, OrgID: k.OrgID, OrgShort: org.ShortID, Scopes: k.Scopes}
		return nil
	})
	return p, err
}

// APIKey is returned once, at creation.
type APIKey struct {
	ID        string
	Key       string // only set on creation
	Prefix    string
	Name      string
	Scopes    []string
	ExpiresAt *time.Time
}

// CreateAPIKey issues a key for the principal's org. A key can never grant scopes its creator lacks.
func (s *Service) CreateAPIKey(ctx context.Context, p Principal, name string, scopes []string, ttl time.Duration) (*APIKey, error) {
	if !p.Can(ScopeAPIKeysWrite) {
		return nil, ErrForbidden
	}
	for _, sc := range scopes {
		if !slices.Contains(knownScopes, sc) {
			return nil, fmt.Errorf("%w: unknown scope %q", ErrInvalid, sc)
		}
		if !p.Can(sc) {
			return nil, fmt.Errorf("%w: cannot grant scope %q you do not hold", ErrForbidden, sc)
		}
	}
	if len(scopes) == 0 || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name and at least one scope are required", ErrInvalid)
	}
	return s.issueKey(ctx, p.scope(), p.OrgID, p.KeyID, name, scopes, ttl)
}

func (s *Service) issueKey(ctx context.Context, scope store.Scope, org, createdBy pgtype.UUID, name string, scopes []string, ttl time.Duration) (*APIKey, error) {
	raw, hash, display, err := newSecret(PrefixAPIKey)
	if err != nil {
		return nil, err
	}
	var expires *time.Time
	if ttl > 0 {
		t := s.now().Add(ttl)
		expires = &t
	}
	var k dbq.ApiKey
	err = s.Store.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		k, err = q.CreateAPIKey(ctx, dbq.CreateAPIKeyParams{
			OrgID: org, Name: name, Prefix: display, KeyHash: hash,
			Scopes: scopes, CreatedBy: createdBy, ExpiresAt: expires,
		})
		if err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: org, ActorType: "api_key", ActorID: uuidString(createdBy),
			Action: "apikey.created", TargetType: "api_key", TargetID: k.ID.String(),
			Data: map[string]any{"name": name, "scopes": scopes, "prefix": display},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &APIKey{ID: k.ID.String(), Key: raw, Prefix: display, Name: name, Scopes: scopes, ExpiresAt: expires}, nil
}
