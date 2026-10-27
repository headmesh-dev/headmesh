// Package netcheck answers "can the sandbox reach X, and how?": DNS through
// the system resolver and MagicDNS, `tailscale ping` (direct or DERP), and a
// TCP connect with latency. It also reads `tailscale status --json`.
package netcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tailscale wraps the tailscale CLI pointed at the sandbox's own tailscaled.
type Tailscale struct {
	Bin    string // default "tailscale"
	Socket string // default /run/tailscale/tailscaled.sock
}

func (t Tailscale) cmd(ctx context.Context, args ...string) *exec.Cmd {
	bin := t.Bin
	if bin == "" {
		bin = "tailscale"
	}
	sock := t.Socket
	if sock == "" {
		sock = "/run/tailscale/tailscaled.sock"
	}
	return exec.CommandContext(ctx, bin, append([]string{"--socket=" + sock}, args...)...) //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
}

// Node is the sandbox's own tailnet identity.
type Node struct {
	BackendState   string   `json:"backend_state"`
	NodeID         string   `json:"node_id,omitempty"`
	HostName       string   `json:"hostname,omitempty"`
	DNSName        string   `json:"dns_name,omitempty"`
	IPs            []string `json:"ips,omitempty"`
	Online         bool     `json:"online"`
	DERPRegion     string   `json:"derp_region,omitempty"`
	MagicDNSSuffix string   `json:"magicdns_suffix,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Health         []string `json:"health,omitempty"`
	Peers          int      `json:"peers"`
	Error          string   `json:"error,omitempty"`
}

type statusJSON struct {
	BackendState   string
	MagicDNSSuffix string
	Health         []string
	Self           *struct {
		ID           string
		HostName     string
		DNSName      string
		TailscaleIPs []string
		Online       bool
		Relay        string
		Tags         []string
	}
	Peer map[string]json.RawMessage
}

// ParseStatus decodes `tailscale status --json`.
func ParseStatus(b []byte) (Node, error) {
	var s statusJSON
	if err := json.Unmarshal(b, &s); err != nil {
		return Node{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	n := Node{BackendState: s.BackendState, MagicDNSSuffix: s.MagicDNSSuffix, Health: s.Health, Peers: len(s.Peer)}
	if s.Self != nil {
		n.NodeID, n.HostName, n.DNSName = s.Self.ID, s.Self.HostName, strings.TrimSuffix(s.Self.DNSName, ".")
		n.IPs, n.Online, n.DERPRegion, n.Tags = s.Self.TailscaleIPs, s.Self.Online, s.Self.Relay, s.Self.Tags
	}
	return n, nil
}

// Status returns the node; BackendState "NeedsLogin" means not enrolled yet.
func (t Tailscale) Status(ctx context.Context) Node {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	c := t.cmd(ctx, "status", "--json")
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	if out.Len() > 0 {
		if n, perr := ParseStatus(out.Bytes()); perr == nil {
			return n
		}
	}
	if err != nil {
		return Node{BackendState: "Unavailable", Error: strings.TrimSpace(err.Error() + ": " + errb.String())}
	}
	return Node{BackendState: "Unknown", Error: "empty status"}
}

// PingResult is a `tailscale ping` outcome.
type PingResult struct {
	OK        bool    `json:"ok"`
	Via       string  `json:"via,omitempty"` // "direct" or "derp"
	Detail    string  `json:"detail,omitempty"`
	LatencyMS float64 `json:"latency_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

var pongRe = regexp.MustCompile(`pong from \S+ \(([^)]*)\) via (\S+(?:\([^)]*\))?) in ([0-9.]+)ms`)

// ParsePing interprets `tailscale ping` output.
func ParsePing(out string) PingResult {
	m := pongRe.FindStringSubmatch(out)
	if m == nil {
		return PingResult{Error: strings.TrimSpace(lastNonEmpty(out))}
	}
	r := PingResult{OK: true, Detail: m[2]}
	r.LatencyMS, _ = strconv.ParseFloat(m[3], 64)
	if strings.HasPrefix(m[2], "DERP(") {
		r.Via = "derp"
	} else {
		r.Via = "direct"
	}
	return r
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Ping runs one `tailscale ping`.
func (t Tailscale) Ping(ctx context.Context, target string) PingResult {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := t.cmd(ctx, "ping", "--c", "1", "--timeout", "5s", target).CombinedOutput()
	r := ParsePing(string(out))
	if !r.OK && r.Error == "" && err != nil {
		r.Error = err.Error()
	}
	return r
}

// DNSResult compares the system resolver with MagicDNS.
type DNSResult struct {
	System      []string `json:"system,omitempty"`
	SystemError string   `json:"system_error,omitempty"`
	MagicDNS    []string `json:"magicdns,omitempty"`
	MagicError  string   `json:"magicdns_error,omitempty"`
}

// TCPResult is a connect probe.
type TCPResult struct {
	OK        bool    `json:"ok"`
	Address   string  `json:"address"`
	LatencyMS float64 `json:"latency_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Result is the net_check output.
type Result struct {
	Target  string      `json:"target"`
	Port    int         `json:"port,omitempty"`
	DNS     *DNSResult  `json:"dns,omitempty"`
	Ping    *PingResult `json:"tailscale_ping,omitempty"`
	TCP     *TCPResult  `json:"tcp,omitempty"`
	Summary string      `json:"summary"`
}

// Checker runs probes.
type Checker struct {
	TS Tailscale
	// MagicDNS is the quad-100 resolver address.
	MagicDNS string
	// DialTimeout bounds TCP connects (default 5s).
	DialTimeout time.Duration
	// SkipPing disables tailscale ping (tests, or when not enrolled).
	SkipPing bool
}

// SplitTarget accepts "host", "host:port" or "[v6]:port".
func SplitTarget(target string, port int) (string, int, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, errors.New("target is required")
	}
	if h, p, err := net.SplitHostPort(target); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("bad port in %q", target)
		}
		if port != 0 && port != n {
			return "", 0, fmt.Errorf("target %q and port %d disagree", target, port)
		}
		return h, n, nil
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %d", port)
	}
	return strings.Trim(target, "[]"), port, nil
}

// Check probes target (and port when non-zero) concurrently.
func (c *Checker) Check(ctx context.Context, target string, port int) (Result, error) {
	host, port, err := SplitTarget(target, port)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := Result{Target: host, Port: port}
	var wg sync.WaitGroup
	isIP := net.ParseIP(host) != nil
	if !isIP {
		res.DNS = &DNSResult{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.dns(ctx, host, res.DNS)
		}()
	}
	if !c.SkipPing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := c.TS.Ping(ctx, host)
			res.Ping = &p
		}()
	}
	if port > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := c.tcp(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
			res.TCP = &t
		}()
	}
	wg.Wait()
	res.Summary = summarize(res)
	return res, nil
}

func (c *Checker) dns(ctx context.Context, host string, out *DNSResult) {
	sys, err := net.DefaultResolver.LookupHost(ctx, host)
	out.System = sys
	if err != nil {
		out.SystemError = err.Error()
	}
	// Go consults /etc/hosts before any resolver, so a hosts entry shows up
	// in both columns; MagicDNS names (*.<suffix>) never live there.
	addr := c.MagicDNS
	if addr == "" {
		addr = "100.100.100.100:53"
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, network, addr)
	}}
	mctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	magic, err := r.LookupHost(mctx, host)
	out.MagicDNS = magic
	if err != nil {
		out.MagicError = err.Error()
	}
}

func (c *Checker) tcp(ctx context.Context, addr string) TCPResult {
	timeout := c.DialTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return TCPResult{Address: addr, Error: err.Error()}
	}
	_ = conn.Close()
	return TCPResult{OK: true, Address: addr, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
}

func summarize(r Result) string {
	var parts []string
	if r.DNS != nil {
		switch {
		case len(r.DNS.MagicDNS) > 0 && len(r.DNS.System) > 0:
			parts = append(parts, "DNS ok")
		case len(r.DNS.MagicDNS) > 0:
			parts = append(parts, "MagicDNS resolves but the system resolver doesn't: check that /etc/resolv.conf points at 100.100.100.100 (tailscale up --accept-dns=true)")
		case len(r.DNS.System) > 0:
			parts = append(parts, "system DNS resolves, MagicDNS doesn't")
		default:
			parts = append(parts, "name does not resolve")
		}
	}
	if r.Ping != nil {
		if r.Ping.OK {
			parts = append(parts, fmt.Sprintf("tailscale ping %s via %s (%.0f ms)", r.Ping.Via, r.Ping.Detail, r.Ping.LatencyMS))
		} else {
			parts = append(parts, "tailscale ping failed")
		}
	}
	if r.TCP != nil {
		if r.TCP.OK {
			parts = append(parts, fmt.Sprintf("TCP %s open (%.1f ms)", r.TCP.Address, r.TCP.LatencyMS))
		} else {
			parts = append(parts, "TCP "+r.TCP.Address+" failed (blocked by the tailnet policy, firewall, or nothing listening)")
		}
	}
	return strings.Join(parts, "; ")
}
