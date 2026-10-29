package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/headmesh-dev/headmesh/internal/service"
)

type serviceBody struct {
	ID          string    `json:"id"`
	DeviceID    string    `json:"device_id"`
	Name        string    `json:"name"`
	Ports       []string  `json:"ports"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func toServiceBody(s *service.NamedService) serviceBody {
	return serviceBody{
		ID: s.ID, DeviceID: s.DeviceID, Name: s.Name, Ports: s.Ports, Description: s.Description, CreatedAt: s.CreatedAt,
	}
}

// registerServices: named ports on your devices (plan §9.8), so people and tools can find what a device offers.
func (h *handlers) registerServices(a huma.API) {
	type serviceIn struct {
		Body struct {
			DeviceID    string   `json:"device_id" format:"uuid"`
			Name        string   `json:"name" pattern:"^[a-z0-9][a-z0-9-]{0,40}$" example:"jupyter"`
			Ports       []string `json:"ports" minItems:"1" maxItems:"32" example:"[\"tcp:8888\"]"`
			Description string   `json:"description,omitempty" maxLength:"500"`
		}
	}
	type serviceOut struct{ Body serviceBody }
	huma.Register(a, huma.Operation{
		OperationID: "create-service", Method: http.MethodPost, Path: "/v1/services",
		Summary: "Name ports on one of your devices", Tags: []string{"services"}, Security: bearer,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *serviceIn) (*serviceOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		svc, err := h.svc.CreateService(ctx, p, service.NamedServiceRequest{
			DeviceID: in.Body.DeviceID, Name: in.Body.Name, Ports: in.Body.Ports, Description: in.Body.Description,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		return &serviceOut{Body: toServiceBody(svc)}, nil
	})

	type servicesOut struct {
		Body struct {
			Services []serviceBody `json:"services"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "list-services", Method: http.MethodGet, Path: "/v1/services", Summary: "Your org's services, by name",
		Tags: []string{"services"}, Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*servicesOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		svcs, err := h.svc.ListServices(ctx, p)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &servicesOut{}
		out.Body.Services = make([]serviceBody, 0, len(svcs))
		for _, svc := range svcs {
			out.Body.Services = append(out.Body.Services, toServiceBody(svc))
		}
		return out, nil
	})

	type serviceIDIn struct {
		ID string `path:"id" format:"uuid"`
	}
	huma.Register(a, huma.Operation{
		OperationID: "delete-service", Method: http.MethodDelete, Path: "/v1/services/{id}",
		Summary: "Delete a service; returns what was deleted", Tags: []string{"services"}, Security: bearer,
	}, func(ctx context.Context, in *serviceIDIn) (*serviceOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		svc, err := h.svc.DeleteService(ctx, p, in.ID)
		if err != nil {
			return nil, h.fail(err)
		}
		return &serviceOut{Body: toServiceBody(svc)}, nil
	})
}
