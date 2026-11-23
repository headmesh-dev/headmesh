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

var standbyCandidates = [...]struct{ serverType, location string }{
	{"cx23", "fsn1"}, {"cx23", "hel1"}, {"cx33", "fsn1"}, {"cx33", "hel1"}, {"cpx21", "fsn1"}, {"cax11", "fsn1"},
}

// StandbyTarget pins the server created for a recovery run and its firewall.
type StandbyTarget struct {
	RunID      string
	ServerID   int64
	FirewallID int64
}

func standbyName(primaryID int64, runID string) string {
	digest := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("hm-recover-%d-%x", primaryID, digest[:8])
}

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
	name := standbyName(primary.ServerID, config.RunID)
	if _, err := checkStandbyCapacity(ctx, client, primary.ServerID, config.RunID, config.ServerLimit); err != nil {
		return nil, err
	}

	for _, candidate := range standbyCandidates {
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
		server, err := readStandby(ctx, client, primary, StandbyTarget{
			RunID: config.RunID, ServerID: result.Server.ID, FirewallID: config.FirewallID,
		})
		if err != nil {
			return nil, fmt.Errorf("read standby %s after creation: %w", name, err)
		}
		if server.Status != hcloud.ServerStatusOff {
			return nil, fmt.Errorf("provider could not verify standby %s is off", name)
		}
		return server, nil
	}
	return nil, fmt.Errorf("provider reported resource capacity or limit errors for every standby candidate")
}

func checkStandbyCapacity(ctx context.Context, client *hcloud.Client, primaryID int64, runID string, limit int) (int, error) {
	servers, err := client.Server.All(ctx)
	if err != nil {
		return 0, fmt.Errorf("list servers for standby capacity: %w", err)
	}
	name, label := standbyName(primaryID, runID), strconv.FormatInt(primaryID, 10)
	for _, server := range servers {
		if server.Name == name || server.Labels[standbyLabel] == label {
			return 0, fmt.Errorf("server %d (%s) already exists for this recovery; inspect it before provisioning again", server.ID, server.Name)
		}
	}
	if len(servers) >= limit {
		return 0, fmt.Errorf("project has %d servers and the configured limit is %d; review capacity in Hetzner Console", len(servers), limit)
	}
	return len(servers), nil
}

// StartStandby boots the pinned replacement after rechecking the provider
// fence, network and firewall. The caller holds the recovery lock. A retry
// after an interrupted request can accept the same running standby.
func StartStandby(ctx context.Context, client *hcloud.Client, primary Primary, target StandbyTarget) (*hcloud.Server, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	server, err := readStandby(ctx, client, primary, target)
	if err != nil {
		return nil, err
	}
	if err := VerifyFence(ctx, client, primary); err != nil {
		return nil, fmt.Errorf("check primary fence before standby boot: %w", err)
	}
	switch server.Status {
	case hcloud.ServerStatusRunning:
		return server, nil
	case hcloud.ServerStatusOff:
	default:
		return nil, fmt.Errorf("standby status %q does not permit boot", server.Status)
	}
	action, _, err := client.Server.Poweron(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("request standby boot: %w", err)
	}
	if err := waitForAction(ctx, client, action); err != nil {
		return nil, fmt.Errorf("wait for standby boot: %w", err)
	}
	updated, err := readStandby(ctx, client, primary, target)
	if err != nil {
		return nil, fmt.Errorf("read standby after boot: %w", err)
	}
	if updated.Status != hcloud.ServerStatusRunning ||
		updated.PublicNet.IPv4.ID != server.PublicNet.IPv4.ID || !updated.PublicNet.IPv4.IP.Equal(server.PublicNet.IPv4.IP) ||
		updated.PublicNet.IPv6.ID != server.PublicNet.IPv6.ID || updated.PublicNet.IPv6.Network.String() != server.PublicNet.IPv6.Network.String() {
		return nil, fmt.Errorf("provider could not verify the standby is running with the same public addresses")
	}
	if err := VerifyFence(ctx, client, primary); err != nil {
		return nil, fmt.Errorf("check primary fence after standby boot: %w", err)
	}
	return updated, nil
}

func readStandby(ctx context.Context, client *hcloud.Client, primary Primary, target StandbyTarget) (*hcloud.Server, error) {
	if target.RunID == "" || target.FirewallID <= 0 || target.ServerID <= 0 || target.ServerID == primary.ServerID {
		return nil, fmt.Errorf("invalid recovery standby pins")
	}
	server, _, err := client.Server.GetByID(ctx, target.ServerID)
	if err != nil {
		return nil, fmt.Errorf("read standby server: %w", err)
	}
	if server == nil || server.ID != target.ServerID || server.Name != standbyName(primary.ServerID, target.RunID) ||
		server.Labels[standbyLabel] != strconv.FormatInt(primary.ServerID, 10) {
		return nil, fmt.Errorf("provider server does not match the pinned standby")
	}
	if len(server.PublicNet.FloatingIPs) != 0 || len(server.PrivateNet) != 0 {
		return nil, fmt.Errorf("standby has an unexpected floating IP or private network")
	}
	v4, v6 := server.PublicNet.IPv4, server.PublicNet.IPv6
	if v4.ID <= 0 || v4.ID == primary.IPv4ID || v4.Blocked || !v4.IP.IsGlobalUnicast() || v4.IP.To4() == nil ||
		v6.ID <= 0 || v6.ID == primary.IPv6ID || v6.Blocked || !v6.IP.IsGlobalUnicast() || v6.IP.To4() != nil || v6.Network == nil {
		return nil, fmt.Errorf("provider could not verify both standby public IPs are assigned and unblocked")
	}
	if ones, bits := v6.Network.Mask.Size(); ones != 64 || bits != 128 {
		return nil, fmt.Errorf("standby public IPv6 assignment is not a /64 network")
	}
	attachments := server.PublicNet.Firewalls
	if len(attachments) != 1 || attachments[0] == nil || attachments[0].Firewall.ID != target.FirewallID || attachments[0].Status != hcloud.FirewallStatusApplied {
		return nil, fmt.Errorf("provider could not verify firewall %d is the only applied firewall on standby %s", target.FirewallID, server.Name)
	}
	return server, nil
}
