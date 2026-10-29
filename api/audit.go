package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/headmesh-dev/headmesh/internal/service"
)

type auditEventBody struct {
	ID         int64           `json:"id" doc:"position in the org's log; pass the last one you saw as after"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorType  string          `json:"actor_type" enum:"user,api_key,system,cell,platform"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action" example:"grant.revoked"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id,omitempty"`
	IP         string          `json:"ip,omitempty"`
	Data       json.RawMessage `json:"data"`
	Hash       string          `json:"hash" doc:"hex SHA-256 link in the org's hash chain"`
}

// registerAudit mounts the audit log (plan §9.7): a cursor-paginated list and a full chain verification.
func (h *handlers) registerAudit(a huma.API) {
	type listIn struct {
		After int64 `query:"after" minimum:"0" doc:"return events after this id (0 for the start)"`
		Limit int32 `query:"limit" minimum:"1" maximum:"500" default:"100"`
	}
	type listOut struct {
		Body struct {
			Events    []auditEventBody `json:"events"`
			NextAfter int64            `json:"next_after" doc:"pass as after for the next page; equals after when there are no more"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "list-audit-events", Method: http.MethodGet, Path: "/v1/audit-events",
		Summary: "Your org's audit log, oldest first", Tags: []string{"audit"}, Security: bearer,
	}, func(ctx context.Context, in *listIn) (*listOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		events, err := h.svc.ListAuditEvents(ctx, p, in.After, in.Limit)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &listOut{}
		out.Body.Events = make([]auditEventBody, 0, len(events))
		out.Body.NextAfter = in.After
		for _, e := range events {
			out.Body.Events = append(out.Body.Events, toAuditEventBody(e))
			out.Body.NextAfter = e.ID
		}
		return out, nil
	})

	type verifyOut struct {
		Body struct {
			Verified bool   `json:"verified"`
			Checked  int    `json:"checked" doc:"events verified before the first problem (all of them when verified)"`
			Problem  string `json:"problem,omitempty" doc:"the first event that failed verification"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "verify-audit-events", Method: http.MethodGet, Path: "/v1/audit-events/verify",
		Summary: "Recompute your org's audit hash chain and report whether it is intact", Tags: []string{"audit"},
		Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*verifyOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		res, err := h.svc.VerifyAudit(ctx, p)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &verifyOut{}
		out.Body.Verified, out.Body.Checked, out.Body.Problem = res.Verified, res.Checked, res.Problem
		return out, nil
	})
}

func toAuditEventBody(e *service.AuditEntry) auditEventBody {
	return auditEventBody{
		ID: e.ID, OccurredAt: e.OccurredAt, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, RequestID: e.RequestID, IP: e.IP, Data: e.Data, Hash: e.Hash,
	}
}
