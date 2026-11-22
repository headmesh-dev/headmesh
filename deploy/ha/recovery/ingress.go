package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// StandbyIngress ties the proof to the standby's assigned addresses and the
// restored Headscale identity in the recovery manifest.
type StandbyIngress struct {
	IPv4, IPv6     netip.Addr
	NoiseKeySHA256 string
}

// ProveIngress verifies HTTPS and native STUN on both standby addresses. The
// public hostnames are used for certificate verification, SNI and HTTP routing.
func ProveIngress(ctx context.Context, standby StandbyIngress) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if err := proveIngress(ctx, standby, nil, dialer.DialContext); err != nil {
		return err
	}
	for _, addr := range []netip.Addr{standby.IPv4, standby.IPv6} {
		if err := probeSTUN(ctx, netip.AddrPortFrom(addr, 3478)); err != nil {
			return fmt.Errorf("verify STUN on standby address %s: %w", addr, err)
		}
	}
	return nil
}

func proveIngress(ctx context.Context, standby StandbyIngress, roots *x509.CertPool, dial func(context.Context, string, string) (net.Conn, error)) error {
	if !standby.IPv4.Is4() || !standby.IPv6.Is6() || standby.IPv6.Is4In6() || standby.IPv6.Zone() != "" ||
		!standby.IPv4.IsGlobalUnicast() || !standby.IPv6.IsGlobalUnicast() {
		return fmt.Errorf("the standby must have an IPv4 and an IPv6 unicast address")
	}
	hash, err := hex.DecodeString(standby.NoiseKeySHA256)
	if err != nil || len(hash) != sha256.Size {
		return fmt.Errorf("invalid Headscale Noise fingerprint in the recovery manifest")
	}
	checks := []struct {
		url    string
		status int
		body   func([]byte) error
	}{
		{"https://hs1.headmesh.dev/health", http.StatusOK, nil},
		{"https://hs1.headmesh.dev/key?v=130", http.StatusOK, func(body []byte) error { return checkNoiseKey(body, hash) }},
		{"https://hs1.headmesh.dev/derp/probe", http.StatusOK, nil},
		{"https://api.headmesh.dev/readyz", http.StatusNoContent, nil},
		{"https://headmesh.dev/", http.StatusOK, checkHeadmeshSite},
	}
	for _, address := range []netip.Addr{standby.IPv4, standby.IPv6} {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx, "tcp", netip.AddrPortFrom(address, 443).String())
			},
		}
		client := &http.Client{
			Transport:     transport,
			Timeout:       8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		for _, check := range checks {
			if err := probeHTTPS(ctx, client, check.url, check.status, check.body); err != nil {
				transport.CloseIdleConnections()
				return fmt.Errorf("verify %s on standby address %s: %w", check.url, address, err)
			}
		}
		transport.CloseIdleConnections()
	}
	return nil
}

func probeHTTPS(ctx context.Context, client *http.Client, url string, status int, checkBody func([]byte) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create HTTPS probe: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request standby HTTPS endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != status {
		return fmt.Errorf("received HTTP %d, expected %d", resp.StatusCode, status)
	}
	if checkBody == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return fmt.Errorf("read HTTPS response: %w", err)
	}
	return checkBody(body)
}

func checkNoiseKey(body, expected []byte) error {
	var result struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode Headscale key response: %w", err)
	}
	key, ok := strings.CutPrefix(result.PublicKey, "mkey:")
	raw, err := hex.DecodeString(key)
	if !ok || err != nil || len(raw) != 32 {
		return fmt.Errorf("the Headscale endpoint did not return a machine public key")
	}
	fingerprint := sha256.Sum256([]byte(result.PublicKey))
	if !bytes.Equal(fingerprint[:], expected) {
		return fmt.Errorf("the served Noise identity differs from the recovery manifest")
	}
	return nil
}

func checkHeadmeshSite(body []byte) error {
	if !bytes.Contains(bytes.ToLower(body), []byte("<title>headmesh")) {
		return fmt.Errorf("the apex did not return the Headmesh site")
	}
	return nil
}
