package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

const (
	enrollmentTTL = 10 * time.Minute
	preAuthKeyTTL = 5 * time.Minute
)

// TokenRequest creates an enrollment token for the principal's org.
type TokenRequest struct {
	Roles            []string
	DeviceKind       string
	MaxUses          int32
	TTL              time.Duration
	Ephemeral        bool
	ApprovalRequired bool
}

// TokenResponse carries the raw token exactly once, with ready-to-paste join commands.
type TokenResponse struct {
	ID        string
	Token     string
	JoinURL   string
	Commands  map[string]string
	ExpiresAt time.Time
}

// CreateEnrollmentToken mints an invite for devices joining the principal's org.
func (s *Service) CreateEnrollmentToken(ctx context.Context, p Principal, req TokenRequest) (*TokenResponse, error) {
	if !p.Can(ScopeEnrollWrite) {
		return nil, ErrForbidden
	}
	for _, r := range req.Roles {
		if err := policy.ValidateRole(r); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if req.DeviceKind == "" {
		req.DeviceKind = "server"
	}
	if req.MaxUses <= 0 {
		req.MaxUses = 1
	}
	if req.TTL <= 0 || req.TTL > 30*24*time.Hour {
		return nil, fmt.Errorf("%w: ttl must be between 1s and 30 days", ErrInvalid)
	}
	raw, hash, display, err := newSecret(PrefixEnrollmentToken)
	if err != nil {
		return nil, err
	}
	expires := s.now().Add(req.TTL)
	var tok dbq.EnrollmentToken
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		tok, err = q.CreateEnrollmentToken(ctx, dbq.CreateEnrollmentTokenParams{
			OrgID: p.OrgID, TokenHash: hash, Prefix: display, Kind: "device", Roles: nonNil(req.Roles),
			DeviceKind: req.DeviceKind, MaxUses: req.MaxUses, Ephemeral: req.Ephemeral,
			ApprovalRequired: req.ApprovalRequired, ExpiresAt: expires,
		})
		if err != nil {
			return fmt.Errorf("create token: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "enrollment_token.created", TargetType: "enrollment_token", TargetID: tok.ID.String(),
			Data: map[string]any{"roles": req.Roles, "max_uses": req.MaxUses, "expires_at": expires, "prefix": display},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &TokenResponse{
		ID: tok.ID.String(), Token: raw, JoinURL: s.PublicURL + "/join/" + raw, ExpiresAt: expires,
		Commands: map[string]string{
			"linux":    fmt.Sprintf("curl -fsSL %s | sh -s -- join %s", s.InstallURL, raw),
			"macos":    fmt.Sprintf("curl -fsSL %s | sh -s -- join %s", s.InstallURL, raw),
			"npm":      "npx @headmesh/cli join " + raw,
			"headmesh": "headmesh join " + raw,
		},
	}, nil
}

// EnrollRequest is what a joining device sends. The token is its only credential.
type EnrollRequest struct {
	Token    string
	Hostname string
	OS       string
	Arch     string
	ClientIP *netip.Addr
}

// EnrollResponse is everything the client needs to run `tailscale up`.
type EnrollResponse struct {
	EnrollmentID     string
	DeviceID         string
	LoginServer      string
	AuthKey          string
	Hostname         string
	Tags             []string
	ExpiresAt        time.Time
	CompletionSecret string
}

// Enroll spends one use of an enrollment token, creates the device, makes sure the cell's live policy already
// declares the device's tags, and returns a single-use 5-minute pre-auth key carrying them.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	name, err := deviceName(req.Hostname)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	completion, completionHash, _, err := newSecret(PrefixCompletion)
	if err != nil {
		return nil, err
	}
	var (
		org        dbq.Org
		cell       dbq.Cell
		device     dbq.Device
		enrollment dbq.Enrollment
		tok        dbq.EnrollmentToken
	)
	// The token is the credential, so this transaction runs in the system scope. Name or short-id collisions
	// roll back (returning the token use) and retry with a suffix.
	for attempt := 0; ; attempt++ {
		suffix := ""
		if attempt > 0 {
			suffix = fmt.Sprintf("-%d", attempt+1)
		}
		err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
			var err error
			tok, err = q.ConsumeEnrollmentToken(ctx, hashSecret(req.Token))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTokenRejected
			} else if err != nil {
				return fmt.Errorf("consume token: %w", err)
			}
			if tok.ApprovalRequired {
				return fmt.Errorf("%w: this token requires admin approval, which is not available yet", ErrInvalid)
			}
			if org, err = q.GetOrg(ctx, tok.OrgID); err != nil {
				return fmt.Errorf("load org: %w", err)
			}
			if org.Status != "active" {
				return fmt.Errorf("%w: org is %s", ErrForbidden, org.Status)
			}
			if cell, err = q.GetCell(ctx, org.CellID); err != nil {
				return fmt.Errorf("load cell: %w", err)
			}
			short, err := newShortID()
			if err != nil {
				return err
			}
			given := name + suffix + "-" + org.Slug
			device, err = q.CreateDevice(ctx, dbq.CreateDeviceParams{
				OrgID: org.ID, CellID: cell.ID, ShortID: short, Name: name + suffix, GivenName: given,
				Kind: tok.DeviceKind, Roles: nonNil(tok.Roles), EnrolledVia: tok.ID,
			})
			if err != nil {
				return fmt.Errorf("create device: %w", err)
			}
			enrollment, err = q.CreateEnrollment(ctx, dbq.CreateEnrollmentParams{
				OrgID: org.ID, TokenID: tok.ID, CellID: cell.ID, Status: "minted", Hostname: req.Hostname,
				Os: strPtr(req.OS), Arch: strPtr(req.Arch), ClientIp: req.ClientIP, ExpiresAt: s.now().Add(enrollmentTTL),
				DeviceID: device.ID, CompletionHash: completionHash,
			})
			if err != nil {
				return fmt.Errorf("create enrollment: %w", err)
			}
			_, err = store.AppendAudit(ctx, q, store.AuditEvent{
				OrgID: org.ID, ActorType: "system", ActorID: "enrollment",
				Action: "device.enrollment_started", TargetType: "device", TargetID: device.ID.String(), IP: req.ClientIP,
				Data: map[string]any{"token_prefix": tok.Prefix, "hostname": req.Hostname, "os": req.OS, "arch": req.Arch},
			})
			return err
		})
		if isUniqueViolation(err) && attempt < 20 {
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}

	// The cell must know the device's tags before a key carrying them exists.
	if err := s.Hub.RefreshAndWait(ctx, cell.Name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	tags := policy.DeviceTags(policy.Device{Short: device.ShortID, Org: org.ShortID, Roles: device.Roles, State: policy.DeviceActive})
	res, err := s.Hub.Do(ctx, cell.Name, &cellv1.Op{Kind: &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{
		AclTags: tags, Ephemeral: tok.Ephemeral, Expiration: timestamppb.New(s.now().Add(preAuthKeyTTL)),
	}}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !res.GetOk() {
		return nil, fmt.Errorf("%w: cell refused to mint a key: %s", ErrUnavailable, res.GetError())
	}
	key := res.GetPreAuthKey()
	keyID := int64(key.GetId()) //nolint:gosec // Headscale key ids are small positive integers
	if err := s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		_, err := q.MarkEnrollmentMinted(ctx, dbq.MarkEnrollmentMintedParams{ID: enrollment.ID, PreauthKeyID: &keyID})
		if err != nil {
			return fmt.Errorf("record minted key: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &EnrollResponse{
		EnrollmentID: enrollment.ID.String(), DeviceID: device.ID.String(), LoginServer: cell.ServerUrl,
		AuthKey: key.GetKey(), Hostname: device.GivenName, Tags: tags, ExpiresAt: key.GetExpiration().AsTime(),
		CompletionSecret: completion,
	}, nil
}

// CompleteRequest binds the registered Headscale node to the enrollment.
type CompleteRequest struct {
	EnrollmentID     string
	CompletionSecret string
	NodeID           uint64
}

// Device is the API view of a device.
type Device struct {
	ID         string
	ShortID    string
	Name       string
	GivenName  string
	Kind       string
	Roles      []string
	IPv4, IPv6 string
	Online     bool
	State      string
}

// CompleteEnrollment checks the caller holds the completion secret, asks the cell which pre-auth key the node
// registered with, and only then binds the node to the device and gives it its MagicDNS name.
func (s *Service) CompleteEnrollment(ctx context.Context, req CompleteRequest) (*Device, error) {
	id, err := store.UUID(req.EnrollmentID)
	if err != nil {
		return nil, fmt.Errorf("%w: enrollment id", ErrInvalid)
	}
	var (
		e      dbq.Enrollment
		cell   dbq.Cell
		device dbq.Device
	)
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		if e, err = q.GetEnrollment(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load enrollment: %w", err)
		}
		if !secretMatches(req.CompletionSecret, e.CompletionHash) {
			return ErrUnauthorized
		}
		if e.Status != "minted" || e.PreauthKeyID == nil {
			return fmt.Errorf("%w: enrollment is %s", ErrConflict, e.Status)
		}
		if s.now().After(e.ExpiresAt) {
			return fmt.Errorf("%w: enrollment expired", ErrConflict)
		}
		if cell, err = q.GetCell(ctx, e.CellID); err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		if device, err = q.GetDevice(ctx, e.DeviceID); err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res, err := s.Hub.Do(ctx, cell.Name, &cellv1.Op{Kind: &cellv1.Op_GetNode{GetNode: &cellv1.GetNode{NodeId: req.NodeID}}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	node := res.GetNode()
	if !res.GetOk() || node == nil {
		return nil, fmt.Errorf("%w: node %d is not registered", ErrNotFound, req.NodeID)
	}
	if int64(node.GetPreAuthKeyId()) != *e.PreauthKeyID { //nolint:gosec // small positive ids
		return nil, fmt.Errorf("%w: node %d did not register with this enrollment's key", ErrForbidden, req.NodeID)
	}
	renamed, err := s.Hub.Do(ctx, cell.Name, &cellv1.Op{Kind: &cellv1.Op_RenameNode{RenameNode: &cellv1.RenameNode{
		NodeId: req.NodeID, NewName: device.GivenName,
	}}})
	if err != nil {
		return nil, fmt.Errorf("%w: rename node: %w", ErrUnavailable, err)
	}
	if !renamed.GetOk() {
		return nil, fmt.Errorf("%w: rename node: %s", ErrUnavailable, renamed.GetError())
	}
	nodeID := int64(req.NodeID) //nolint:gosec // small positive ids
	var v4, v6 *netip.Addr
	for _, raw := range node.GetIpAddresses() {
		if a, err := netip.ParseAddr(raw); err == nil {
			if a.Is4() {
				v4 = &a
			} else {
				v6 = &a
			}
		}
	}
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		if device, err = q.AttachDeviceNode(ctx, dbq.AttachDeviceNodeParams{ID: device.ID, HeadscaleNodeID: &nodeID, Ipv4: v4, Ipv6: v6}); err != nil {
			return fmt.Errorf("attach node: %w", err)
		}
		if _, err := q.CompleteEnrollment(ctx, dbq.CompleteEnrollmentParams{ID: e.ID, HeadscaleNodeID: &nodeID, DeviceID: device.ID}); err != nil {
			return fmt.Errorf("complete enrollment: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: e.OrgID, ActorType: "system", ActorID: "enrollment",
			Action: "device.enrolled", TargetType: "device", TargetID: device.ID.String(),
			Data: map[string]any{"node_id": req.NodeID, "given_name": device.GivenName, "ips": node.GetIpAddresses()},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return toDevice(device), nil
}

func toDevice(d dbq.Device) *Device {
	out := &Device{
		ID: d.ID.String(), ShortID: d.ShortID, Name: d.Name, GivenName: d.GivenName, Kind: d.Kind,
		Roles: d.Roles, Online: d.Online, State: d.State,
	}
	if d.Ipv4 != nil {
		out.IPv4 = d.Ipv4.String()
	}
	if d.Ipv6 != nil {
		out.IPv6 = d.Ipv6.String()
	}
	return out
}

// ListDevices returns the principal's org devices.
func (s *Service) ListDevices(ctx context.Context, p Principal) ([]*Device, error) {
	if !p.Can(ScopeDevicesRead) {
		return nil, ErrForbidden
	}
	var out []*Device
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		ds, err := q.ListDevicesByOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		for _, d := range ds {
			out = append(out, toDevice(d))
		}
		return nil
	})
	return out, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}
