package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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

// registerAudit mounts the audit log (plan §9.7): a cursor-paginated list, a full chain verification and an
// NDJSON export.
func (h *handlers) registerAudit(a huma.API) {
	h.registerAuditExport(a)
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

// auditExportLine is one line of the NDJSON export: every field the event's hash covers, as they were hashed.
type auditExportLine struct {
	ID         int64           `json:"id"`
	OrgID      string          `json:"org_id"`
	OccurredAt string          `json:"occurred_at" doc:"RFC 3339 in UTC with nanoseconds, exactly as hashed"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id"`
	IP         string          `json:"ip"`
	UserAgent  string          `json:"user_agent"`
	Data       json.RawMessage `json:"data"`
	PrevHash   string          `json:"prev_hash" doc:"hex SHA-256 of the previous event in the chain"`
	Hash       string          `json:"hash"`
}

const auditExportDoc = "One JSON object per line, oldest first. Each line verifies on its own: hash = SHA-256(prev_hash bytes " +
	"followed by the JSON object {org_id, occurred_at, actor_type, actor_id, action, target_type, target_id, request_id, " +
	"ip, user_agent, data} with keys in that order, data's keys sorted, no whitespace). The first event's prev_hash " +
	"is SHA-256(\"headmesh-audit-genesis:\" + org_id). `headmesh audit verify --file` does all of this offline."

func toAuditExportLine(e *service.AuditExportEntry) auditExportLine {
	return auditExportLine{
		ID: e.ID, OrgID: e.OrgID, OccurredAt: e.OccurredAt.UTC().Format(time.RFC3339Nano), ActorType: e.ActorType,
		ActorID: e.ActorID, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID, RequestID: e.RequestID,
		IP: e.IP, UserAgent: e.UserAgent, Data: e.Data, PrevHash: e.PrevHash, Hash: e.Hash,
	}
}

// registerAuditExport streams the log as NDJSON for archives and SIEMs (plan §9.7).
func (h *handlers) registerAuditExport(a huma.API) {
	type exportIn struct {
		After int64 `query:"after" minimum:"0" doc:"start after this id (0 for the whole log, which also lets the genesis link be checked)"`
	}
	lineSchema := a.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(auditExportLine{}), true, "")
	huma.Register(a, huma.Operation{
		OperationID: "export-audit-events", Method: http.MethodGet, Path: "/v1/audit-events/export",
		Summary: "Download your org's audit log as NDJSON, verifiable offline", Description: auditExportDoc,
		Tags: []string{"audit"}, Security: bearer,
		Responses: map[string]*huma.Response{
			"200": {Description: "NDJSON, one event per line", Content: map[string]*huma.MediaType{
				"application/x-ndjson": {Schema: lineSchema},
			}},
		},
	}, func(ctx context.Context, in *exportIn) (*huma.StreamResponse, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		// Refuse before the 200 goes out; once streaming starts, an error can only end the stream.
		if !p.Can(service.ScopeAuditRead) {
			return nil, h.fail(service.ErrForbidden)
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Content-Disposition", `attachment; filename="headmesh-audit.ndjson"`)
			hctx.SetStatus(http.StatusOK)
			w := hctx.BodyWriter()
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			flusher, _ := w.(http.Flusher)
			n := 0
			err := h.svc.ExportAuditEvents(ctx, p, in.After, func(e *service.AuditExportEntry) error {
				if err := enc.Encode(toAuditExportLine(e)); err != nil {
					return fmt.Errorf("write export line: %w", err)
				}
				if n++; flusher != nil && n%100 == 0 {
					flusher.Flush()
				}
				return nil
			})
			if err != nil {
				h.log.Error("audit export stopped early", "error", err, "lines", n)
			}
		}}, nil
	})
}

func toAuditEventBody(e *service.AuditEntry) auditEventBody {
	return auditEventBody{
		ID: e.ID, OccurredAt: e.OccurredAt, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, RequestID: e.RequestID, IP: e.IP, Data: e.Data, Hash: e.Hash,
	}
}
