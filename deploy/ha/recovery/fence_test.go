package recovery

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

var fixturePrimary = Primary{ServerID: 71, ServerName: "primary-fixture", IPv4ID: 81, IPv6ID: 82}

func TestFencePrimaryAndRetry(t *testing.T) {
	api := newFenceAPI(t)
	client := api.client(t)
	if err := FencePrimary(t.Context(), client, fixturePrimary); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFence(t.Context(), client, fixturePrimary); err != nil {
		t.Fatal(err)
	}
	if err := FencePrimary(t.Context(), client, fixturePrimary); err != nil {
		t.Fatalf("retry: %v", err)
	}
	api.checkCalls(t, "poweroff", "label", "detach IPv4", "detach IPv6")
}

func TestFencePrimaryResumesPartialFence(t *testing.T) {
	api := newFenceAPI(t)
	api.status = "off"
	api.labels["fenced"] = "true"
	api.ipv4Attached, api.ipv4Assignee = false, nil
	client := api.client(t)
	if err := VerifyFence(t.Context(), client, fixturePrimary); err == nil {
		t.Fatal("accepted a fence with IPv6 still attached")
	}
	if err := FencePrimary(t.Context(), client, fixturePrimary); err != nil {
		t.Fatal(err)
	}
	api.checkCalls(t, "detach IPv6")
}

func TestFencePrimaryIPv4Only(t *testing.T) {
	api := newFenceAPI(t)
	api.ipv6Attached = false
	primary := fixturePrimary
	primary.IPv6ID = 0
	if err := FencePrimary(t.Context(), api.client(t), primary); err != nil {
		t.Fatal(err)
	}
	api.checkCalls(t, "poweroff", "label", "detach IPv4")
}

func TestFencePrimaryFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(api *fenceAPI, primary *Primary)
		error string
		calls []string
	}{
		{"wrong server", func(_ *fenceAPI, p *Primary) { p.ServerName = "another" }, "pinned primary", nil},
		{"invalid pins", func(_ *fenceAPI, p *Primary) { p.ServerID = 0 }, "invalid recovery primary pins", nil},
		{"wrong assignee", func(a *fenceAPI, _ *Primary) { a.ipv4Assignee = 99 }, "assignment disagrees", nil},
		{"unconfigured IPv6", func(_ *fenceAPI, p *Primary) { p.IPv6ID = 0 }, "configured network", nil},
		{"private network", func(a *fenceAPI, _ *Primary) { a.private = true }, "private network", nil},
		{"unknown server status", func(a *fenceAPI, _ *Primary) { a.status = "unknown" }, "does not permit fencing", nil},
		{"failed poweroff", func(a *fenceAPI, _ *Primary) { a.poweroffStatus = "error" }, "wait for primary poweroff", []string{"poweroff"}},
		{"unknown poweroff status", func(a *fenceAPI, _ *Primary) { a.poweroffStatus = "unknown" }, "action status", []string{"poweroff"}},
		{"successful action but still on", func(a *fenceAPI, _ *Primary) { a.ignorePoweroff = true }, "after poweroff", []string{"poweroff"}},
		{"no label readback", func(a *fenceAPI, _ *Primary) { a.ignoreLabel = true }, "labelled fenced=true", []string{"poweroff", "label"}},
		{"IP record still assigned", func(a *fenceAPI, _ *Primary) { a.ignoreUnassign = true }, "reports another assignee", []string{"poweroff", "label", "detach IPv4"}},
		{"unassigned IP without label", func(a *fenceAPI, _ *Primary) {
			a.status, a.ipv4Attached, a.ipv4Assignee = "off", false, nil
		}, "without a confirmed fence", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newFenceAPI(t)
			primary := fixturePrimary
			test.setup(api, &primary)
			err := FencePrimary(t.Context(), api.client(t), primary)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("expected %q, got %v", test.error, err)
			}
			api.checkCalls(t, test.calls...)
		})
	}
}

type fenceAPI struct {
	t              *testing.T
	mu             sync.Mutex
	calls          []string
	labels         map[string]string
	status         string
	ipv4Attached   bool
	ipv6Attached   bool
	ipv4Assignee   any
	private        bool
	poweroffStatus string
	ignorePoweroff bool
	ignoreLabel    bool
	ignoreUnassign bool
}

func newFenceAPI(t *testing.T) *fenceAPI {
	return &fenceAPI{
		t:              t,
		status:         "running",
		labels:         map[string]string{"role": "control"},
		ipv4Attached:   true,
		ipv6Attached:   true,
		ipv4Assignee:   fixturePrimary.ServerID,
		poweroffStatus: "success",
	}
}

func (a *fenceAPI) client(t *testing.T) *hcloud.Client {
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	return hcloud.NewClient(hcloud.WithEndpoint(server.URL), hcloud.WithToken("fixture"),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}))
}

func (a *fenceAPI) checkCalls(t *testing.T, want ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(a.calls, want) {
		t.Fatalf("mutations = %v, want %v", a.calls, want)
	}
}

func (a *fenceAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	switch r.Method + " " + r.URL.Path {
	case "GET /servers/71":
		body = a.server()
	case "POST /servers/71/actions/poweroff":
		a.calls = append(a.calls, "poweroff")
		body = map[string]any{"action": actionResponse(101, "running")}
	case "GET /actions":
		if r.URL.Query().Get("id") != "101" {
			a.t.Errorf("unexpected action query %s", r.URL.RawQuery)
		}
		if a.poweroffStatus == "success" && !a.ignorePoweroff {
			a.status = "off"
		}
		body = map[string]any{"actions": []any{actionResponse(101, a.poweroffStatus)}}
	case "PUT /servers/71":
		a.calls = append(a.calls, "label")
		var request struct{ Labels map[string]string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			a.t.Errorf("decode label update: %v", err)
		}
		if a.status != "off" || !maps.Equal(request.Labels, map[string]string{"role": "control", "fenced": "true"}) {
			a.t.Errorf("unexpected label update while %q: %v", a.status, request.Labels)
		}
		if !a.ignoreLabel {
			a.labels = request.Labels
		}
		body = a.server()
	case "GET /primary_ips/81":
		body = ipResponse(81, "ipv4", "192.0.2.71", a.ipv4Assignee)
	case "GET /primary_ips/82":
		var assignee any
		if a.ipv6Attached {
			assignee = fixturePrimary.ServerID
		}
		body = ipResponse(82, "ipv6", "2001:db8:71::/64", assignee)
	case "POST /primary_ips/81/actions/unassign":
		a.requireMarkedOff()
		a.calls = append(a.calls, "detach IPv4")
		a.ipv4Attached = false
		if !a.ignoreUnassign {
			a.ipv4Assignee = nil
		}
		body = map[string]any{"action": actionResponse(102, "success")}
	case "POST /primary_ips/82/actions/unassign":
		a.requireMarkedOff()
		a.calls = append(a.calls, "detach IPv6")
		a.ipv6Attached = false
		body = map[string]any{"action": actionResponse(103, "success")}
	default:
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		a.t.Errorf("write fixture response: %v", err)
	}
}

func (a *fenceAPI) requireMarkedOff() {
	if a.status != "off" || a.labels["fenced"] != "true" {
		a.t.Error("unassign requested before the primary was off and marked")
	}
}

func (a *fenceAPI) server() map[string]any {
	publicNet := map[string]any{"ipv4": nil, "ipv6": nil}
	if a.ipv4Attached {
		publicNet["ipv4"] = map[string]any{"id": 81, "ip": "192.0.2.71"}
	}
	if a.ipv6Attached {
		publicNet["ipv6"] = map[string]any{"id": 82, "ip": "2001:db8:71::/64"}
	}
	var privateNet []map[string]any
	if a.private {
		privateNet = []map[string]any{{"network": 12, "ip": "10.0.0.2"}}
	}
	return map[string]any{"server": map[string]any{
		"id": fixturePrimary.ServerID, "name": fixturePrimary.ServerName, "status": a.status,
		"labels": a.labels, "public_net": publicNet, "private_net": privateNet,
	}}
}

func ipResponse(id int, family, address string, assignee any) map[string]any {
	assigneeType := "server"
	if assignee == nil {
		assigneeType = "unassigned"
	}
	return map[string]any{"primary_ip": map[string]any{
		"id": id, "type": family, "ip": address, "assignee_id": assignee, "assignee_type": assigneeType,
	}}
}

func actionResponse(id int, status string) map[string]any {
	action := map[string]any{"id": id, "status": status}
	if status == "error" {
		action["error"] = map[string]string{"code": "action_failed", "message": "fixture failure"}
	}
	return action
}
