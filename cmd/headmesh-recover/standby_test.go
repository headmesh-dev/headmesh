package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestStandbyCommandValidatesInputBeforeUsingCredentials(t *testing.T) {
	base := strings.Fields("-primary-id 71 -primary-name primary-fixture -primary-ipv4-id 81 -primary-ipv6-id 82 -run-id outage-fixture -firewall-id 12")
	fixture := filepath.Join(t.TempDir(), "cloud-init.yaml")
	if err := os.WriteFile(fixture, []byte("#cloud-config\nssh_pwauth: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientReached := errors.New("validated input reached the client factory")
	for _, tc := range []struct {
		command string
		args    []string
		useAPI  bool
	}{
		{"start-standby", nil, false},
		{"start-standby", append(slices.Clone(base), "-standby-id", "71"), false},
		{"start-standby", append(slices.Clone(base), "-standby-id", "91"), true},
		{"create-standby", append(slices.Clone(base), "-server-limit", "5", "-ssh-key-ids", "13,0", "-cloud-init", fixture), false},
		{"create-standby", append(slices.Clone(base), "-server-limit", "5", "-ssh-key-ids", "13,14", "-cloud-init", fixture), true},
	} {
		err := runStandby(t.Context(), tc.command, tc.args, io.Discard, func() (*hcloud.Client, error) { return nil, clientReached })
		if err == nil || errors.Is(err, clientReached) != tc.useAPI {
			t.Fatalf("%s %v: error %v, expected API access %t", tc.command, tc.args, err, tc.useAPI)
		}
	}
}

func TestStandbyOutputUsesAssignableIPv6AndPayloadArchitecture(t *testing.T) {
	_, ipv6Network, err := net.ParseCIDR("2001:db8:91::/64")
	if err != nil {
		t.Fatal(err)
	}
	for architecture, want := range map[hcloud.Architecture]string{hcloud.ArchitectureX86: "amd64", hcloud.ArchitectureARM: "arm64"} {
		server := &hcloud.Server{
			ID: 91, Name: "hm-recover-71-example", Status: hcloud.ServerStatusRunning, ServerType: &hcloud.ServerType{Architecture: architecture},
			PublicNet: hcloud.ServerPublicNet{
				IPv4: hcloud.ServerPublicNetIPv4{IP: net.ParseIP("192.0.2.91")},
				IPv6: hcloud.ServerPublicNetIPv6{Network: ipv6Network},
			},
		}
		var output bytes.Buffer
		if err := writeStandby(&output, server); err != nil {
			t.Fatal(err)
		}
		var result standbyOutput
		if err := json.NewDecoder(&output).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.Architecture != want || result.IPv4 != "192.0.2.91" || result.IPv6 != "2001:db8:91::1" ||
			result.ServerID != 91 || result.Name != server.Name || result.Status != "running" {
			t.Fatalf("standby output = %+v", result)
		}
	}
}
