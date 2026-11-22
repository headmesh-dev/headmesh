package recovery

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestProveStandbyIngress(t *testing.T) {
	const hs = "hs1.headmesh.dev"
	const api = "api.headmesh.dev"
	const site = "headmesh.dev"
	key := "mkey:" + strings.Repeat("ab", 32)
	fingerprint := sha256.Sum256([]byte(key))
	target := StandbyIngress{
		IPv4: netip.MustParseAddr("192.0.2.22"), IPv6: netip.MustParseAddr("2001:db8::22"),
		NoiseKeySHA256: hex.EncodeToString(fingerprint[:]),
	}
	for _, test := range []struct {
		name, want  string
		certNames   []string
		untrusted   bool
		breakIPv6   bool
		wrongNoise  bool
		redirect    bool
		apiNotReady bool
	}{
		{name: "both standby addresses serve the restored services"},
		{name: "wrong certificate host", certNames: []string{api, site}, want: "certificate is valid for"},
		{name: "untrusted certificate", untrusted: true, want: "unknown authority"},
		{name: "wrong Noise identity", wrongNoise: true, want: "Noise identity differs"},
		{name: "refuse an off-host redirect", redirect: true, want: "HTTP 302"},
		{name: "backend still recovering", apiNotReady: true, want: "HTTP 503"},
		{name: "IPv6 ingress absent", breakIPv6: true, want: "2001:db8::22"},
	} {
		t.Run(test.name, func(t *testing.T) {
			names := []string{hs, api, site}
			if len(test.certNames) > 0 {
				names = test.certNames
			}
			cert, roots := ingressFixtureCertificate(t, names)
			if test.untrusted {
				roots = nil
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS.ServerName != r.Host {
					t.Errorf("SNI %q differs from the host %q", r.TLS.ServerName, r.Host)
				}
				switch r.Host + r.URL.RequestURI() {
				case hs + "/health":
					if test.redirect {
						http.Redirect(w, r, "https://unrelated.invalid/health", http.StatusFound)
					}
				case hs + "/key?v=130":
					served := key
					if test.wrongNoise {
						served = "mkey:" + strings.Repeat("ba", 32)
					}
					_, _ = fmt.Fprintf(w, `{"publicKey":%q}`, served)
				case api + "/readyz":
					if test.apiNotReady {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case site + "/":
					_, _ = fmt.Fprint(w, "<!doctype html><title>Headmesh</title>")
				default:
					t.Errorf("unrecognized probe: %s %s", r.Host, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			t.Cleanup(server.Close)
			dialer := new(net.Dialer)
			err := proveIngress(t.Context(), target, roots, func(ctx context.Context, network, address string) (net.Conn, error) {
				switch address {
				case "192.0.2.22:443":
				case "[2001:db8::22]:443":
					if test.breakIPv6 {
						return nil, fmt.Errorf("fixture IPv6 path is unavailable")
					}
				default:
					return nil, fmt.Errorf("unexpected dial target %q", address)
				}
				return dialer.DialContext(ctx, network, server.Listener.Addr().String())
			})
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted error containing %q, got %v", test.want, err)
			}
		})
	}
}

func ingressFixtureCertificate(t *testing.T, names []string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		DNSNames: names, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
