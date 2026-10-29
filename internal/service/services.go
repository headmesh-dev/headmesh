package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// serviceNamePattern matches the services.name check constraint.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// NamedServiceRequest names ports on one of the principal's devices, for example "jupyter" on tcp:8888.
type NamedServiceRequest struct {
	DeviceID    string
	Name        string
	Ports       []string
	Description string
}

// NamedService is a named set of ports on a device: what the device offers, so people and tools can find it and
// grant access to it by name.
type NamedService struct {
	ID          string
	DeviceID    string
	Name        string
	Ports       []string
	Description string
	CreatedAt   time.Time
}

// CreateService records a named service on one of the principal's active devices. Names are unique per org.
func (s *Service) CreateService(ctx context.Context, p Principal, req NamedServiceRequest) (*NamedService, error) {
	if !p.Can(ScopeServicesWrite) {
		return nil, ErrForbidden
	}
	if !serviceNamePattern.MatchString(req.Name) {
		return nil, fmt.Errorf("%w: name must be lowercase letters, digits and dashes, up to 41 characters", ErrInvalid)
	}
	ports := make([]string, 0, len(req.Ports))
	for _, raw := range req.Ports {
		pt, err := policy.ParsePort(raw)
		if err != nil || pt == policy.PortAny {
			return nil, fmt.Errorf("%w: port %q (a service names explicit ports such as tcp:8888)", ErrInvalid, raw)
		}
		ports = append(ports, string(pt))
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("%w: at least one port is required", ErrInvalid)
	}
	deviceID, err := store.UUID(req.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: device_id", ErrInvalid)
	}
	var out *NamedService
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		d, err := q.GetDevice(ctx, deviceID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && d.OrgID != p.OrgID) {
			return fmt.Errorf("%w: device is not in your org", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		if d.State == "removed" {
			return fmt.Errorf("%w: device is removed", ErrInvalid)
		}
		svc, err := q.CreateService(ctx, dbq.CreateServiceParams{
			OrgID: p.OrgID, DeviceID: d.ID, Name: req.Name, Ports: ports, Description: strPtr(req.Description),
		})
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: your org already has a service named %q", ErrConflict, req.Name)
		} else if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		out = toNamedService(svc)
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "service.created", TargetType: "service", TargetID: out.ID, Data: map[string]any{
				"device": out.DeviceID, "name": out.Name, "ports": ports,
			},
		})
		return err
	})
	return out, err
}

// ListServices returns the principal's org's services by name.
func (s *Service) ListServices(ctx context.Context, p Principal) ([]*NamedService, error) {
	if !p.Can(ScopeDevicesRead) && !p.Can(ScopeServicesWrite) {
		return nil, ErrForbidden
	}
	var out []*NamedService
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		svcs, err := q.ListServicesByOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("list services: %w", err)
		}
		for _, svc := range svcs {
			out = append(out, toNamedService(svc))
		}
		return nil
	})
	return out, err
}

// DeleteService removes one of the principal's org's services.
func (s *Service) DeleteService(ctx context.Context, p Principal, serviceID string) (*NamedService, error) {
	if !p.Can(ScopeServicesWrite) {
		return nil, ErrForbidden
	}
	id, err := store.UUID(serviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: service id", ErrInvalid)
	}
	var out *NamedService
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		svc, err := q.GetService(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && svc.OrgID != p.OrgID) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load service: %w", err)
		}
		if err := q.DeleteService(ctx, dbq.DeleteServiceParams{ID: id, OrgID: p.OrgID}); err != nil {
			return fmt.Errorf("delete service: %w", err)
		}
		out = toNamedService(svc)
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "service.deleted", TargetType: "service", TargetID: out.ID, Data: map[string]any{
				"device": out.DeviceID, "name": out.Name, "ports": out.Ports,
			},
		})
		return err
	})
	return out, err
}

func toNamedService(svc dbq.Service) *NamedService {
	return &NamedService{
		ID: svc.ID.String(), DeviceID: uuidString(svc.DeviceID), Name: svc.Name, Ports: svc.Ports,
		Description: derefStr(svc.Description), CreatedAt: svc.CreatedAt,
	}
}
