package netcheck

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestParseStatus(t *testing.T) {
	raw := `{"BackendState":"Running","MagicDNSSuffix":"mesh.xcelsior.ca","Health":[],
	"Self":{"ID":"nodeid:abc","HostName":"asus-sandbox","DNSName":"asus-sandbox.mesh.xcelsior.ca.",
	"TailscaleIPs":["100.64.0.20","fd7a::20"],"Online":true,"Relay":"tor","Tags":["tag:sandbox"]},
	"Peer":{"k1":{},"k2":{}}}`
	n, err := ParseStatus([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if n.NodeID != "nodeid:abc" || n.DNSName != "asus-sandbox.mesh.xcelsior.ca" || n.Peers != 2 || n.DERPRegion != "tor" || !n.Online {
		t.Fatalf("node %+v", n)
	}
	n, err = ParseStatus([]byte(`{"BackendState":"NeedsLogin","Self":null}`))
	if err != nil || n.BackendState != "NeedsLogin" || n.NodeID != "" {
		t.Fatalf("logged out %+v %v", n, err)
	}
	if _, err := ParseStatus([]byte("nope")); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestParsePing(t *testing.T) {
	r := ParsePing("pong from tower-server (100.64.0.3) via DERP(tor) in 41ms\n")
	if !r.OK || r.Via != "derp" || r.Detail != "DERP(tor)" || r.LatencyMS != 41 {
		t.Fatalf("derp %+v", r)
	}
	r = ParsePing("pong from tower-server (100.64.0.3) via 192.168.1.20:41641 in 2.5ms")
	if !r.OK || r.Via != "direct" || r.LatencyMS != 2.5 {
		t.Fatalf("direct %+v", r)
	}
	r = ParsePing("timeout waiting for ping reply\n")
	if r.OK || !strings.Contains(r.Error, "timeout") {
		t.Fatalf("timeout %+v", r)
	}
}

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in       string
		port     int
		host     string
		wantPort int
		err      bool
	}{
		{"tower-server:11434", 0, "tower-server", 11434, false},
		{"tower-server", 22, "tower-server", 22, false},
		{"[fd7a::1]:80", 0, "fd7a::1", 80, false},
		{"100.64.0.1", 0, "100.64.0.1", 0, false},
		{"x:99999", 0, "", 0, true},
		{"x:80", 81, "", 0, true},
		{"", 0, "", 0, true},
	}
	for _, c := range cases {
		h, p, err := SplitTarget(c.in, c.port)
		if (err != nil) != c.err || h != c.host || p != c.wantPort {
			t.Errorf("SplitTarget(%q,%d) = %q,%d,%v", c.in, c.port, h, p, err)
		}
	}
}

func TestCheckLocalTCP(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	c := &Checker{SkipPing: true, MagicDNS: "127.0.0.1:1"}
	res, err := c.Check(context.Background(), "127.0.0.1:"+strconv.Itoa(port), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.TCP == nil || !res.TCP.OK || res.DNS != nil || !strings.Contains(res.Summary, "open") {
		t.Fatalf("open port %+v", res)
	}
	_ = ln.Close()
	res, _ = c.Check(context.Background(), "127.0.0.1", port)
	if res.TCP.OK || !strings.Contains(res.Summary, "failed") {
		t.Fatalf("closed port %+v", res)
	}
	res, _ = c.Check(context.Background(), "hm-no-such-host.invalid", 0)
	if res.DNS == nil || res.DNS.SystemError == "" || res.DNS.MagicError == "" || !strings.Contains(res.Summary, "does not resolve") {
		t.Fatalf("dns %+v %s", res.DNS, res.Summary)
	}
}
