package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type accessReasonBody struct {
	Kind string `json:"kind" enum:"template,rule,grant,platform_rule"`
	ID   string `json:"id,omitempty" doc:"the rule, grant or platform rule; empty for the template and for your platform's own rules"`
}

// registerAccess: "can this device reach that one, and why" (plan §9.8, access check).
func (h *handlers) registerAccess(a huma.API) {
	type checkIn struct {
		Src  string `query:"src" format:"uuid" required:"true" doc:"device that would open the connection"`
		Dst  string `query:"dst" format:"uuid" required:"true" doc:"device it would connect to"`
		Port string `query:"port" required:"true" example:"tcp:22" doc:"one proto:port"`
	}
	type checkOut struct {
		Body struct {
			Allowed bool               `json:"allowed"`
			Port    string             `json:"port"`
			Because []accessReasonBody `json:"because" doc:"every entry that allows the connection on its own"`
			Detail  string             `json:"detail,omitempty" doc:"why it is refused"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "check-access", Method: http.MethodGet, Path: "/v1/access-check",
		Summary: "Whether one device may connect to another on a port, and what allows it",
		Description: "One of the two devices must be in your org. A device in another org that nothing lets through " +
			"gets the same answer as one that doesn't exist.",
		Tags: []string{"rules"}, Security: bearer,
	}, func(ctx context.Context, in *checkIn) (*checkOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		c, err := h.svc.CheckAccess(ctx, p, in.Src, in.Dst, in.Port)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &checkOut{}
		out.Body.Allowed, out.Body.Port, out.Body.Detail = c.Allowed, c.Port, c.Detail
		out.Body.Because = make([]accessReasonBody, 0, len(c.Because))
		for _, r := range c.Because {
			out.Body.Because = append(out.Body.Because, accessReasonBody{Kind: r.Kind, ID: r.ID})
		}
		return out, nil
	})
}
