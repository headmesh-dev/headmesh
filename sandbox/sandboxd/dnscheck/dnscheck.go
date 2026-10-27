// Package dnscheck is the dns-check oneshot (plan §6.6, pitfall 3): once
// the node is enrolled, a MagicDNS name must resolve. The classic failure
// is "IP works, names fail", and the result explains the fix.
package dnscheck

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// Checker runs the check.
type Checker struct {
	TS interface {
		Status(context.Context) netcheck.Node
		Ping(context.Context, string) netcheck.PingResult
	}
	Lookup func(ctx context.Context, host string) ([]string, error)
	Now    func() time.Time
}

// Run checks name (fully qualified) and ip.
func (c *Checker) Run(ctx context.Context, name, ip string) status.DNSCheck {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	lookup := c.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	res := status.DNSCheck{CheckedAt: now().UTC(), Name: name, IP: ip}
	node := c.TS.Status(ctx)
	if node.BackendState != "Running" {
		res.Status = "skipped"
		res.Message = fmt.Sprintf("tailscaled is %s: not enrolled yet (no ts_authkey), so there's no MagicDNS to check", node.BackendState)
		return res
	}
	if name == "" {
		res.Status = "skipped"
		res.Message = "tailnet.dns_check_name is empty"
		return res
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, lerr := lookup(lctx, name)
	cancel()
	res.Resolved = addrs
	if ip != "" {
		res.IPPingOK = c.TS.Ping(ctx, ip).OK
	}
	switch {
	case lerr == nil && len(addrs) > 0:
		res.Status = "ok"
		res.Message = fmt.Sprintf("%s resolves to %s", name, strings.Join(addrs, ", "))
	case res.IPPingOK:
		res.Status = "failed"
		res.Message = fmt.Sprintf("IP works but names fail: %s doesn't resolve (%v) while tailscale ping %s succeeds. "+
			"MagicDNS isn't applied: check that tailscale up ran with --accept-dns=true, that /etc/resolv.conf has "+
			"nameserver 100.100.100.100 with the tailnet search domain, and that the rootfs is writable (spike T-8). "+
			"Then restart the container.", name, lerr, ip)
	default:
		res.Status = "failed"
		res.Message = fmt.Sprintf("tailnet unreachable: %s doesn't resolve (%v) and tailscale ping %s fails. "+
			"Check the Headscale grants for this node's tag, that the node is online in `headscale nodes list`, "+
			"and that the host firewall allows UDP 41641 out.", name, lerr, ip)
	}
	return res
}
