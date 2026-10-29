package api

import (
	"context"
	"net/http"
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
	type ruleOut struct{ Body ruleBody }
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
		return &ruleOut{Body: toRuleBody(r)}, nil
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
		return &ruleOut{Body: toRuleBody(r)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "delete-rule", Method: http.MethodDelete, Path: "/v1/rules/{id}",
		Summary: "Delete a rule; returns the deleted rule once the cell enforces the change", Tags: []string{"rules"},
		Security: bearer,
	}, func(ctx context.Context, in *ruleIDIn) (*ruleOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.DeleteRule(ctx, p, in.ID)
		if err != nil {
			return nil, h.fail(err)
		}
		return &ruleOut{Body: toRuleBody(r)}, nil
	})
}
