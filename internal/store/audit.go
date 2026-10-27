package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// AuditEvent is one entry in the tamper-evident audit log.
type AuditEvent struct {
	OrgID      pgtype.UUID // zero (invalid) for events that belong to no tenant
	OccurredAt time.Time
	ActorType  string // user | api_key | system | cell | platform
	ActorID    string
	Action     string // dotted, e.g. "grant.revoked"
	TargetType string
	TargetID   string
	RequestID  string
	IP         *netip.Addr
	UserAgent  string
	Data       map[string]any
}

// ErrChainBroken reports an audit chain whose stored hashes do not match the recomputed ones.
var ErrChainBroken = errors.New("audit chain broken")

// chainKey is the org id, or the zero UUID for tenant-less events.
func chainKey(org pgtype.UUID) pgtype.UUID {
	if org.Valid {
		return org
	}
	return pgtype.UUID{Valid: true}
}

// genesis is the prev_hash of the first event in a chain.
func genesis(key pgtype.UUID) []byte {
	sum := sha256.Sum256([]byte("headmesh-audit-genesis:" + key.String()))
	return sum[:]
}

// canonicalEvent is the exact structure hashed into the chain. Field order is fixed by the struct, and Data is
// re-encoded with sorted keys, so the hash does not depend on how Postgres stores jsonb. Changing this
// structure is a breaking change to every existing chain.
type canonicalEvent struct {
	OrgID      string          `json:"org_id"`
	OccurredAt string          `json:"occurred_at"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id"`
	IP         string          `json:"ip"`
	UserAgent  string          `json:"user_agent"`
	Data       json.RawMessage `json:"data"`
}

// canonicalData re-encodes a JSON object with sorted keys, preserving numbers exactly.
func canonicalData(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("audit data: %w", err)
	}
	out, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		return nil, fmt.Errorf("audit data: %w", err)
	}
	return out, nil
}

func eventHash(prev []byte, org pgtype.UUID, at time.Time, actorType, actorID, action, targetType, targetID, requestID string,
	ip *netip.Addr, userAgent string, data []byte,
) ([]byte, error) {
	cd, err := canonicalData(data)
	if err != nil {
		return nil, err
	}
	ce := canonicalEvent{
		OccurredAt: at.UTC().Format(time.RFC3339Nano), ActorType: actorType, ActorID: actorID, Action: action,
		TargetType: targetType, TargetID: targetID, RequestID: requestID, UserAgent: userAgent, Data: cd,
	}
	if org.Valid {
		ce.OrgID = org.String()
	}
	if ip != nil {
		ce.IP = ip.String()
	}
	body, err := json.Marshal(ce)
	if err != nil {
		return nil, fmt.Errorf("encode audit event: %w", err)
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(body)
	return h.Sum(nil), nil
}

// AppendAudit adds ev to its org's chain inside the caller's transaction. The chain head row is locked for the
// rest of the transaction, so concurrent appends to the same org serialise and the chain never forks.
func AppendAudit(ctx context.Context, q *dbq.Queries, ev AuditEvent) (int64, error) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}
	// Postgres stores microseconds; hash what will be stored.
	ev.OccurredAt = ev.OccurredAt.UTC().Truncate(time.Microsecond)
	data := ev.Data
	if data == nil {
		data = map[string]any{}
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("encode audit data: %w", err)
	}
	key := chainKey(ev.OrgID)
	if err := q.InitChainHead(ctx, dbq.InitChainHeadParams{ChainKey: key, HeadHash: genesis(key)}); err != nil {
		return 0, fmt.Errorf("init audit chain: %w", err)
	}
	head, err := q.LockChainHead(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("lock audit chain: %w", err)
	}
	hash, err := eventHash(head.HeadHash, ev.OrgID, ev.OccurredAt, ev.ActorType, ev.ActorID, ev.Action, ev.TargetType,
		ev.TargetID, ev.RequestID, ev.IP, ev.UserAgent, rawData)
	if err != nil {
		return 0, err
	}
	id, err := q.InsertAuditEvent(ctx, dbq.InsertAuditEventParams{
		OrgID: ev.OrgID, OccurredAt: ev.OccurredAt, ActorType: ev.ActorType, ActorID: ev.ActorID, Action: ev.Action,
		TargetType: ev.TargetType, TargetID: ev.TargetID, RequestID: nilIfEmpty(ev.RequestID), Ip: ev.IP,
		UserAgent: nilIfEmpty(ev.UserAgent), Data: rawData, PrevHash: head.HeadHash, Hash: hash,
	})
	if err != nil {
		return 0, fmt.Errorf("insert audit event: %w", err)
	}
	if err := q.AdvanceChainHead(ctx, dbq.AdvanceChainHeadParams{ChainKey: key, HeadHash: hash, LastID: id}); err != nil {
		return 0, fmt.Errorf("advance audit chain: %w", err)
	}
	return id, nil
}

// VerifyAuditChain recomputes org's chain from genesis and checks it ends at the stored head. It returns the
// number of events checked, or ErrChainBroken naming the first event that does not verify.
func VerifyAuditChain(ctx context.Context, q *dbq.Queries, org pgtype.UUID) (int, error) {
	key := chainKey(org)
	prev := genesis(key)
	var after int64
	checked := 0
	for {
		page, err := q.ListAuditEventsForChain(ctx, dbq.ListAuditEventsForChainParams{OrgID: org, ID: after, Limit: 500})
		if err != nil {
			return checked, fmt.Errorf("load audit events: %w", err)
		}
		for _, e := range page {
			if !bytes.Equal(e.PrevHash, prev) {
				return checked, fmt.Errorf("%w: event %d does not link to its predecessor", ErrChainBroken, e.ID)
			}
			want, err := eventHash(prev, e.OrgID, e.OccurredAt, e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID,
				deref(e.RequestID), e.Ip, deref(e.UserAgent), e.Data)
			if err != nil {
				return checked, err
			}
			if !bytes.Equal(want, e.Hash) {
				return checked, fmt.Errorf("%w: event %d content does not match its hash", ErrChainBroken, e.ID)
			}
			prev, after = e.Hash, e.ID
			checked++
		}
		if len(page) < 500 {
			break
		}
	}
	if checked == 0 {
		return 0, nil
	}
	head, err := q.LockChainHead(ctx, key)
	if err != nil {
		return checked, fmt.Errorf("load audit chain head: %w", err)
	}
	if !bytes.Equal(head.HeadHash, prev) || head.LastID != after {
		return checked, fmt.Errorf("%w: chain head does not match the last event (events removed or reordered)", ErrChainBroken)
	}
	return checked, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
