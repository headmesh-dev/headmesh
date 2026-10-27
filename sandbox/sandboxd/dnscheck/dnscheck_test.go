package dnscheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
)

type fakeTS struct {
	state string
	ping  bool
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
