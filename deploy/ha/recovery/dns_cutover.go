package recovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// DNSCutover identifies the fenced primary and the running standby to promote.
type DNSCutover struct {
	Primary Primary
	Standby StandbyTarget
}

// PlanDNSCutover previews the updates using read-only provider calls.
func PlanDNSCutover(ctx context.Context, hetzner *hcloud.Client, dns *CloudflareDNS, target DNSCutover) ([]DNSChange, error) {
	addresses, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return nil, err
	}
	current, err := dns.readManagedRecords(ctx)
	if err != nil {
		return nil, err
	}
	changes, _, err := planDNS(current, addresses)
	return changes, err
}

// PromoteDNS verifies both providers and directly proves ingress before
// changing records. The caller holds the recovery lock throughout promotion.
func PromoteDNS(ctx context.Context, hetzner *hcloud.Client, dns *CloudflareDNS, target DNSCutover, noiseHash string) ([]DNSChange, error) {
	return promoteDNS(ctx, hetzner, dns, target, noiseHash, ProveIngress)
}

func promoteDNS(ctx context.Context, hetzner *hcloud.Client, dns *CloudflareDNS, target DNSCutover, noiseHash string,
	prove func(context.Context, StandbyIngress) error,
) ([]DNSChange, error) {
	if fingerprint, err := hex.DecodeString(noiseHash); err != nil || len(fingerprint) != 32 {
		return nil, fmt.Errorf("DNS cutover requires the Noise fingerprint from the verified recovery manifest")
	}
	addresses, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return nil, err
	}
	current, err := dns.readManagedRecords(ctx)
	if err != nil {
		return nil, err
	}
	changes, expected, err := planDNS(current, addresses)
	if err != nil {
		return nil, err
	}
	if err := prove(ctx, StandbyIngress{
		IPv4: addresses.standbyV4, IPv6: addresses.standbyV6, NoiseKeySHA256: noiseHash,
	}); err != nil {
		return nil, fmt.Errorf("standby ingress proof failed before DNS cutover: %w", err)
	}
	beforeWrite, err := dns.readManagedRecords(ctx)
	if err != nil {
		return nil, err
	}
	if !matchesDNSRecords(beforeWrite, current) {
		return nil, fmt.Errorf("the Cloudflare records changed during standby verification")
	}
	checked, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return nil, err
	}
	if addresses != checked {
		return nil, fmt.Errorf("primary or standby address assignments changed during standby verification")
	}
	if len(changes) == 0 {
		return nil, nil
	}
	writeErr := dns.apply(ctx, changes)
	confirmed, err := dns.readManagedRecords(ctx)
	if err != nil {
		return nil, errors.Join(writeErr, fmt.Errorf("read Cloudflare records after attempted DNS cutover: %w", err))
	}
	if !matchesDNSRecords(confirmed, expected) {
		return nil, errors.Join(writeErr, fmt.Errorf("the Cloudflare DNS readback differs from the planned records"))
	}
	afterWrite, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return nil, fmt.Errorf("cutover moved Cloudflare records but provider verification failed: %w", err)
	}
	if afterWrite != checked {
		return nil, fmt.Errorf("cutover moved Cloudflare records but primary or standby address assignments changed")
	}
	return changes, nil
}

func readPromotionAddresses(ctx context.Context, hetzner *hcloud.Client, target DNSCutover) (promotionAddresses, error) {
	server, err := readStandby(ctx, hetzner, target.Primary, target.Standby)
	if err != nil {
		return promotionAddresses{}, err
	}
	if server.Status != hcloud.ServerStatusRunning {
		return promotionAddresses{}, fmt.Errorf("DNS cutover requires a running standby")
	}
	state, err := readPrimary(ctx, hetzner, target.Primary)
	if err != nil {
		return promotionAddresses{}, fmt.Errorf("read primary before DNS cutover: %w", err)
	}
	if err := state.checkFenced(); err != nil {
		return promotionAddresses{}, fmt.Errorf("verify primary fence for DNS cutover: %w", err)
	}
	oldV4, ok := netip.AddrFromSlice(state.ipv4.IP.To4())
	if !ok || !oldV4.IsGlobalUnicast() {
		return promotionAddresses{}, fmt.Errorf("provider returned an invalid IPv4 for the fenced primary")
	}
	var oldV6 netip.Prefix
	if state.ipv6 != nil {
		if state.ipv6.Network == nil {
			return promotionAddresses{}, fmt.Errorf("provider returned no IPv6 network for the fenced primary")
		}
		oldV6, err = netip.ParsePrefix(state.ipv6.Network.String())
		if err != nil || !oldV6.Addr().Is6() || oldV6.Bits() != 64 || !oldV6.Addr().IsGlobalUnicast() {
			return promotionAddresses{}, fmt.Errorf("provider returned an invalid IPv6 network for the fenced primary")
		}
	}
	newV4, _ := netip.AddrFromSlice(server.PublicNet.IPv4.IP.To4())
	newV6, err := netip.ParsePrefix(server.PublicNet.IPv6.Network.String())
	if err != nil {
		return promotionAddresses{}, fmt.Errorf("read standby IPv6 network for cutover: %w", err)
	}
	// A newly assigned Hetzner /64 uses the first address on the standby.
	standbyV6 := newV6.Masked().Addr().Next()
	if oldV4 == newV4 || oldV6.Contains(standbyV6) {
		return promotionAddresses{}, fmt.Errorf("standby and fenced primary address assignments overlap")
	}
	return promotionAddresses{oldIPv4: oldV4, oldIPv6: oldV6, standbyV4: newV4, standbyV6: standbyV6}, nil
}
