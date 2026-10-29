package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// AccessReason is one thing that allows a connection: the org's template, a rule, a grant or a platform rule.
type AccessReason struct {
	Kind string // template | rule | grant | platform_rule
	// ID is the rule, grant or platform rule id. It is empty for the template, and for a platform rule the caller's
	// org doesn't own (a child org sees that its platform allows the connection, not the platform's rule ids).
	ID string
}

// AccessCheck answers whether one device may open a connection to another on one port, and why.
type AccessCheck struct {
	Allowed bool
	Port    string
	Because []AccessReason
	// Detail says why a connection is refused when the reason is the caller's own device.
	Detail string
}

// CheckAccess evaluates the policy the cell enforces for src → dst on port (for example tcp:22). The caller's org
// must own at least one of the two devices. When the other device belongs to another org, or doesn't exist, the
// answer is the same "not allowed" either way, so the check can't be used to discover other orgs' devices.
func (s *Service) CheckAccess(ctx context.Context, p Principal, srcID, dstID, port string) (*AccessCheck, error) {
	if !p.Can(ScopeDevicesRead) {
		return nil, ErrForbidden
	}
	proto, num, err := singlePort(port)
	if err != nil {
		return nil, err
	}
	srcUUID, err := store.UUID(srcID)
	if err != nil {
		return nil, fmt.Errorf("%w: src", ErrInvalid)
	}
	dstUUID, err := store.UUID(dstID)
	if err != nil {
		return nil, fmt.Errorf("%w: dst", ErrInvalid)
	}
	out := &AccessCheck{Port: fmt.Sprintf("%s:%d", proto, num)}
	if srcUUID == dstUUID {
		return nil, fmt.Errorf("%w: src and dst are the same device", ErrInvalid)
	}

	var snap policy.Snapshot
	var src, dst dbq.Device
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var srcErr, dstErr error
		src, srcErr = q.GetDevice(ctx, srcUUID)
		dst, dstErr = q.GetDevice(ctx, dstUUID)
		for _, err := range []error{srcErr, dstErr} {
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("load device: %w", err)
			}
		}
		mineSrc, mineDst := srcErr == nil && src.OrgID == p.OrgID, dstErr == nil && dst.OrgID == p.OrgID
		if !mineSrc && !mineDst {
			return fmt.Errorf("%w: neither device is in your org", ErrNotFound)
		}
		if srcErr != nil || dstErr != nil || src.CellID != dst.CellID {
			return errOtherSide
		}
		cell, err := q.GetCell(ctx, src.CellID)
		if err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		snap, err = store.LoadSnapshot(ctx, q, cell, s.now(), 0)
		return err
	})
	// A refusal reads the same whether or not the other org's device exists: it only ever mentions the caller's own.
	deny := func() *AccessCheck {
		out.Allowed, out.Because = false, nil
		out.Detail = fmt.Sprintf("no template, rule, grant or platform rule allows %s", out.Port)
		for _, d := range []struct {
			side string
			dev  dbq.Device
		}{{"src", src}, {"dst", dst}} {
			if d.dev.OrgID == p.OrgID && d.dev.State != "active" {
				out.Detail = fmt.Sprintf("%s device is %s", d.side, d.dev.State)
				break
			}
		}
		return out
	}
	if errors.Is(err, errOtherSide) {
		return deny(), nil
	}
	if err != nil {
		return nil, err
	}

	e, err := policy.Explain(snap, s.now(), src.ShortID, dst.ShortID, proto, num)
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	if !e.Allowed {
		return deny(), nil
	}
	out.Allowed = true
	for _, r := range e.Because {
		reason := AccessReason{Kind: string(r.Kind), ID: r.ID}
		if r.Kind == policy.ReasonTemplate || (r.Kind == policy.ReasonPlatformRule && r.Org != p.OrgShort) {
			reason.ID = ""
		}
		out.Because = append(out.Because, reason)
	}
	return out, nil
}

// errOtherSide: the device outside the caller's org is missing or in another cell. It is reported as "not
// allowed", exactly like an existing device that nothing lets through.
var errOtherSide = errors.New("other device not reachable")

// singlePort parses one concrete port such as tcp:22; ranges and * don't name a single connection.
func singlePort(raw string) (string, int, error) {
	pt, err := policy.ParsePort(raw)
	if err != nil || pt == policy.PortAny || strings.Contains(string(pt), "-") {
		return "", 0, fmt.Errorf("%w: port must be one proto:port such as tcp:22", ErrInvalid)
	}
	proto, n, _ := strings.Cut(string(pt), ":")
	num, err := strconv.Atoi(n)
	if err != nil {
		return "", 0, fmt.Errorf("%w: port %q", ErrInvalid, raw)
	}
	return proto, num, nil
}
