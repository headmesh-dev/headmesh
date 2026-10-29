package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// selfSigned writes a certificate and key for one DNS name to dir and returns their paths.
func selfSigned(t *testing.T, dir, name string, usage x509.ExtKeyUsage) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// An agent on the server's own host dials loopback; server_name makes it verify the public name anyway.
func TestServerTLSVerifiesServerNameWhenDialingAnAddress(t *testing.T) {
	dir := t.TempDir()
	serverCert, serverKey := selfSigned(t, dir, "api.example.test", x509.ExtKeyUsageServerAuth)
	clientCert, clientKey := selfSigned(t, dir, "hs1", x509.ExtKeyUsageClientAuth)
	pair, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).HandshakeContext(t.Context())
			_ = c.Close()
		}
	}()

	dial := func(serverName string) error {
		cfg := config{ClientCert: clientCert, ClientKey: clientKey, ServerCA: serverCert, ServerName: serverName}
		tlsCfg, err := serverTLS(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if tlsCfg.ServerName == "" {
			// What http.Transport does when server_name is empty: verify the host it dialed.
			host, _, _ := net.SplitHostPort(ln.Addr().String())
			tlsCfg.ServerName = host
		}
		conn, err := (&tls.Dialer{Config: tlsCfg}).DialContext(t.Context(), "tcp", ln.Addr().String())
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if err := dial("api.example.test"); err != nil {
		t.Fatalf("with server_name: %v", err)
	}
	if err := dial(""); err == nil {
		t.Fatal("without server_name the loopback address must not match a certificate for api.example.test")
	}
	if err := dial("other.example.test"); err == nil {
		t.Fatal("a server_name the certificate doesn't cover must fail")
	}
}

func TestLoadConfigAcceptsServerName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cellagent.json")
	body := `{"cell_name":"hs1","server_url":"https://127.0.0.1:7443","server_name":"api.headmesh.dev",` +
		`"server_keys":["AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "api.headmesh.dev" {
		t.Fatalf("server_name = %q", cfg.ServerName)
	}
}
