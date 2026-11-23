package recovery

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// StandbyPreflight describes the catalog and quota observed before fencing.
type StandbyPreflight struct {
	ProjectServers int                `json:"project_servers"`
	ServerLimit    int                `json:"server_limit"`
	Candidates     []StandbyCandidate `json:"candidates"`
}

// StandbyCandidate is a supported type, location, and release architecture.
type StandbyCandidate struct {
	ServerType   string `json:"server_type"`
	Location     string `json:"location"`
	Architecture string `json:"architecture"`
}

// CheckStandbyPrerequisites checks provider resources using read-only requests.
// The catalog and project count are observations, rather than reservations.
func CheckStandbyPrerequisites(ctx context.Context, client *hcloud.Client, primary Primary, config StandbyConfig, runner netip.Addr) (StandbyPreflight, error) {
	var report StandbyPreflight
	if config.RunID == "" || config.ServerLimit <= 0 || config.Image == "" || config.FirewallID <= 0 ||
		len(config.SSHKeyIDs) == 0 || !runner.IsGlobalUnicast() || runner.Zone() != "" || runner.Is4In6() {
		return report, fmt.Errorf("standby preflight requires the pinned provider resources and runner SSH source IP")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	state, err := readPrimary(ctx, client, primary)
	if err != nil {
		return report, err
	}
	if state.server.Status != hcloud.ServerStatusRunning && state.server.Status != hcloud.ServerStatusOff {
		return report, fmt.Errorf("primary status %q does not permit fencing", state.server.Status)
	}
	report.ServerLimit = config.ServerLimit
	report.ProjectServers, err = checkStandbyCapacity(ctx, client, primary.ServerID, config.RunID, config.ServerLimit)
	if err != nil {
		return StandbyPreflight{}, err
	}
	if err := verifyStandbyAccess(ctx, client, config, runner); err != nil {
		return StandbyPreflight{}, err
	}
	report.Candidates, err = readStandbyCatalog(ctx, client, config.Image)
	if err != nil {
		return StandbyPreflight{}, err
	}
	return report, nil
}

func verifyStandbyAccess(ctx context.Context, client *hcloud.Client, config StandbyConfig, runner netip.Addr) error {
	fw, _, err := client.Firewall.GetByID(ctx, config.FirewallID)
	if err != nil {
		return fmt.Errorf("read the approved standby firewall: %w", err)
	}
	if fw == nil || fw.ID != config.FirewallID {
		return fmt.Errorf("the approved standby firewall is missing")
	}
	if !firewallAllows(fw.Rules, hcloud.FirewallRuleProtocolTCP, 22, runner, false) {
		return fmt.Errorf("the standby firewall does not admit SSH from the runner")
	}
	for _, family := range []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified()} {
		if !firewallAllows(fw.Rules, hcloud.FirewallRuleProtocolTCP, 443, family, true) ||
			!firewallAllows(fw.Rules, hcloud.FirewallRuleProtocolUDP, 3478, family, true) {
			return fmt.Errorf("the standby firewall must admit public TCP 443 and UDP 3478 over IPv4 and IPv6")
		}
	}
	for _, id := range config.SSHKeyIDs {
		key, _, err := client.SSHKey.GetByID(ctx, id)
		if err != nil {
			return fmt.Errorf("read pinned standby SSH key %d: %w", id, err)
		}
		if key == nil || key.ID != id || key.PublicKey == "" {
			return fmt.Errorf("pinned standby SSH key %d is missing", id)
		}
	}
	return nil
}

func firewallAllows(rules []hcloud.FirewallRule, protocol hcloud.FirewallRuleProtocol, port int, addr netip.Addr, public bool) bool {
	for _, r := range rules {
		if r.Direction != hcloud.FirewallRuleDirectionIn || r.Protocol != protocol || !coversPort(r.Port, port) {
			continue
		}
		if slices.ContainsFunc(r.SourceIPs, func(source net.IPNet) bool {
			prefix, err := netip.ParsePrefix(source.String())
			return err == nil && prefix.Contains(addr) && (!public || prefix.Bits() == 0)
		}) {
			return true
		}
	}
	return false
}

func coversPort(value *string, port int) bool {
	if value == nil || *value == "any" {
		return true
	}
	first, last, hasRange := strings.Cut(*value, "-")
	low, err := strconv.Atoi(first)
	if err != nil {
		return false
	}
	if !hasRange {
		return low == port
	}
	high, err := strconv.Atoi(last)
	return err == nil && low <= port && port <= high
}

func readStandbyCatalog(ctx context.Context, client *hcloud.Client, imageName string) ([]StandbyCandidate, error) {
	var available []StandbyCandidate
	types := map[string]*hcloud.ServerType{}
	images := map[hcloud.Architecture]*hcloud.Image{}
	for _, candidate := range standbyCandidates {
		kind, seen := types[candidate.serverType]
		if !seen {
			var err error
			kind, _, err = client.ServerType.GetByName(ctx, candidate.serverType)
			if err != nil {
				return nil, fmt.Errorf("read server type %s: %w", candidate.serverType, err)
			}
			types[candidate.serverType] = kind
		}
		if kind == nil || !slices.ContainsFunc(kind.Locations, func(loc hcloud.ServerTypeLocation) bool {
			return loc.Location != nil && loc.Location.Name == candidate.location && loc.Available
		}) {
			continue
		}
		arch, err := releaseArchitecture(kind.Architecture)
		if err != nil {
			return nil, err
		}
		image, seen := images[kind.Architecture]
		if !seen {
			image, _, err = client.Image.GetByNameAndArchitecture(ctx, imageName, kind.Architecture)
			if err != nil {
				return nil, fmt.Errorf("read standby image %s for %s: %w", imageName, arch, err)
			}
			images[kind.Architecture] = image
		}
		if image != nil && image.ID > 0 && image.Name == imageName && image.Architecture == kind.Architecture && image.Status == hcloud.ImageStatusAvailable {
			available = append(available, StandbyCandidate{ServerType: candidate.serverType, Location: candidate.location, Architecture: arch})
		}
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("the provider catalog has no supported standby candidate with image %s", imageName)
	}
	return available, nil
}

func releaseArchitecture(arch hcloud.Architecture) (string, error) {
	switch arch {
	case hcloud.ArchitectureX86:
		return "amd64", nil
	case hcloud.ArchitectureARM:
		return "arm64", nil
	default:
		return "", fmt.Errorf("the provider reported unsupported standby architecture %q", arch)
	}
}
