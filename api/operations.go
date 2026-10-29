package api

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/headmesh-dev/headmesh/internal/service"
	"github.com/headmesh-dev/headmesh/internal/store"
)

type deviceBody struct {
	ID        string   `json:"id"`
	ShortID   string   `json:"short_id"`
	Name      string   `json:"name"`
	GivenName string   `json:"given_name" doc:"MagicDNS name: <name>-<org slug>"`
	Kind      string   `json:"kind"`
	Roles     []string `json:"roles"`
	IPv4      string   `json:"ipv4,omitempty"`
	IPv6      string   `json:"ipv6,omitempty"`
	Online    bool     `json:"online"`
	State     string   `json:"state"`
}

func toDeviceBody(d *service.Device) deviceBody {
	return deviceBody{
		ID: d.ID, ShortID: d.ShortID, Name: d.Name, GivenName: d.GivenName, Kind: d.Kind, Roles: d.Roles,
		IPv4: d.IPv4, IPv6: d.IPv6, Online: d.Online, State: d.State,
	}
}

type grantBody struct {
	ID          string    `json:"id"`
	GranteeOrg  string    `json:"grantee_org,omitempty"`
	DstDeviceID string    `json:"dst_device_id"`
	Ports       []string  `json:"ports"`
	StartsAt    time.Time `json:"starts_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Revoked     bool      `json:"revoked"`
	ExternalRef string    `json:"external_ref,omitempty"`
	Enforced    bool      `json:"enforced" doc:"true when the cell confirmed the resulting policy is live"`
}

func toGrantBody(g *service.Grant) grantBody {
	return grantBody{
		ID: g.ID, GranteeOrg: g.GranteeOrg, DstDeviceID: g.DstDeviceID, Ports: g.Ports, StartsAt: g.StartsAt,
		ExpiresAt: g.ExpiresAt, Revoked: g.Revoked, ExternalRef: g.ExternalRef, Enforced: g.Enforced,
	}
}

func (h *handlers) register(a huma.API) {
	h.registerAudit(a)
	type meOut struct {
		Body struct {
			Org    string   `json:"org" doc:"short id of the key's org"`
			Scopes []string `json:"scopes"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "me", Method: http.MethodGet, Path: "/v1/me", Summary: "Describe the calling API key",
		Tags: []string{"account"}, Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*meOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		out := &meOut{}
		out.Body.Org, out.Body.Scopes = p.OrgShort, p.Scopes
		return out, nil
	})

	type tokenIn struct {
		Body struct {
			Roles            []string `json:"roles,omitempty" maxItems:"16"`
			DeviceKind       string   `json:"device_kind,omitempty" enum:"server,gpu,laptop,sandbox,agent,ingress,other"`
			MaxUses          int32    `json:"max_uses,omitempty" minimum:"1" maximum:"10000"`
			TTLSeconds       int64    `json:"ttl_seconds" minimum:"60" maximum:"2592000"`
			Ephemeral        bool     `json:"ephemeral,omitempty" doc:"nodes are removed after going offline"`
			ApprovalRequired bool     `json:"approval_required,omitempty"`
		}
	}
	type tokenOut struct {
		Body struct {
			ID        string            `json:"id"`
			Token     string            `json:"token" doc:"shown once"`
			JoinURL   string            `json:"join_url"`
			Commands  map[string]string `json:"commands"`
			ExpiresAt time.Time         `json:"expires_at"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "create-enrollment-token", Method: http.MethodPost, Path: "/v1/enrollment-tokens",
		Summary: "Create an invite link and one-line install commands", Tags: []string{"enrollment"},
		Security: bearer, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *tokenIn) (*tokenOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		r, err := h.svc.CreateEnrollmentToken(ctx, p, service.TokenRequest{
			Roles: in.Body.Roles, DeviceKind: in.Body.DeviceKind, MaxUses: in.Body.MaxUses,
			TTL: time.Duration(in.Body.TTLSeconds) * time.Second, Ephemeral: in.Body.Ephemeral,
			ApprovalRequired: in.Body.ApprovalRequired,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		out := &tokenOut{}
		out.Body.ID, out.Body.Token, out.Body.JoinURL, out.Body.Commands, out.Body.ExpiresAt = r.ID, r.Token, r.JoinURL, r.Commands, r.ExpiresAt
		return out, nil
	})

	type enrollIn struct {
		Body struct {
			Token    string `json:"token" minLength:"20" maxLength:"200"`
			Hostname string `json:"hostname" minLength:"1" maxLength:"253"`
			OS       string `json:"os,omitempty" maxLength:"32"`
			Arch     string `json:"arch,omitempty" maxLength:"32"`
		}
	}
	type enrollOut struct {
		Body struct {
			EnrollmentID     string    `json:"enrollment_id"`
			DeviceID         string    `json:"device_id"`
			LoginServer      string    `json:"login_server"`
			AuthKey          string    `json:"auth_key" doc:"single-use Headscale pre-auth key"`
			Hostname         string    `json:"hostname" doc:"pass to tailscale up --hostname"`
			Tags             []string  `json:"tags"`
			ExpiresAt        time.Time `json:"expires_at"`
			CompletionSecret string    `json:"completion_secret" doc:"present to complete-enrollment"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "enroll", Method: http.MethodPost, Path: "/v1/enroll",
		Summary: "Exchange an enrollment token for a single-use pre-auth key", Tags: []string{"enrollment"},
		Middlewares: huma.Middlewares{h.rateLimit(a)},
	}, func(ctx context.Context, in *enrollIn) (*enrollOut, error) {
		r, err := h.svc.Enroll(ctx, service.EnrollRequest{
			Token: in.Body.Token, Hostname: in.Body.Hostname,
			OS: in.Body.OS, Arch: in.Body.Arch, ClientIP: clientIPFrom(ctx),
		})
		if err != nil {
			return nil, h.fail(err)
		}
		out := &enrollOut{}
		out.Body.EnrollmentID, out.Body.DeviceID, out.Body.LoginServer, out.Body.AuthKey = r.EnrollmentID, r.DeviceID, r.LoginServer, r.AuthKey
		out.Body.Hostname, out.Body.Tags, out.Body.ExpiresAt, out.Body.CompletionSecret = r.Hostname, r.Tags, r.ExpiresAt, r.CompletionSecret
		return out, nil
	})

	type completeIn struct {
		ID   string `path:"id"`
		Body struct {
			CompletionSecret string `json:"completion_secret" minLength:"20" maxLength:"200"`
			NodeID           uint64 `json:"node_id" minimum:"1"`
		}
	}
	type deviceOut struct{ Body deviceBody }
	huma.Register(a, huma.Operation{
		OperationID: "complete-enrollment", Method: http.MethodPost, Path: "/v1/enrollments/{id}/complete",
		Summary: "Bind the registered node to the enrolled device", Tags: []string{"enrollment"},
		Middlewares: huma.Middlewares{h.rateLimit(a)},
	}, func(ctx context.Context, in *completeIn) (*deviceOut, error) {
		d, err := h.svc.CompleteEnrollment(ctx, service.CompleteRequest{
			EnrollmentID:     in.ID,
			CompletionSecret: in.Body.CompletionSecret, NodeID: in.Body.NodeID,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		return &deviceOut{Body: toDeviceBody(d)}, nil
	})

	type devicesOut struct {
		Body struct {
			Devices []deviceBody `json:"devices"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "list-devices", Method: http.MethodGet, Path: "/v1/devices", Summary: "List your org's devices",
		Tags: []string{"devices"}, Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*devicesOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		ds, err := h.svc.ListDevices(ctx, p)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &devicesOut{}
		out.Body.Devices = make([]deviceBody, 0, len(ds))
		for _, d := range ds {
			out.Body.Devices = append(out.Body.Devices, toDeviceBody(d))
		}
		return out, nil
	})

	type grantIn struct {
		Body struct {
			GranteeOrg  string             `json:"grantee_org" pattern:"^[a-z2-7]{10}$" doc:"short id of the org receiving access"`
			Src         store.SelectorJSON `json:"src"`
			DstDeviceID string             `json:"dst_device_id" format:"uuid"`
			Ports       []string           `json:"ports" minItems:"1" maxItems:"32" example:"[\"tcp:22\"]"`
			StartsAt    *time.Time         `json:"starts_at,omitempty"`
			ExpiresAt   time.Time          `json:"expires_at"`
			Reason      string             `json:"reason,omitempty" maxLength:"500"`
			ExternalRef string             `json:"external_ref,omitempty" maxLength:"200" doc:"idempotency key, e.g. a rental id"`
		}
	}
	type grantOut struct{ Body grantBody }
	huma.Register(a, huma.Operation{
		OperationID: "create-grant", Method: http.MethodPost, Path: "/v1/grants",
		Summary: "Give another org time-limited access to one of your devices", Tags: []string{"grants"},
		Security: bearer, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *grantIn) (*grantOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		g, err := h.svc.CreateGrant(ctx, p, service.GrantRequest{
			GranteeOrg: in.Body.GranteeOrg, Src: in.Body.Src,
			DstDeviceID: in.Body.DstDeviceID, Ports: in.Body.Ports, StartsAt: in.Body.StartsAt, ExpiresAt: in.Body.ExpiresAt,
			Reason: in.Body.Reason, ExternalRef: in.Body.ExternalRef,
		})
		if err != nil {
			return nil, h.fail(err)
		}
		return &grantOut{Body: toGrantBody(g)}, nil
	})

	type grantsOut struct {
		Body struct {
			Grants []grantBody `json:"grants"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "list-grants", Method: http.MethodGet, Path: "/v1/grants", Summary: "Active grants you issued or received",
		Tags: []string{"grants"}, Security: bearer,
	}, func(ctx context.Context, _ *struct{}) (*grantsOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		gs, err := h.svc.ListGrants(ctx, p)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &grantsOut{}
		out.Body.Grants = make([]grantBody, 0, len(gs))
		for _, g := range gs {
			out.Body.Grants = append(out.Body.Grants, toGrantBody(g))
		}
		return out, nil
	})

	type revokeIn struct {
		ID string `path:"id" format:"uuid"`
	}
	huma.Register(a, huma.Operation{
		OperationID: "revoke-grant", Method: http.MethodPost, Path: "/v1/grants/{id}/revoke",
		Summary: "Revoke a grant; returns once the cell enforces it", Tags: []string{"grants"}, Security: bearer,
	}, func(ctx context.Context, in *revokeIn) (*grantOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		g, err := h.svc.RevokeGrant(ctx, p, in.ID)
		if err != nil {
			return nil, h.fail(err)
		}
		return &grantOut{Body: toGrantBody(g)}, nil
	})

	type keyIn struct {
		Body struct {
			Name       string   `json:"name" minLength:"1" maxLength:"100"`
			Scopes     []string `json:"scopes" minItems:"1"`
			TTLSeconds int64    `json:"ttl_seconds,omitempty" minimum:"0"`
		}
	}
	type keyOut struct {
		Body struct {
			ID        string     `json:"id"`
			Key       string     `json:"key" doc:"shown once"`
			Prefix    string     `json:"prefix"`
			Scopes    []string   `json:"scopes"`
			ExpiresAt *time.Time `json:"expires_at,omitempty"`
		}
	}
	huma.Register(a, huma.Operation{
		OperationID: "create-api-key", Method: http.MethodPost, Path: "/v1/api-keys", Summary: "Create an API key",
		Tags: []string{"account"}, Security: bearer, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *keyIn) (*keyOut, error) {
		p, err := h.principal(ctx)
		if err != nil {
			return nil, err
		}
		k, err := h.svc.CreateAPIKey(ctx, p, in.Body.Name, in.Body.Scopes, time.Duration(in.Body.TTLSeconds)*time.Second)
		if err != nil {
			return nil, h.fail(err)
		}
		out := &keyOut{}
		out.Body.ID, out.Body.Key, out.Body.Prefix, out.Body.Scopes, out.Body.ExpiresAt = k.ID, k.Key, k.Prefix, k.Scopes, k.ExpiresAt
		return out, nil
	})
}

type clientIPKey struct{}

func clientIPFrom(ctx context.Context) *netip.Addr {
	ip, _ := ctx.Value(clientIPKey{}).(*netip.Addr)
	return ip
}

// rateLimit throttles the public enrollment endpoints per client address and records the address for auditing.
func (h *handlers) rateLimit(a huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		ip := h.clientIP(ctx)
		key := "unknown"
		if ip != nil {
			key = ip.String()
		}
		if !h.limiter.allow(key) {
			ctx.SetHeader("Retry-After", "60")
			_ = huma.WriteErr(a, ctx, http.StatusTooManyRequests, "too many enrollment attempts; retry in a minute")
			return
		}
		next(huma.WithValue(ctx, clientIPKey{}, ip))
	}
}
