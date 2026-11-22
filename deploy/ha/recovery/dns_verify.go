package recovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// VerifyCutover proves the promoted records and public ingress from the runner.
// It reads Hetzner and Cloudflare before and after the network probes.
func VerifyCutover(ctx context.Context, hetzner *hcloud.Client, dns *CloudflareDNS, target DNSCutover, noiseHash string) error {
	resolver := &net.Resolver{PreferGo: true, StrictErrors: true}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	probes := publicProbes{lookup: resolver.LookupNetIP, dial: dialer.DialContext, stun: probeSTUN}
	return verifyCutover(ctx, hetzner, dns, target, noiseHash, probes.verify)
}

func verifyCutover(ctx context.Context, hetzner *hcloud.Client, dns *CloudflareDNS, target DNSCutover, noiseHash string,
	prove func(context.Context, []dnsRecord, promotionAddresses, []byte) error,
) error {
	hash, err := hex.DecodeString(noiseHash)
	if err != nil || len(hash) != sha256.Size {
		return fmt.Errorf("cutover verification requires the Noise fingerprint from the verified recovery manifest")
	}
	addresses, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return err
	}
	records, err := dns.readManagedRecords(ctx)
	if err != nil {
		return err
	}
	changes, _, err := planDNS(records, addresses)
	if err != nil {
		return err
	}
	if len(changes) != 0 {
		return fmt.Errorf("the Cloudflare cutover is incomplete for %s %s", changes[0].Name, changes[0].Type)
	}
	if err := prove(ctx, records, addresses, hash); err != nil {
		return fmt.Errorf("public cutover proof failed: %w", err)
	}
	checked, err := readPromotionAddresses(ctx, hetzner, target)
	if err != nil {
		return err
	}
	if checked != addresses {
		return fmt.Errorf("primary or standby addresses changed during public verification")
	}
	confirmed, err := dns.readManagedRecords(ctx)
	if err != nil {
		return err
	}
	if !matchesDNSRecords(confirmed, records) {
		return fmt.Errorf("the Cloudflare records changed during public verification")
	}
	return nil
}

type publicProbes struct {
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	stun   func(context.Context, netip.AddrPort) error
	roots  *x509.CertPool
}

type publicRoute struct {
	addresses []netip.Addr
	proxied   bool
}

func (p publicProbes) verify(ctx context.Context, records []dnsRecord, standby promotionAddresses, hash []byte) error {
	routes := make(map[string]publicRoute, len(cutoverNames))
	for _, name := range cutoverNames {
		answer, err := p.lookup(ctx, "ip", name+".")
		if err != nil {
			return fmt.Errorf("resolve %s from the recovery runner: %w", name, err)
		}
		route, err := verifyDNSAnswer(name, answer, records, standby)
		if err != nil {
			return err
		}
		routes[name] = route
	}
	checks := ingressProbes(hash)
	nonce := rand.Text()
	for _, host := range cutoverNames {
		route := routes[host]
		if host == cellHost {
			for _, addr := range route.addresses {
				if err := p.checkHost(ctx, host, publicRoute{addresses: []netip.Addr{addr}}, checks, nonce); err != nil {
					return fmt.Errorf("via resolved address %s: %w", addr, err)
				}
				if err := p.stun(ctx, netip.AddrPortFrom(addr, 3478)); err != nil {
					return fmt.Errorf("verify %s STUN via resolved address %s: %w", host, addr, err)
				}
			}
			continue
		}
		if err := p.checkHost(ctx, host, route, checks, nonce); err != nil {
			return err
		}
	}
	return nil
}

func verifyDNSAnswer(name string, answer []netip.Addr, records []dnsRecord, standby promotionAddresses) (publicRoute, error) {
	originName := name
	proxied := false
	for _, record := range records {
		if record.name == name && record.kind == dnsCNAME {
			originName, proxied = record.content, record.proxied
			break
		}
	}
	var expected []netip.Addr
	for _, record := range records {
		if record.name == originName {
			addr, err := netip.ParseAddr(record.content)
			if err != nil {
				return publicRoute{}, fmt.Errorf("invalid Cloudflare origin record for %s", originName)
			}
			expected = append(expected, addr)
			proxied = proxied || record.proxied
		}
	}
	if len(answer) == 0 {
		return publicRoute{}, fmt.Errorf("no IP addresses resolved for %s", name)
	}
	for i, addr := range answer {
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.Zone() != "" {
			return publicRoute{}, fmt.Errorf("%s resolved to an address outside public ingress", name)
		}
		if proxied && (addr == standby.standbyV4 || addr == standby.standbyV6 || addr == standby.oldIPv4 || standby.oldIPv6.Contains(addr)) {
			return publicRoute{}, fmt.Errorf("proxied %s still resolves to a direct origin", name)
		}
		answer[i] = addr
	}
	slices.SortFunc(answer, netip.Addr.Compare)
	answer = slices.Compact(answer)
	slices.SortFunc(expected, netip.Addr.Compare)
	if !proxied && !slices.Equal(answer, expected) {
		return publicRoute{}, fmt.Errorf("DNS answers for %s differ from its promoted records: got %v, want %v", name, answer, expected)
	}
	return publicRoute{addresses: answer, proxied: proxied}, nil
}

func (p publicProbes) checkHost(ctx context.Context, host string, route publicRoute, checks []ingressProbe, nonce string) error {
	client := ingressClient(p.roots, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(host, "443") {
			return nil, fmt.Errorf("unexpected HTTPS destination %q", address)
		}
		var dialErr error
		for _, addr := range route.addresses {
			conn, err := p.dial(ctx, network, netip.AddrPortFrom(addr, 443).String())
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, dialErr
	})
	defer client.CloseIdleConnections()
	for _, check := range checks {
		if check.host == host {
			if err := probeHTTPS(ctx, client, check, route.proxied, nonce); err != nil {
				return fmt.Errorf("verify public %s%s: %w", host, check.path, err)
			}
		}
	}
	return nil
}
