package oauth

import (
	"context"
	"net/netip"
	"testing"
)

func TestCIMDDialNeverConnectsToPrivateServices(t *testing.T) {
	for _, address := range []string{"127.0.0.1:443", "100.64.0.7:443", "[::1]:443", "169.254.169.254:80"} {
		conn, err := publicDial(context.Background(), "tcp", address)
		if err == nil || conn != nil {
			t.Fatalf("private service was dialed: %s", address)
		}
	}
}

func TestCIMDRejectsInternalAndTransitionAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.64.0.7", "::ffff:100.64.0.7", "::1", "fd7a:115c:a1e0::1", "fe80::1", "64:ff9b::a00:1", "2002:a00:1::1", "198.18.0.1", "0.0.0.0"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("private metadata address accepted: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("public address refused: %s", raw)
		}
	}
}

func TestRedirectsAreExactAndOwned(t *testing.T) {
	for _, raw := range []string{"https://evil.test/callback", "https://chatgpt.com.evil.test/callback", "https://user@chatgpt.com/callback", "http://chatgpt.com/callback", "https://chatgpt.com:444/callback", "https://chatgpt.com/*", "https://chatgpt.com/callback#fragment"} {
		if validRedirect(raw, "") {
			t.Errorf("untrusted redirect accepted %s", raw)
		}
	}
	for _, raw := range []string{"https://chatgpt.com/connector_platform/oauth/redirect", "https://claude.ai/api/mcp/auth_callback", "http://localhost:54321/callback", "http://127.0.0.1/callback"} {
		if !validRedirect(raw, "") {
			t.Errorf("supported redirect refused %s", raw)
		}
	}
	if !validRedirect("https://client.test/callback", "https://client.test/metadata.json") {
		t.Fatal("CIMD cannot use own origin")
	}
	if validRedirect("https://client.test.evil.test/callback", "https://client.test/metadata.json") {
		t.Fatal("CIMD accepted foreign origin")
	}
}
