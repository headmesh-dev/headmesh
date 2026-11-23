package recovery

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrepareInputsKeepsOnlyVerifiedPinnedArtifacts(t *testing.T) {
	for _, tool := range []string{"age", "age-keygen", "curl", "sqlite3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for the recovery input test", tool)
		}
	}
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	objects := make(map[string][]byte)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.URL.Path)
		credential := "fixture-keys"
		if r.URL.Path == manifestObjectPath {
			credential = "fixture-manifest"
		}
		if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Authorization"), "Credential="+credential+"/") {
			t.Error("recovery preparation must use the correct scoped read credentials")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, ok := objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	writeRuntimeFixture(t, filepath.Join(private, "test-ca.crt"), ca)
	t.Setenv("CURL_CA_BUNDLE", filepath.Join(private, "test-ca.crt"))
	writeRuntimeFixture(t, filepath.Join(private, "manifest.netrc"), []byte("machine 127.0.0.1 login fixture-manifest password fixture\n"))
	writeRuntimeFixture(t, filepath.Join(private, "keys.netrc"), []byte("machine 127.0.0.1 login fixture-keys password fixture\n"))
	var recipients []string
	for _, role := range []string{"owner", "recovery"} {
		path := filepath.Join(private, role+".agekey")
		if err := exec.CommandContext(t.Context(), "age-keygen", "-o", path).Run(); err != nil {
			t.Fatal(err)
		}
		pub, err := exec.CommandContext(t.Context(), "age-keygen", "-y", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, strings.TrimSpace(string(pub)))
	}

	src := RuntimeSources{EtcDir: filepath.Join(private, "etc"), NoiseKey: filepath.Join(private, "noise.key")}
	writeRuntimeFixture(t, src.NoiseKey, []byte("privkey:"+strings.Repeat("8a", 32)))
	derp := filepath.Join(private, "derp.key")
	writeRuntimeFixture(t, derp, []byte("privkey:"+strings.Repeat("bc", 32)))
	config := filepath.Join(private, "config.yaml")
	writeRuntimeFixture(t, config, []byte("server_url: https://fixture.invalid\n"))
	keyBinary, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBinary})
	for name, data := range runtimeFixtureFiles(ca, key) {
		writeRuntimeFixture(t, filepath.Join(src.EtcDir, name), data)
	}
	manifest := validManifest()
	manifest.CapturedAt = time.Now().UTC().Add(-time.Minute)
	manifest.Headscale.NoiseKeySHA256, err = machinePublicHash(src.NoiseKey)
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var keyBundle, runtimeBundle bytes.Buffer
	if err := PackKeyBundle(t.Context(), KeyBundleSources{NoiseKey: src.NoiseKey, DERPKey: derp, Configuration: config}, recipients, "age", &keyBundle); err != nil {
		t.Fatal(err)
	}
	if err := PackRuntime(t.Context(), src, recipients, &runtimeBundle); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(private, "waitlist.db")
	if err := exec.CommandContext(t.Context(), "sqlite3", db, `CREATE TABLE schema_migrations (name TEXT);
INSERT INTO schema_migrations VALUES ('0001_create_waitlist.sql');
CREATE TABLE waitlist (id TEXT, email TEXT, consent_version TEXT, confirmed_at TEXT, unsubscribed_at TEXT);`).Run(); err != nil {
		t.Fatal(err)
	}
	waitlist, err := exec.CommandContext(t.Context(), "age", "-e", "-r", recipients[0], "-r", recipients[1], db).Output()
	if err != nil {
		t.Fatal(err)
	}
	keyPath, runtimePath := keyObjectPath(manifest.Headscale.NoiseKeySHA256), runtimeObjectPath(manifest.Headscale.NoiseKeySHA256)
	mu.Lock()
	objects[manifestObjectPath], objects[keyPath], objects[runtimePath], objects[waitlistObjectPath] = manifestJSON, keyBundle.Bytes(), runtimeBundle.Bytes(), waitlist
	mu.Unlock()
	opt := PrepareOptions{Endpoint: server.URL, Credentials: private, OutputDir: filepath.Join(private, "prepared")}
	report, err := PrepareInputs(t.Context(), opt)
	if err != nil || !report.ManifestCapturedAt.Equal(manifest.CapturedAt) || report.ManifestAgeSeconds < 60 {
		t.Fatalf("prepare returned %+v, %v", report, err)
	}
	entries, err := os.ReadDir(opt.OutputDir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("prepared directory contains unexpected entries: %v, %v", entries, err)
	}
	for file, want := range map[string][]byte{
		"manifest.json": manifestJSON, "headscale.age": keyBundle.Bytes(), "runtime.age": runtimeBundle.Bytes(), "waitlist.db.age": waitlist,
	} {
		path := filepath.Join(opt.OutputDir, file)
		got, err := readBundleFile(path, maxCiphertext)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("prepared input %s differs from the bytes verified: %v", file, err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode() != 0o600 {
			t.Fatalf("prepared input %s is not private: %v", file, err)
		}
	}
	if _, err := PrepareInputs(t.Context(), opt); err == nil || !strings.Contains(err.Error(), "must not already exist") {
		t.Fatalf("retried into an existing prepared directory: %v", err)
	}
	mu.Lock()
	if len(requests) != 4 || requests[0] != manifestObjectPath {
		t.Errorf("expected one manifest and three artifact GETs, got %v", requests)
	}
	delete(objects, waitlistObjectPath)
	mu.Unlock()
	opt.OutputDir = filepath.Join(private, "missing")
	if _, err := PrepareInputs(t.Context(), opt); err == nil || !strings.Contains(err.Error(), "waitlist/prod.db.age (HTTP 404)") {
		t.Fatalf("missing waitlist error = %v", err)
	}
	mu.Lock()
	objects[waitlistObjectPath] = bytes.Clone(waitlist)
	objects[waitlistObjectPath][len(waitlist)-1] ^= 1
	mu.Unlock()
	opt.OutputDir = filepath.Join(private, "corrupt")
	if _, err := PrepareInputs(t.Context(), opt); err == nil || !strings.Contains(err.Error(), "decrypt waitlist snapshot") {
		t.Fatalf("corrupt waitlist error = %v", err)
	}
	for _, pattern := range []string{"missing", "corrupt", ".headmesh-prepare-*"} {
		files, err := filepath.Glob(filepath.Join(private, pattern))
		if err != nil || len(files) != 0 {
			t.Fatalf("failed preparation left private files: %v, %v", files, err)
		}
	}
	if _, err := os.Stat(filepath.Join(opt.OutputDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preparation unexpectedly released a manifest: %v", err)
	}
}
