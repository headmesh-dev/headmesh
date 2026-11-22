package recovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestCreateStandbyFallsBackOnCapacityErrors(t *testing.T) {
	api := newStandbyAPI(t)
	api.createErrors = []string{"resource_unavailable", "resource_limit_exceeded"}
	client := api.client(t)
	config := fixtureStandbyConfig()
	server, err := CreateStandby(t.Context(), client, fixturePrimary, config)
	if err != nil || server == nil || server.ID != 91 || server.Status != hcloud.ServerStatusOff {
		t.Fatalf("standby = %v, error = %v", server, err)
	}
	if _, err := CreateStandby(t.Context(), client, fixturePrimary, config); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("repeat create did not fail closed: %v", err)
	}
	api.checkCreates(t, "cx23@fsn1", "cx23@hel1", "cx33@fsn1")
}

func TestCreateStandbyExhaustsPlannedCapacity(t *testing.T) {
	api := newStandbyAPI(t)
	api.createErrors = slices.Repeat([]string{"resource_unavailable"}, 6)
	server, err := CreateStandby(t.Context(), api.client(t), fixturePrimary, fixtureStandbyConfig())
	if server != nil || err == nil || !strings.Contains(err.Error(), "every standby candidate") {
		t.Fatalf("standby = %v, error = %v", server, err)
	}
	api.checkCreates(t, "cx23@fsn1", "cx23@hel1", "cx33@fsn1", "cx33@hel1", "cpx21@fsn1", "cax11@fsn1")
}

func TestCreateStandbyDoesNotDuplicateAfterTimeout(t *testing.T) {
	api := newStandbyAPI(t)
	api.createErrors = []string{"timeout"}
	client := api.client(t)
	config := fixtureStandbyConfig()
	if _, err := CreateStandby(t.Context(), client, fixturePrimary, config); err == nil || !strings.Contains(err.Error(), "create standby") {
		t.Fatalf("create timeout = %v", err)
	}
	if _, err := CreateStandby(t.Context(), client, fixturePrimary, config); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("retry could not identify the existing standby: %v", err)
	}
	api.checkCreates(t, "cx23@fsn1")
}

func TestCreateStandbyFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*standbyAPI, *StandbyConfig)
		want   string
		writes []string
	}{
		{"full project across pages", func(a *standbyAPI, c *StandbyConfig) { a.extraServer = true; c.ServerLimit = 2 }, "configured limit is 2", nil},
		{"broken fence", func(a *standbyAPI, _ *StandbyConfig) { a.primary.status = "running" }, "read fence before provisioning", nil},
		{"fence changes after capacity check", func(a *standbyAPI, _ *StandbyConfig) { a.breakFenceOnList = true }, "check primary fence before creating", nil},
		{"no applied firewall", func(a *standbyAPI, _ *StandbyConfig) { a.firewallStatus = "pending" }, "could not verify firewall", []string{"cx23@fsn1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newStandbyAPI(t)
			config := fixtureStandbyConfig()
			test.setup(api, &config)
			server, err := CreateStandby(t.Context(), api.client(t), fixturePrimary, config)
			if server != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("standby = %v, error = %v, wanted %q", server, err, test.want)
			}
			api.checkCreates(t, test.writes...)
		})
	}
}

func fixtureStandbyConfig() StandbyConfig {
	return StandbyConfig{
		RunID: "fixture-outage", ServerLimit: 4, Image: "ubuntu-26.04",
		FirewallID: 12, SSHKeyIDs: []int64{13}, CloudConfig: "#cloud-config\npackage_update: true\n",
	}
}

type standbyAPI struct {
	t                *testing.T
	mu               sync.Mutex
	primary          *fenceAPI
	createCalls      []string
	createErrors     []string
	name             string
	firewallStatus   string
	extraServer      bool
	breakFenceOnList bool
}

func newStandbyAPI(t *testing.T) *standbyAPI {
	primary := newFenceAPI(t)
	primary.status = "off"
	primary.labels["fenced"] = "true"
	primary.ipv4Attached, primary.ipv6Attached, primary.ipv4Assignee = false, false, nil
	return &standbyAPI{t: t, primary: primary, firewallStatus: "applied"}
}

func (a *standbyAPI) client(t *testing.T) *hcloud.Client {
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	return hcloud.NewClient(hcloud.WithEndpoint(server.URL), hcloud.WithToken("fixture"), hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}))
}

func (a *standbyAPI) checkCreates(t *testing.T, want ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(a.createCalls, want) {
		t.Fatalf("creates = %v, want %v", a.createCalls, want)
	}
}

func (a *standbyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	switch r.Method + " " + r.URL.Path {
	case "GET /servers":
		body = a.listPage(r.URL.Query().Get("page"))
	case "POST /servers":
		body = a.create(w, r)
	case "GET /servers/91":
		body = map[string]any{"server": a.standby()}
	default:
		a.primary.ServeHTTP(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		a.t.Errorf("write fixture response: %v", err)
	}
}

func (a *standbyAPI) listPage(page string) map[string]any {
	if a.breakFenceOnList {
		a.primary.status = "running"
	}
	var servers []any
	nextPage := 0
	switch page {
	case "1":
		servers = append(servers, a.primary.server()["server"])
		if a.extraServer || a.name != "" {
			nextPage = 2
		}
	case "2":
		if a.extraServer {
			servers = append(servers, map[string]any{"id": 92, "name": "other-fixture"})
		}
		if a.name != "" {
			servers = append(servers, a.standby())
		}
	default:
		a.t.Errorf("unexpected page %q", page)
	}
	return map[string]any{"servers": servers, "meta": map[string]any{"pagination": map[string]int{"next_page": nextPage}}}
}

func (a *standbyAPI) create(w http.ResponseWriter, r *http.Request) map[string]any {
	var request struct {
		Name     string
		Image    string
		Location string
		Labels   map[string]string
		Type     string  `json:"server_type"`
		SSHKeys  []int64 `json:"ssh_keys"`
		Start    *bool   `json:"start_after_create"`
		UserData string  `json:"user_data"`
		Firewall []struct {
			ID int64 `json:"firewall"`
		} `json:"firewalls"`
		PublicNet struct {
			IPv4 bool `json:"enable_ipv4"`
			IPv6 bool `json:"enable_ipv6"`
		} `json:"public_net"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		a.t.Errorf("decode create request: %v", err)
	}
	if !strings.HasPrefix(request.Name, "hm-recover-71-") || request.Image != "ubuntu-26.04" || request.Labels[standbyLabel] != "71" ||
		request.Start == nil || *request.Start || !slices.Equal(request.SSHKeys, []int64{13}) ||
		request.UserData != fixtureStandbyConfig().CloudConfig || len(request.Firewall) != 1 || request.Firewall[0].ID != 12 ||
		!request.PublicNet.IPv4 || !request.PublicNet.IPv6 {
		a.t.Errorf("unexpected create request %v", request)
	}
	a.createCalls = append(a.createCalls, request.Type+"@"+request.Location)
	if i := len(a.createCalls) - 1; i < len(a.createErrors) {
		code := a.createErrors[i]
		if code == "timeout" {
			// The server might already exist even though the caller saw a timeout.
			a.name = request.Name
			w.WriteHeader(http.StatusGatewayTimeout)
		} else {
			w.WriteHeader(http.StatusConflict)
		}
		return map[string]any{"error": map[string]string{"code": code, "message": "fixture failure"}}
	}
	a.name = request.Name
	w.WriteHeader(http.StatusCreated)
	return map[string]any{"server": a.standby(), "action": actionResponse(104, "success"), "next_actions": []any{actionResponse(105, "success")}}
}

func (a *standbyAPI) standby() map[string]any {
	return map[string]any{
		"name": a.name, "id": 91, "status": "off", "labels": map[string]string{standbyLabel: "71"},
		"public_net": map[string]any{
			"ipv4":      map[string]any{"id": 83, "ip": "192.0.2.91"},
			"ipv6":      map[string]any{"id": 84, "ip": "2001:db8:91::/64"},
			"firewalls": []map[string]any{{"id": 12, "status": a.firewallStatus}},
		},
	}
}
