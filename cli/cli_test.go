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

// TestAuditVerifyFailsOnABrokenChain: `headmesh audit verify` must exit non-zero when the server reports a broken
// chain, so scripts and cron jobs notice.
func TestAuditVerifyFailsOnABrokenChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
		want    string
	}{
		{"intact", `{"verified":true,"checked":12}`, false, "Audit log intact: 12 events verified"},
		{"broken", `{"verified":false,"checked":3,"problem":"audit chain broken: event 4 content does not match its hash"}`, true, "BROKEN after 3 events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/audit-events/verify" || r.Header.Get("Authorization") != "Bearer hmk_test" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			env := map[string]string{"HEADMESH_API_KEY": "hmk_test", "HEADMESH_SERVER": srv.URL}
			out, err := run(t, Deps{Getenv: func(k string) string { return env[k] }}, "audit", "verify")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output %q lacks %q", out, tc.want)
			}
		})
	}
}

func TestParseSelector(t *testing.T) {
	for in, want := range map[string]Selector{
		"all": {Kind: "all"}, "users": {Kind: "users"}, "role:gpu-host": {Kind: "role", Role: "gpu-host"},
		"device:0193f0c4-0000-7000-8000-000000000001": {Kind: "device", Device: "0193f0c4-0000-7000-8000-000000000001"},
	} {
		got, err := ParseSelector(in)
		if err != nil || got != want {
			t.Fatalf("ParseSelector(%q) = %+v, %v", in, got, err)
		}
		if got.String() != in {
			t.Fatalf("%+v renders as %q, want %q", got, got.String(), in)
		}
	}
	for _, bad := range []string{"", "role:", "device", "all:x", "everyone"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Fatalf("ParseSelector(%q) accepted", bad)
		}
	}
}

// TestRuleAddSendsSelectorsAndPorts: `headmesh rule add` turns its flags into the API's rule body.
func TestRuleAddSendsSelectorsAndPorts(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/rules" || r.Header.Get("Authorization") != "Bearer hmk_test" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"r1","src":{"kind":"role","role":"ops"},"dst":{"kind":"all"},"ports":["tcp:22"],"enforced":true}`))
	}))
	defer srv.Close()
	env := map[string]string{"HEADMESH_API_KEY": "hmk_test", "HEADMESH_SERVER": srv.URL}
	out, err := run(t, Deps{Getenv: func(k string) string { return env[k] }},
		"rule", "add", "--from", "role:ops", "--to", "all", "--port", "tcp:22", "--for", "2h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Rule r1: role:ops → all on tcp:22 (enforced: true)") {
		t.Fatalf("output %q", out)
	}
	src, _ := got["src"].(map[string]any)
	if src["kind"] != "role" || src["role"] != "ops" || got["dst"].(map[string]any)["kind"] != "all" {
		t.Fatalf("selectors sent: %v", got)
	}
	if exp, _ := got["expires_at"].(string); exp == "" {
		t.Fatalf("--for didn't set expires_at: %v", got)
	}
	if _, err := run(t, Deps{Getenv: func(k string) string { return env[k] }}, "rule", "add", "--from", "everyone", "--to", "all", "--port", "tcp:22"); err == nil {
		t.Fatal("a bad selector was accepted")
	}
}

func TestAccessCheckPrintsTheAnswerAndItsReasons(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"allowed":true,"port":"tcp:22","because":[{"kind":"rule","id":"r1"},{"kind":"template"}]}`, "Allowed on tcp:22 by rule r1, template"},
		{`{"allowed":false,"port":"tcp:22","because":[],"detail":"src device is quarantined"}`, "Not allowed on tcp:22: src device is quarantined"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/v1/access-check" || q.Get("src") != "a" || q.Get("dst") != "b" || q.Get("port") != "tcp:22" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.body))
		}))
		env := map[string]string{"HEADMESH_API_KEY": "hmk_test", "HEADMESH_SERVER": srv.URL}
		out, err := run(t, Deps{Getenv: func(k string) string { return env[k] }}, "access", "check", "a", "b", "tcp:22")
		srv.Close()
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("output %q (%v), want %q", out, err, tc.want)
		}
	}
}
