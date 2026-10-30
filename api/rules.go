package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/headmesh-dev/headmesh/internal/service"
	"github.com/headmesh-dev/headmesh/internal/store"
)

type ruleBody struct {
	ID        string             `json:"id"`
	Src       store.SelectorJSON `json:"src"`
	Dst       store.SelectorJSON `json:"dst"`
	Ports     []string           `json:"ports"`
	StartsAt  *time.Time         `json:"starts_at,omitempty"`
	ExpiresAt *time.Time         `json:"expires_at,omitempty"`
	Version   int32              `json:"version"`
	CreatedAt time.Time          `json:"created_at"`
	Enforced  bool               `json:"enforced,omitempty" doc:"on create and delete: true when the cell confirmed the resulting policy is live"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ruleETag is a rule's version as a strong ETag.
func ruleETag(r *service.Rule) string { return `"` + strconv.Itoa(int(r.Version)) + `"` }

// parseIfMatch reads an If-Match header naming one rule version. Empty and * match any version (0).
func parseIfMatch(v string) (int32, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "*" {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(v, "W/"), `"`), 10, 32)
	if err != nil || n < 1 {
		return 0, huma.Error412PreconditionFailed("If-Match must be one ETag returned for this rule, for example \"3\"")
	}
	return int32(n), nil
}

func toRuleBody(r *service.Rule) ruleBody {
	return ruleBody{
		ID: r.ID, Src: r.Src, Dst: r.Dst, Ports: r.Ports, StartsAt: r.StartsAt, ExpiresAt: r.ExpiresAt,
		Version: r.Version, CreatedAt: r.CreatedAt, Enforced: r.Enforced,
	}
}

// registerRules: access inside the caller's org (plan §9.8). Access to another org's devices is a grant.
func (h *handlers) registerRules(a huma.API) {
	type ruleIn struct {
		Body struct {
			Src       store.SelectorJSON `json:"src" doc:"devices that may connect: {kind: all|users|role|device}"`
			Dst       store.SelectorJSON `json:"dst" doc:"devices they may reach, in the same form"`
			Ports     []string           `json:"ports" minItems:"1" maxItems:"32" example:"[\"tcp:22\"]" doc:"proto:port, proto:lo-hi, or * for every port"`
			StartsAt  *time.Time         `json:"starts_at,omitempty"`
			ExpiresAt *time.Time         `json:"expires_at,omitempty" doc:"omit for a standing rule"`
		}
	}
	type ruleOut struct {
		ETag string `header:"ETag" doc:"the rule's version; send it as If-Match to update"`
		Body ruleBody
	}
	huma.Register(a, huma.Operation{
		OperationID: "create-rule", Method: http.MethodPost, Path: "/v1/rules",
		Summary: "Let devices in your org reach other devices in your org", Tags: []string{"rules"},
		Security: bearer, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *ruleIn) (*ruleOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.CreateRule(ctx, p, service.RuleRequest{
			Src: in.Body.Src, Dst: in.Body.Dst, Ports: in.Body.Ports, StartsAt: in.Body.StartsAt, ExpiresAt: in.Body.ExpiresAt,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		return &ruleOut{ETag: ruleETag(r), Body: toRuleBody(r)}, nil
	})

	type rulePairBody struct {
		SrcID   string   `json:"src_id"`
		SrcName string   `json:"src_name"`
		DstID   string   `json:"dst_id"`
		DstName string   `json:"dst_name"`
		New     []string `json:"new" doc:"the rule's ports this pair can't reach yet"`
		Already []string `json:"already" doc:"the rule's ports something else already allows"`
	}
	type previewOut struct {
		Body struct {
			At        time.Time      `json:"at" doc:"the instant previewed: now, or starts_at when later"`
			NewPairs  int            `json:"new_pairs" doc:"device pairs that would gain at least one port"`
			Pairs     []rulePairBody `json:"pairs"`
			Truncated bool           `json:"truncated" doc:"more than 500 pairs matched; new_pairs still counts them all"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "preview-rule", Method: http.MethodPost, Path: "/v1/rules/preview",
		Summary: "What a rule would change, without creating it",
		Description: "Validates the rule exactly as create does and compiles the cell's policy with it, then lists every " +
			"pair of your devices it would let through and which of its ports are new for each pair.",
		Tags: []string{"rules"}, Security: bearer,
	}, func(ctx context.Context, in *ruleIn) (*previewOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		pv, err := h.svc.PreviewRule(ctx, p, service.RuleRequest{
			Src: in.Body.Src, Dst: in.Body.Dst, Ports: in.Body.Ports, StartsAt: in.Body.StartsAt, ExpiresAt: in.Body.ExpiresAt,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		out := &previewOut{}
		out.Body.At, out.Body.NewPairs, out.Body.Truncated = pv.At, pv.NewPairs, pv.Truncated
		out.Body.Pairs = make([]rulePairBody, 0, len(pv.Pairs))
		for _, pr := range pv.Pairs {
			out.Body.Pairs = append(out.Body.Pairs, rulePairBody{
				SrcID: pr.SrcID, SrcName: pr.SrcName, DstID: pr.DstID, DstName: pr.DstName,
				New: nonNil(pr.New), Already: nonNil(pr.Already),
			})
		}
		return out, nil
	})

	type rulesOut struct {
		Body struct {
			Rules []ruleBody `json:"rules"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "list-rules", Method: http.MethodGet, Path: "/v1/rules", Summary: "Your org's rules, oldest first",
		Tags: []string{"rules"}, Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*rulesOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		rs, err := h.svc.ListRules(ctx, p)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &rulesOut{}
		out.Body.Rules = make([]ruleBody, 0, len(rs))
		for _, r := range rs {
			out.Body.Rules = append(out.Body.Rules, toRuleBody(r))
		}
		return out, nil
	})

	type ruleIDIn struct {
		ID string `path:"id" format:"uuid"`
	}
	type ruleDeletedOut struct{ Body ruleBody }
	huma.Register(a, huma.Operation{
		OperationID: "get-rule", Method: http.MethodGet, Path: "/v1/rules/{id}", Summary: "One of your org's rules",
		Tags: []string{"rules"}, Security: bearer,
	}, func(ctx context.Context, in *ruleIDIn) (*ruleOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.GetRule(ctx, p, in.ID)
		if err != nil {
			return nil, h.fail(err)
		}
		return &ruleOut{ETag: ruleETag(r), Body: toRuleBody(r)}, nil
	})

	type ruleUpdateIn struct {
		ID      string `path:"id" format:"uuid"`
		IfMatch string `header:"If-Match" doc:"the ETag you read; the update is refused with 412 if the rule changed since"`
		ruleIn
	}
	huma.Register(a, huma.Operation{
		OperationID: "update-rule", Method: http.MethodPut, Path: "/v1/rules/{id}",
		Summary: "Replace a rule's selectors, ports and times; returns once the cell enforces the change",
		Tags:    []string{"rules"}, Security: bearer,
	}, func(ctx context.Context, in *ruleUpdateIn) (*ruleOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		version, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.UpdateRule(ctx, p, in.ID, service.RuleRequest{
			Src: in.Body.Src, Dst: in.Body.Dst, Ports: in.Body.Ports, StartsAt: in.Body.StartsAt, ExpiresAt: in.Body.ExpiresAt,
		}, version)
		if err != nil {
			return nil, h.fail(err)
		}
		return &ruleOut{ETag: ruleETag(r), Body: toRuleBody(r)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "delete-rule", Method: http.MethodDelete, Path: "/v1/rules/{id}",
		Summary: "Delete a rule; returns the deleted rule once the cell enforces the change", Tags: []string{"rules"},
		Security: bearer,
	}, func(ctx context.Context, in *ruleIDIn) (*ruleDeletedOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.DeleteRule(ctx, p, in.ID)
		if err != nil {
			return nil, h.fail(err)
		}
		return &ruleDeletedOut{Body: toRuleBody(r)}, nil
	})
}
