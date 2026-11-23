package recovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestCheckStandbyPrerequisitesReadsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, want, runner, omit string
		capacity, sshOnly        bool
	}{
		{name: "available x86 and arm candidates"},
		{name: "runner denied", runner: "198.51.100.44", want: "does not admit SSH"},
		{name: "isolated drill firewall", sshOnly: true, want: "TCP 443 and UDP 3478"},
		{name: "missing key", omit: "key", want: "SSH key 13 is missing"},
		{name: "no compatible image", omit: "image", want: "no supported standby candidate"},
		{name: "quota exhausted", capacity: true, want: "configured limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newStandbyAPI(t)
			api.primary.status = "running"
			api.primary.ipv4Attached, api.primary.ipv6Attached = true, true
			api.primary.ipv4Assignee = new(fixturePrimary.ServerID)
			delete(api.primary.labels, "fenced")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("standby preflight sent %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var body map[string]any
				switch r.URL.Path {
				case "/firewalls/12":
					rules := []map[string]any{{"direction": "in", "protocol": "tcp", "port": "22", "source_ips": []string{"192.0.2.44/32"}}}
					if !tc.sshOnly {
						for protocol, port := range map[string]string{"tcp": "443-444", "udp": "3478"} {
							rules = append(rules, map[string]any{"direction": "in", "protocol": protocol, "port": port, "source_ips": []string{"0.0.0.0/0", "::/0"}})
						}
					}
					body = map[string]any{"firewall": map[string]any{"id": 12, "rules": rules}}
				case "/ssh_keys/13":
					if tc.omit == "key" {
						w.WriteHeader(http.StatusNotFound)
						body = map[string]any{"error": map[string]string{"code": "not_found"}}
						break
					}
					body = map[string]any{"ssh_key": map[string]any{"id": 13, "public_key": "ssh-ed25519 fixture"}}
				case "/server_types":
					name := r.URL.Query().Get("name")
					var types []map[string]any
					if arch, ok := map[string]string{"cx23": "x86", "cax11": "arm"}[name]; ok {
						types = append(types, map[string]any{
							"id": 1, "name": name, "architecture": arch, "locations": []map[string]any{
								{"id": 1, "name": "fsn1", "available": true}, {"id": 2, "name": "hel1", "available": false},
							},
						})
					}
					body = map[string]any{"server_types": types}
				case "/images":
					var images []map[string]any
					if tc.omit != "image" {
						images = append(images, map[string]any{"id": 10, "name": "ubuntu-26.04", "architecture": r.URL.Query().Get("architecture"), "status": "available"})
					}
					body = map[string]any{"images": images}
				default:
					api.ServeHTTP(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Errorf("write preflight response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			config := fixtureStandbyConfig()
			if tc.capacity {
				config.ServerLimit = 1
			}
			runner := "192.0.2.44"
			if tc.runner != "" {
				runner = tc.runner
			}
			client := hcloud.NewClient(hcloud.WithEndpoint(server.URL), hcloud.WithToken("fixture"))
			report, err := CheckStandbyPrerequisites(t.Context(), client, fixturePrimary, config, netip.MustParseAddr(runner))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("preflight error = %v, want %q", err, tc.want)
				}
				return
			}
			want := []StandbyCandidate{{"cx23", "fsn1", "amd64"}, {"cax11", "fsn1", "arm64"}}
			if err != nil || report.ProjectServers != 1 || report.ServerLimit != 4 || !slices.Equal(report.Candidates, want) {
				t.Fatalf("unexpected preflight result %+v, %v", report, err)
			}
		})
	}
}
