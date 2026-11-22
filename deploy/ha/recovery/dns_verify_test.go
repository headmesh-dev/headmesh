package recovery

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestVerifyCutoverIsReadOnlyAndTiedToCurrentProviders(t *testing.T) {
	for _, tc := range []struct {
		name, want          string
		incomplete, noProof bool
		duringProof         func(*standbyAPI, *cloudflareFixture)
	}{
		{name: "verified promotion"},
		{name: "batch never applied", incomplete: true, noProof: true, want: "cutover is incomplete"},
		{name: "fence changed", want: "fence", duringProof: func(h *standbyAPI, _ *cloudflareFixture) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.primary.status = "running"
		}},
		{name: "zone changed", want: "records changed", duringProof: func(_ *standbyAPI, c *cloudflareFixture) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.records[0].Proxied = new(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cf, target := dnsCutoverFixture(t)
			if !tc.incomplete {
				for i := range cf.records {
					if cf.records[i].Type == dnsA {
						cf.records[i].Content = "192.0.2.91"
						if cf.records[i].Name == cellHost {
							cf.records[i].TTL = cellTTL
						}
					}
				}
				cf.records = append(cf.records, cfFixtureRecord{
					ID: "hs1-aaaa", Name: cellHost, Type: dnsAAAA, Content: "2001:db8:91::1", TTL: cellTTL, Proxied: new(false),
				})
			}
			called := false
			err := verifyCutover(t.Context(), h.client(t), cf.client(t), target, strings.Repeat("ab", 32),
				func(_ context.Context, records []dnsRecord, addresses promotionAddresses, hash []byte) error {
					called = true
					if len(records) != 5 || addresses.standbyV4.String() != "192.0.2.91" || addresses.standbyV6.String() != "2001:db8:91::1" ||
						len(hash) != sha256.Size || hash[0] != 0xab {
						t.Fatalf("incorrect public proof inputs: %d records, %+v, %x", len(records), addresses, hash)
					}
					if tc.duringProof != nil {
						tc.duringProof(h, cf)
					}
					return nil
				})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("verify error = %v, want %q", err, tc.want)
			}
			if called == tc.noProof || cf.writes() != 0 {
				t.Fatalf("public proof called = %v, Cloudflare batches = %d", called, cf.writes())
			}
		})
	}
}

func TestPublicCutoverProbes(t *testing.T) {
	const api = "api.headmesh.dev"
	const www = "www.headmesh.dev"
	standby := promotionAddresses{
		oldIPv4: netip.MustParseAddr("192.0.2.71"), oldIPv6: netip.MustParsePrefix("2001:db8:71::/64"),
		standbyV4: netip.MustParseAddr("192.0.2.91"), standbyV6: netip.MustParseAddr("2001:db8:91::1"),
	}
	edge := netip.MustParseAddr("198.51.100.30")
	key := "mkey:" + strings.Repeat("ab", 32)
	hash := sha256.Sum256([]byte(key))
	for _, tc := range []struct {
		name, want                                        string
		answer                                            func(map[string][]netip.Addr)
		cached, missingRay, wrongNoise, noV6TLS, noV6STUN bool
	}{
		{name: "four public hostnames and both cell addresses"},
		{name: "cached old IPv6 alongside current pair", want: "DNS answers for hs1.headmesh.dev differ", answer: func(answers map[string][]netip.Addr) {
			answers[cellHost] = append(answers[cellHost], standby.oldIPv6.Addr().Next())
		}},
		{name: "missing cell IPv6", want: "DNS answers for hs1.headmesh.dev differ", answer: func(answers map[string][]netip.Addr) {
			answers[cellHost] = []netip.Addr{standby.standbyV4}
		}},
		{name: "site bypasses proxy", want: "resolves to a direct origin", answer: func(answers map[string][]netip.Addr) {
			answers[siteHost] = []netip.Addr{standby.standbyV4}
		}},
		{name: "DNS-only API has stale origin", want: "DNS answers for api.headmesh.dev differ", answer: func(answers map[string][]netip.Addr) {
			answers[api] = []netip.Addr{standby.oldIPv4}
		}},
		{name: "cached API readiness", want: "cached readiness", cached: true},
		{name: "site response missing Cloudflare header", want: "missing its Cloudflare Ray ID", missingRay: true},
		{name: "different Noise identity", want: "Noise identity differs", wrongNoise: true},
		{name: "IPv6 HTTPS route down", want: "via resolved address 2001:db8:91::1", noV6TLS: true},
		{name: "IPv6 STUN response missing", want: "STUN via resolved address 2001:db8:91::1", noV6STUN: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := map[string][]netip.Addr{
				api: {standby.standbyV4}, siteHost: {edge}, www: {edge}, cellHost: {standby.standbyV6, standby.standbyV4, standby.standbyV4},
			}
			if tc.answer != nil {
				tc.answer(answers)
			}
			records := []dnsRecord{
				{name: api, kind: dnsA, content: standby.standbyV4.String()},
				{name: siteHost, kind: dnsA, content: standby.standbyV4.String(), proxied: true},
				{name: www, kind: dnsCNAME, content: siteHost},
				{name: cellHost, kind: dnsA, content: standby.standbyV4.String()},
				{name: cellHost, kind: dnsAAAA, content: standby.standbyV6.String()},
			}
			cert, roots := ingressFixtureCertificate(t, cutoverNames[:])
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS.ServerName != r.Host {
					t.Errorf("SNI = %s, host = %s", r.TLS.ServerName, r.Host)
				}
				if (r.Host == siteHost || r.Host == www) && !tc.missingRay {
					w.Header().Set("CF-Ray", "fixture-edge")
				}
				switch r.URL.Path {
				case "/health", "/derp/probe":
				case "/key":
					served := key
					if tc.wrongNoise {
						served = "mkey:" + strings.Repeat("ba", 32)
					}
					_, _ = fmt.Fprintf(w, `{"publicKey":%q}`, served)
				case "/readyz", "/api/waitlist/readyz":
					if r.URL.Query().Get("recovery_probe") == "" || r.Header.Get("Cache-Control") != "no-cache" {
						t.Error("readiness request omitted the cache controls")
					}
					if tc.cached {
						w.Header().Set("CF-Cache-Status", "HIT")
					}
					w.WriteHeader(http.StatusNoContent)
				case "/api/waitlist":
					w.WriteHeader(http.StatusMethodNotAllowed)
				case "/":
					if r.Host == www {
						http.Redirect(w, r, "https://headmesh.dev/", http.StatusMovedPermanently)
						return
					}
					_, _ = fmt.Fprint(w, "<title>Headmesh</title>")
				default:
					t.Errorf("unexpected request %s %s", r.Host, r.URL)
				}
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			t.Cleanup(server.Close)
			var stunAddresses []netip.AddrPort
			probes := publicProbes{
				lookup: func(_ context.Context, network, name string) ([]netip.Addr, error) {
					if network != "ip" || !strings.HasSuffix(name, ".") {
						t.Errorf("lookup must query both families and use an absolute DNS name: %s %s", network, name)
					}
					return answers[strings.TrimSuffix(name, ".")], nil
				},
				dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					if tc.noV6TLS && address == "[2001:db8:91::1]:443" {
						return nil, errors.New("fixture has no IPv6 HTTPS")
					}
					switch address {
					case "192.0.2.91:443", "[2001:db8:91::1]:443", "198.51.100.30:443":
						return new(net.Dialer).DialContext(ctx, network, server.Listener.Addr().String())
					default:
						return nil, fmt.Errorf("unexpected dial address %s", address)
					}
				},
				stun: func(_ context.Context, addr netip.AddrPort) error {
					stunAddresses = append(stunAddresses, addr)
					if tc.noV6STUN && addr.Addr().Is6() {
						return errors.New("fixture has no IPv6 STUN")
					}
					return nil
				},
				roots: roots,
			}
			err := probes.verify(t.Context(), records, standby, hash[:])
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("probe error = %v, want %q", err, tc.want)
				}
				return
			}
			expected := []netip.AddrPort{netip.AddrPortFrom(standby.standbyV4, 3478), netip.AddrPortFrom(standby.standbyV6, 3478)}
			if err != nil || !slices.Equal(stunAddresses, expected) {
				t.Fatalf("STUN checks = %v, public proof error = %v", stunAddresses, err)
			}
		})
	}
}
