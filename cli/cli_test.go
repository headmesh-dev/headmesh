package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

type fakeTailscale struct {
	mu        sync.Mutex
	prefs     string
	calls     [][]string
	instances []Instance
	keySeen   string
}

func (f *fakeTailscale) Run(_ context.Context, inst Instance, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	f.instances = append(f.instances, inst)
	switch args[0] {
	case "debug":
		return []byte(f.prefs), nil
	case "up":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--auth-key=file:"); ok {
				b, err := os.ReadFile(v)
				if err != nil {
					return nil, err
				}
				f.keySeen = string(b)
			}
		}
		return nil, nil
	case "status":
		return []byte(`{"Self":{"ID":"42","HostName":"box"}}`), nil
	}
	return nil, errors.New("unexpected tailscale call")
}

func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/enroll":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["token"] != "hmj_testtoken" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"enrollment token is invalid"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(EnrollResponse{
				EnrollmentID: "e1", LoginServer: "https://hs1.test",
				AuthKey: "hskey-supersecret", Hostname: "box-acme", CompletionSecret: "hmc_secret",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/enrollments/e1/complete":
			var in struct {
				Secret string `json:"completion_secret"`
				Node   uint64 `json:"node_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Secret != "hmc_secret" || in.Node != 42 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(Device{ID: "d1", GivenName: "box-acme", IPv4: "100.64.0.9"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func run(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	cmd := NewRoot("test", deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestJoinEnrollsConnectsAndCompletes(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	ts := &fakeTailscale{prefs: `{"ControlURL":"","WantRunning":false}`}
	out, err := run(t, Deps{Tailscale: ts, Hostname: func() (string, error) { return "box", nil }, Getenv: func(string) string { return "" }},
		"join", "--server", srv.URL, "hmj_testtoken")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if !strings.Contains(out, "Joined as box-acme (100.64.0.9)") {
		t.Fatalf("output = %q", out)
	}
	if ts.keySeen != "hskey-supersecret" {
		t.Fatal("tailscale up did not receive the key through the key file")
	}
	for _, call := range ts.calls {
		for _, a := range call {
			if strings.Contains(a, "hskey-supersecret") {
				t.Fatalf("the auth key leaked into process arguments: %v", call)
			}
		}
	}
}

func TestJoinRefusesToHijackAnotherTailnet(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	ts := &fakeTailscale{prefs: `{"ControlURL":"https://controlplane.tailscale.com","WantRunning":true}`}
	_, err := run(t, Deps{Tailscale: ts, Hostname: func() (string, error) { return "box", nil }, Getenv: func(string) string { return "" }},
		"join", "--server", srv.URL, "hmj_testtoken")
	if !errors.Is(err, ErrOtherTailnet) {
		t.Fatalf("want ErrOtherTailnet, got %v", err)
	}
	for _, c := range ts.calls {
		if c[0] == "up" {
			t.Fatal("tailscale up must not run when the daemon belongs to another tailnet")
		}
	}
}

func TestJoinWithInstanceUsesItsOwnDaemon(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	ts := &fakeTailscale{prefs: `{}`}
	var ensured Instance
	_, err := run(t, Deps{
		Tailscale: ts, Hostname: func() (string, error) { return "box", nil }, Getenv: func(string) string { return "" },
		EnsureInstance: func(_ context.Context, i Instance) error { ensured = i; return nil },
	},
		"join", "--server", srv.URL, "--instance", "acme", "hmj_testtoken")
	if err != nil {
		t.Fatal(err)
	}
	if ensured.Socket != "/run/headmesh/tailscale-acme.sock" {
		t.Fatalf("instance not created: %+v", ensured)
	}
	for _, inst := range ts.instances {
		if inst.Socket != ensured.Socket {
			t.Fatal("every tailscale call must target the instance's socket")
		}
	}
	if p := ensured.Port(); p < 41650 || p >= 41850 {
		t.Fatalf("instance port %d out of range", p)
	}
}

func TestJoinSurfacesServerErrors(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	_, err := run(t, Deps{
		Tailscale: &fakeTailscale{prefs: "{}"}, Hostname: func() (string, error) { return "box", nil },
		Getenv: func(string) string { return "" },
	}, "join", "--server", srv.URL, "hmj_wrong")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("want the server's 401 problem, got %v", err)
	}
}

func TestParseInvite(t *testing.T) {
	cases := []struct {
		in, token, base string
		ok              bool
	}{
		{"hmj_abc", "hmj_abc", "", true},
		{"https://api.headmesh.dev/join/hmj_abc", "hmj_abc", "https://api.headmesh.dev", true},
		{"https://example.com/headmesh/join/hmj_abc", "hmj_abc", "https://example.com/headmesh", true},
		{"http://api.headmesh.dev/join/hmj_abc", "", "", false},
		{"https://api.headmesh.dev/join/nope", "", "", false},
		{"garbage", "", "", false},
	}
	for _, c := range cases {
		tok, base, err := parseInvite(c.in)
		if (err == nil) != c.ok || tok != c.token || base != c.base {
			t.Errorf("parseInvite(%q) = %q, %q, %v", c.in, tok, base, err)
		}
	}
	if _, err := NamedInstance("Bad_Name"); err == nil {
		t.Error("invalid instance names must be rejected")
	}
	if _, err := NamedInstance("averyverylongname"); err == nil {
		t.Error("instance names must fit an interface name")
	}
}

func TestCommandsNeedAnAPIKey(t *testing.T) {
	_, err := run(t, Deps{Getenv: func(string) string { return "" }}, "devices", "ls")
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("want ErrNoAPIKey, got %v", err)
	}
}
