package status

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
)

func fakeTailscale(t *testing.T, state string) netcheck.Tailscale {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\necho '{\"BackendState\":\"" + state + "\",\"Self\":{\"ID\":\"n1\",\"DNSName\":\"sbx.mesh.test.\",\"TailscaleIPs\":[\"100.64.0.9\"],\"Online\":true}}'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return netcheck.Tailscale{Bin: p}
}

func TestCollectAndText(t *testing.T) {
	dir := t.TempDir()
	dns := filepath.Join(dir, "dns-check.json")
	if err := procutil.WriteJSONAtomic(dns, DNSCheck{Status: "failed", Message: "IP works but names fail"}, 0o644); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(90 * time.Minute)
	c := &Collector{
		Name: "asus-sandbox-yolo", Mode: "yolo", YOLOExpiresAt: exp, StartedAt: time.Now(),
		Boot:         func() (bootid.Identity, error) { return bootid.Identity{BootID: "abcdef123456", Key: "k"}, nil },
		Tailscale:    fakeTailscale(t, "Running"),
		DNSCheckFile: dns, GPUMode: "off", Entrypoints: []string{"claude-runner"},
		RunnerHealth: "http://127.0.0.1:1/healthz", S6ServiceDir: dir,
		TokensActive: func() (int, error) { return 2, nil },
	}
	r := c.Collect(context.Background(), Options{})
	if r.Healthy || r.TokensActive != 2 || r.Tailnet.NodeID != "n1" || r.YOLOExpiresAt == nil {
		t.Fatalf("report %+v", r)
	}
	joined := strings.Join(r.Problems, "\n")
	if !strings.Contains(joined, "dns-check failed") || !strings.Contains(joined, "claude-runner") {
		t.Fatalf("problems %v", r.Problems)
	}
	var names []string
	for _, ep := range r.EntryPoints {
		names = append(names, ep.Name)
	}
	if strings.Join(names, ",") != "mcp,tailscale-ssh,claude-runner,claude-rc,openai-tunnel,codex-exec" {
		t.Fatalf("entry points %v", names)
	}
	var buf bytes.Buffer
	WriteText(&buf, r)
	out := buf.String()
	for _, want := range []string{"mode YOLO (halts", "Tailnet: sbx.mesh.test 100.64.0.9 node n1", "DNS check: failed", "Problems:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

func TestNotEnrolledIsNotAProblem(t *testing.T) {
	c := &Collector{
		Name: "x", Mode: "regular", Boot: func() (bootid.Identity, error) { return bootid.Identity{}, nil },
		Tailscale: fakeTailscale(t, "NeedsLogin"), GPUMode: "off",
	}
	r := c.Collect(context.Background(), Options{Light: true})
	if !r.Healthy {
		t.Fatalf("problems %v", r.Problems)
	}
	if b := humanBytes(5 << 30); b != "5.0 GiB" {
		t.Fatalf("bytes %q", b)
	}
}
