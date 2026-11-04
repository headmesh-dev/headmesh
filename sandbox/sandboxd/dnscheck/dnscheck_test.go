package dnscheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
)

type fakeTS struct {
	state string
	ping  bool
}

func TestStartupRetriesTransientMissingRecord(t *testing.T) {
	calls := 0
	ch := &Checker{
		TS: fakeTS{state: "Running"}, RetryFor: time.Second,
		Lookup: func(context.Context, string) ([]string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("record not received yet")
			}
			return []string{"100.64.0.4"}, nil
		},
	}
	res := ch.Run(context.Background(), "tower.example", "100.64.0.4")
	if calls != 2 || res.Status != "ok" {
		t.Fatalf("calls %d, result %+v", calls, res)
	}
}

func TestStartupRetryStillFailsWhenDNSNeverArrives(t *testing.T) {
	ch := &Checker{
		TS: fakeTS{state: "Running"}, RetryFor: 20 * time.Millisecond,
		Lookup: func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		},
	}
	if res := ch.Run(context.Background(), "missing.example", "100.64.0.4"); res.Status != "failed" {
		t.Fatalf("unavailable DNS was treated as healthy: %+v", res)
	}
}

func (f fakeTS) Status(context.Context) netcheck.Node { return netcheck.Node{BackendState: f.state} }

func (f fakeTS) Ping(context.Context, string) netcheck.PingResult {
	return netcheck.PingResult{OK: f.ping}
}

func TestRun(t *testing.T) {
	ok := func(context.Context, string) ([]string, error) { return []string{"100.64.0.1"}, nil }
	fail := func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	cases := []struct {
		name   string
		ts     fakeTS
		lookup func(context.Context, string) ([]string, error)
		want   string
		msg    string
	}{
		{"not enrolled", fakeTS{state: "NeedsLogin"}, ok, "skipped", "not enrolled"},
		{"resolves", fakeTS{state: "Running", ping: true}, ok, "ok", "resolves to 100.64.0.1"},
		{"ip works names fail", fakeTS{state: "Running", ping: true}, fail, "failed", "IP works but names fail"},
		{"nothing works", fakeTS{state: "Running"}, fail, "failed", "tailnet unreachable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := &Checker{TS: c.ts, Lookup: c.lookup}
			res := ch.Run(context.Background(), "prod-hetzner.mesh.xcelsior.ca", "100.64.0.1")
			if res.Status != c.want || !strings.Contains(res.Message, c.msg) {
				t.Fatalf("%+v", res)
			}
		})
	}
	ch := &Checker{TS: fakeTS{state: "Running"}}
	if res := ch.Run(context.Background(), "", ""); res.Status != "skipped" {
		t.Fatalf("empty name %+v", res)
	}
}
