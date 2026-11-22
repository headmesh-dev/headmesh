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

func TestStartStandbyChecksFenceAndReconcilesLostResponse(t *testing.T) {
	api := newStandbyAPI(t)
	api.name = standbyName(fixturePrimary.ServerID, fixtureStandbyConfig().RunID)
	api.loseBootResponse = true
	client := api.client(t)
	target := fixtureStandbyTarget()
	if _, err := StartStandby(t.Context(), client, fixturePrimary, target); err == nil || !strings.Contains(err.Error(), "request standby boot") {
		t.Fatalf("lost boot response = %v", err)
	}
	server, err := StartStandby(t.Context(), client, fixturePrimary, target)
	if err != nil || server == nil || server.Status != hcloud.ServerStatusRunning {
		t.Fatalf("standby = %v, error = %v", server, err)
	}
	api.checkBootTrace(t, "read standby", "read primary", "poweron", "read standby", "read primary")
}

func TestStartStandbyVerifiesRunningState(t *testing.T) {
	api := newStandbyAPI(t)
	api.name = standbyName(fixturePrimary.ServerID, fixtureStandbyConfig().RunID)
	server, err := StartStandby(t.Context(), api.client(t), fixturePrimary, fixtureStandbyTarget())
	if err != nil || server == nil || server.Status != hcloud.ServerStatusRunning {
		t.Fatalf("standby = %v, error = %v", server, err)
	}
	api.checkBootTrace(t, "read standby", "read primary", "poweron", "read standby", "read primary")
}

func TestStartStandbyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*standbyAPI, *StandbyTarget)
		want      string
		bootCalls int
	}{
		{"original server ID", func(_ *standbyAPI, g *StandbyTarget) { g.ServerID = fixturePrimary.ServerID }, "invalid recovery standby pins", 0},
		{"wrong run", func(_ *standbyAPI, g *StandbyTarget) { g.RunID = "other-run" }, "pinned standby", 0},
		{"running primary", func(a *standbyAPI, _ *StandbyTarget) { a.primary.status = "running" }, "fence before standby boot", 0},
		{"running standby still needs a fence", func(a *standbyAPI, _ *StandbyTarget) {
			a.status = "running"
			a.primary.status = "running"
		}, "fence before standby boot", 0},
		{"pending firewall", func(a *standbyAPI, _ *StandbyTarget) { a.firewallStatus = "pending" }, "only applied firewall", 0},
		{"extra firewall", func(a *standbyAPI, _ *StandbyTarget) { a.extraFirewall = true }, "only applied firewall", 0},
		{"address blocked", func(a *standbyAPI, _ *StandbyTarget) { a.ipBlocked = true }, "assigned and unblocked", 0},
		{"starting standby", func(a *standbyAPI, _ *StandbyTarget) { a.status = "starting" }, "does not permit boot", 0},
		{"provider still shows off", func(a *standbyAPI, _ *StandbyTarget) { a.ignoreBoot = true }, "same public addresses", 1},
		{"network changed during boot", func(a *standbyAPI, _ *StandbyTarget) { a.changeIPAfterBoot = true }, "same public addresses", 1},
		{"fence lost during boot", func(a *standbyAPI, _ *StandbyTarget) { a.breakFenceAfterBoot = true }, "fence after standby boot", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newStandbyAPI(t)
			target := fixtureStandbyTarget()
			api.name = standbyName(fixturePrimary.ServerID, target.RunID)
			tc.setup(api, &target)
			server, err := StartStandby(t.Context(), api.client(t), fixturePrimary, target)
			if server != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("standby = %v, error = %v, wanted %q", server, err, tc.want)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			got := 0
			for _, entry := range api.bootTrace {
				if entry == "poweron" {
					got++
				}
			}
			if got != tc.bootCalls {
				t.Fatalf("boot calls = %d, want %d", got, tc.bootCalls)
			}
		})
	}
}

func fixtureStandbyConfig() StandbyConfig {
	return StandbyConfig{
		RunID: "fixture-outage", ServerLimit: 4, Image: "ubuntu-26.04",
		FirewallID: 12, SSHKeyIDs: []int64{13}, CloudConfig: "#cloud-config\npackage_update: true\n",
	}
}

func fixtureStandbyTarget() StandbyTarget {
	config := fixtureStandbyConfig()
	return StandbyTarget{RunID: config.RunID, ServerID: 91, FirewallID: config.FirewallID}
}

type standbyAPI struct {
	t                   *testing.T
	mu                  sync.Mutex
	primary             *fenceAPI
	createCalls         []string
	createErrors        []string
	name                string
	firewallStatus      string
	extraServer         bool
	breakFenceOnList    bool
	status              string
	bootTrace           []string
	loseBootResponse    bool
	ignoreBoot          bool
	extraFirewall       bool
	ipBlocked           bool
	changeIPAfterBoot   bool
	breakFenceAfterBoot bool
}

func newStandbyAPI(t *testing.T) *standbyAPI {
	primary := newFenceAPI(t)
	primary.status = "off"
	primary.labels["fenced"] = "true"
	primary.ipv4Attached, primary.ipv6Attached, primary.ipv4Assignee = false, false, nil
	return &standbyAPI{t: t, primary: primary, firewallStatus: "applied", status: "off"}
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

func (a *standbyAPI) checkBootTrace(t *testing.T, want ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(a.bootTrace, want) {
		t.Fatalf("boot trace = %v, want %v", a.bootTrace, want)
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
		a.bootTrace = append(a.bootTrace, "read standby")
		body = map[string]any{"server": a.standby()}
	case "POST /servers/91/actions/poweron":
		a.bootTrace = append(a.bootTrace, "poweron")
		if !a.ignoreBoot {
			a.status = "running"
		}
		if a.breakFenceAfterBoot {
			a.primary.status = "running"
		}
		if a.loseBootResponse {
			w.WriteHeader(http.StatusGatewayTimeout)
			body = map[string]any{"error": map[string]string{"code": "timeout", "message": "fixture timeout"}}
		} else {
			body = map[string]any{"action": actionResponse(106, "success")}
		}
	default:
		if r.Method == http.MethodGet && r.URL.Path == "/servers/71" {
			a.bootTrace = append(a.bootTrace, "read primary")
		}
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
	firewalls := []map[string]any{{"id": 12, "status": a.firewallStatus}}
	if a.extraFirewall {
		firewalls = append(firewalls, map[string]any{"id": 18, "status": "applied"})
	}
	ipv4 := "192.0.2.91"
	if a.status == "running" && a.changeIPAfterBoot {
		ipv4 = "192.0.2.92"
	}
	return map[string]any{
		"name": a.name, "id": 91, "status": a.status, "labels": map[string]string{standbyLabel: "71"},
		"public_net": map[string]any{
			"ipv4":      map[string]any{"id": 83, "ip": ipv4, "blocked": a.ipBlocked},
			"ipv6":      map[string]any{"id": 84, "ip": "2001:db8:91::/64"},
			"firewalls": firewalls,
		},
	}
}
