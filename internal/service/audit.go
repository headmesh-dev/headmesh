package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// AuditEntry is one event of an org's tamper-evident audit log (plan §9.7).
type AuditEntry struct {
	ID         int64
	OccurredAt time.Time
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	RequestID  string
	IP         string
	Data       json.RawMessage
	// Hash is this event's link in the org's hash chain, hex-encoded.
	Hash string
}

// AuditVerification is the result of recomputing an org's audit chain.
type AuditVerification struct {
	// Verified is true when every event links to its predecessor, matches its hash, and the chain head points at the
	// last event.
	Verified bool
	// Checked is how many events were verified before the first problem (all of them when Verified).
	Checked int
	// Problem names the first event that failed, when Verified is false.
	Problem string
}

// ListAuditEvents returns up to limit of the caller's org's audit events with an id greater than after, oldest first,
// so a client pages through the log by passing the last id it saw.
func (s *Service) ListAuditEvents(ctx context.Context, p Principal, after int64, limit int32) ([]*AuditEntry, error) {
	if !p.Can(ScopeAuditRead) {
		return nil, ErrForbidden
	}
	if after < 0 || limit < 1 || limit > 500 {
		return nil, fmt.Errorf("%w: after must be ≥ 0 and limit between 1 and 500", ErrInvalid)
	}
	var out []*AuditEntry
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		events, err := q.ListAuditEvents(ctx, dbq.ListAuditEventsParams{OrgID: p.OrgID, ID: after, Limit: limit})
		if err != nil {
			return fmt.Errorf("list audit events: %w", err)
		}
		out = make([]*AuditEntry, 0, len(events))
		for _, e := range events {
			out = append(out, toAuditEntry(e))
		}
		return nil
	})
	return out, err
}

// VerifyAudit recomputes the caller's org's hash chain from its first event and checks that it ends at the stored
// chain head. A broken chain is a result, not an error: Verified is false and Problem says where it broke.
func (s *Service) VerifyAudit(ctx context.Context, p Principal) (*AuditVerification, error) {
	if !p.Can(ScopeAuditRead) {
		return nil, ErrForbidden
	}
	var res AuditVerification
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		n, err := store.VerifyAuditChain(ctx, q, p.OrgID)
		res.Checked = n
		switch {
		case err == nil:
			res.Verified = true
			return nil
		case errors.Is(err, store.ErrChainBroken):
			res.Problem = err.Error()
			return nil
		default:
			return fmt.Errorf("verify audit chain: %w", err)
		}
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func toAuditEntry(e dbq.AuditEvent) *AuditEntry {
	entry := &AuditEntry{
		ID: e.ID, OccurredAt: e.OccurredAt, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, RequestID: derefOr(e.RequestID, ""), Data: json.RawMessage(e.Data),
		Hash: hex.EncodeToString(e.Hash),
	}
	if e.Ip != nil {
		entry.IP = e.Ip.String()
	}
	if len(entry.Data) == 0 {
		entry.Data = json.RawMessage("{}")
	}
	return entry
}
