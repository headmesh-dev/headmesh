package recovery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// StandbyConfig supplies the approved image, firewall, and login keys. RunID
// must stay the same when retrying a recovery run. CloudConfig is restore-mode
// cloud-init with no credentials because Hetzner stores the user data.
type StandbyConfig struct {
	RunID       string
	ServerLimit int
	Image       string
	FirewallID  int64
	SSHKeyIDs   []int64
	CloudConfig string
}

const standbyLabel = "headmesh-recovery-for"

// CreateStandby allocates a powered-off replacement after rechecking the
// original fence. The caller holds the recovery lock. After a failed or
// interrupted create, keep the same RunID so the provider name is stable.
func CreateStandby(ctx context.Context, client *hcloud.Client, primary Primary, config StandbyConfig) (*hcloud.Server, error) {
	if config.RunID == "" || config.ServerLimit <= 0 || config.Image == "" || config.FirewallID <= 0 ||
		len(config.SSHKeyIDs) == 0 || !strings.HasPrefix(config.CloudConfig, "#cloud-config\n") {
		return nil, fmt.Errorf("missing standby configuration")
	}
	keys := make([]*hcloud.SSHKey, 0, len(config.SSHKeyIDs))
	for _, id := range config.SSHKeyIDs {
		if id <= 0 {
			return nil, fmt.Errorf("invalid standby SSH key ID")
		}
		keys = append(keys, &hcloud.SSHKey{ID: id})
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if err := VerifyFence(ctx, client, primary); err != nil {
		return nil, fmt.Errorf("read fence before provisioning: %w", err)
	}

	primaryID := strconv.FormatInt(primary.ServerID, 10)
	digest := sha256.Sum256([]byte(config.RunID))
	name := fmt.Sprintf("hm-recover-%s-%x", primaryID, digest[:8])
	servers, err := client.Server.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers for standby capacity: %w", err)
	}
	for _, server := range servers {
		if server.Name == name || server.Labels[standbyLabel] == primaryID {
			return nil, fmt.Errorf("server %d (%s) already exists for this recovery; inspect it before provisioning again", server.ID, server.Name)
		}
	}
	if len(servers) >= config.ServerLimit {
		return nil, fmt.Errorf("project has %d servers and the configured limit is %d; review capacity in Hetzner Console", len(servers), config.ServerLimit)
	}

	for _, candidate := range []struct{ serverType, location string }{
		{"cx23", "fsn1"}, {"cx23", "hel1"}, {"cx33", "fsn1"}, {"cx33", "hel1"}, {"cpx21", "fsn1"}, {"cax11", "fsn1"},
	} {
		if err := VerifyFence(ctx, client, primary); err != nil {
			return nil, fmt.Errorf("check primary fence before creating %s@%s: %w", candidate.serverType, candidate.location, err)
		}
		result, _, err := client.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name: name, StartAfterCreate: new(false),
			ServerType: &hcloud.ServerType{Name: candidate.serverType},
			Location:   &hcloud.Location{Name: candidate.location},
			Image:      &hcloud.Image{Name: config.Image},
			SSHKeys:    keys,
			UserData:   config.CloudConfig,
			Labels:     map[string]string{standbyLabel: primaryID},
			Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: hcloud.Firewall{ID: config.FirewallID}}},
			PublicNet:  &hcloud.ServerCreatePublicNet{EnableIPv4: true, EnableIPv6: true},
		})
		if hcloud.IsError(err, hcloud.ErrorCodeResourceUnavailable, hcloud.ErrorCodeResourceLimitExceeded) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create standby %s on %s@%s: %w", name, candidate.serverType, candidate.location, err)
		}
		if result.Server == nil || result.Server.ID <= 0 || result.Server.ID == primary.ServerID ||
			result.Server.Name != name || result.Server.Labels[standbyLabel] != primaryID {
			return nil, fmt.Errorf("provider did not return the expected standby identity for %s", name)
		}
		for _, action := range append([]*hcloud.Action{result.Action}, result.NextActions...) {
			if err := waitForAction(ctx, client, action); err != nil {
				return nil, fmt.Errorf("wait for standby %s creation: %w", name, err)
			}
		}
		server, _, err := client.Server.GetByID(ctx, result.Server.ID)
		if err != nil {
			return nil, fmt.Errorf("read standby %s after creation: %w", name, err)
		}
		if server == nil || server.ID != result.Server.ID || server.Name != name || server.Labels[standbyLabel] != primaryID ||
			server.Status != hcloud.ServerStatusOff || server.PublicNet.IPv4.ID == 0 || server.PublicNet.IPv4.IsUnspecified() ||
			server.PublicNet.IPv6.ID == 0 || server.PublicNet.IPv6.IsUnspecified() {
			return nil, fmt.Errorf("provider could not verify standby %s is off with its public IPs assigned", name)
		}
		if !standbyFirewallApplied(server, config.FirewallID) {
			return nil, fmt.Errorf("provider could not verify firewall %d on standby %s", config.FirewallID, name)
		}
		return server, nil
	}
	return nil, fmt.Errorf("provider reported resource capacity or limit errors for every standby candidate")
}

func standbyFirewallApplied(server *hcloud.Server, id int64) bool {
	for _, attachment := range server.PublicNet.Firewalls {
		if attachment != nil && attachment.Firewall.ID == id && attachment.Status == hcloud.FirewallStatusApplied {
			return true
		}
	}
	return false
}
