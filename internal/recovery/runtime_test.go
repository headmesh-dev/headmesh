package recovery

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRuntimeConditionalPublicationAndStaging(t *testing.T) {
	for _, tool := range []string{"curl", "age", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for runtime snapshot verification", tool)
		}
	}
	private := t.TempDir()
	src := RuntimeSources{EtcDir: filepath.Join(private, "etc"), NoiseKey: filepath.Join(private, "noise.key")}
	writeRuntimeFixture(t, src.NoiseKey, []byte("privkey:"+strings.Repeat("ac", 32)))
	manifest := validManifest()
	var err error
	manifest.Headscale.NoiseKeySHA256, err = machinePublicHash(src.NoiseKey)
	if err != nil {
		t.Fatal(err)
	}
	var identities, recipients []string
	for _, role := range []string{"owner", "recovery", "unrelated"} {
		identity := filepath.Join(private, role+".agekey")
		if err := exec.CommandContext(t.Context(), "age-keygen", "-o", identity).Run(); err != nil {
			t.Fatal(err)
		}
		recipient, err := exec.CommandContext(t.Context(), "age-keygen", "-y", identity).Output()
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
		recipients = append(recipients, strings.TrimSpace(string(recipient)))
	}
	var mu sync.Mutex
	var stored []byte
	var rejectWrite bool
	objectPath := runtimeObjectPath(manifest.Headscale.NoiseKeySHA256)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != objectPath {
			t.Errorf("unexpected runtime object %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"fixture-runtime"`)
		case http.MethodPut:
			condition := r.Header.Get("If-None-Match") == "*"
			if stored != nil {
				condition = r.Header.Get("If-Match") == `"fixture-runtime"`
			}
			if !condition {
				t.Error("runtime put omitted its write condition")
			}
			if !condition || rejectWrite {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || !bytes.HasPrefix(body, []byte("age-encryption.org/v1\n")) || bytes.Contains(body, []byte("fixture-client-credential")) {
				t.Errorf("invalid runtime ciphertext: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			stored = body
		case http.MethodGet:
			if _, err := w.Write(stored); err != nil {
				t.Error(err)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(private, "fixture.crt")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	writeRuntimeFixture(t, ca, certificate)
	t.Setenv("CURL_CA_BUNDLE", ca)
	keyBytes, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	files := runtimeFixtureFiles(certificate, key)
	for name, data := range files {
		writeRuntimeFixture(t, filepath.Join(src.EtcDir, name), data)
	}
	netrc := filepath.Join(private, "runtime.netrc")
	writeRuntimeFixture(t, netrc, []byte("machine 127.0.0.1 login fixture password fixture\n"))
	for range 2 {
		if err := PublishRuntime(t.Context(), src, recipients[:2], server.URL, netrc); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range identities[:2] {
		stage := filepath.Join(private, filepath.Base(identity)+".stage")
		opt := RuntimeStage{Identity: identity, Netrc: netrc, Endpoint: server.URL, OutputDir: stage}
		if err := StageRuntime(t.Context(), manifest, opt); err != nil {
			t.Fatal(err)
		}
		for name, want := range files {
			path := filepath.Join(stage, name)
			got, err := readBundleFile(path, runtimeFileLimit)
			if err != nil || !bytes.Equal(want, got) {
				t.Fatalf("staged %s differs: %v", name, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("staged %s must be private: %v", name, err)
			}
		}
		if err := StageRuntime(t.Context(), manifest, opt); !errors.Is(err, os.ErrExist) {
			t.Fatalf("stage overwrote an existing output: %v", err)
		}
	}
	mu.Lock()
	local := filepath.Join(private, "runtime.age")
	localCiphertext := bytes.Clone(stored)
	rejectWrite = true
	mu.Unlock()
	writeRuntimeFixture(t, local, localCiphertext)
	if err := PublishRuntime(t.Context(), src, recipients[:2], server.URL, netrc); err == nil || !strings.Contains(err.Error(), "HTTP 412") {
		t.Fatalf("runtime upload conflict error = %v", err)
	}
	wrong := validManifest()
	for _, tc := range []struct {
		identity string
		manifest Manifest
		want     string
	}{
		{identities[0], wrong, "different Noise identity"},
		{identities[2], manifest, "unlock runtime snapshot"},
	} {
		stage := filepath.Join(private, "refused")
		err := StageRuntime(t.Context(), tc.manifest, RuntimeStage{Bundle: local, Identity: tc.identity, OutputDir: stage})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("stage err = %v, want %q", err, tc.want)
		}
		if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused stage left files behind: %v", err)
		}
	}
	if err := os.Remove(filepath.Join(src.EtcDir, "headmesh/oauth/fixture-client-secret")); err != nil {
		t.Fatal(err)
	}
	if err := PackRuntime(t.Context(), src, recipients[:2], io.Discard); err == nil || !strings.Contains(err.Error(), "fixture-client-secret") {
		t.Fatalf("pack accepted a missing configured credential: %v", err)
	}
}

func runtimeFixtureFiles(certificate, key []byte) map[string][]byte {
	files := make(map[string][]byte)
	for _, name := range runtimeRequired {
		files[name] = []byte("fixture\n")
	}
	seed := bytes.Repeat([]byte{0xcd}, ed25519.SeedSize)
	public := ed25519.NewKeyFromSeed(seed)[ed25519.SeedSize:]
	files["headmesh/prod/backend/op-signing.key"] = []byte(base64.StdEncoding.EncodeToString(seed))
	files["headmesh-cellagent/cellagent.json"] = []byte(`{"client_cert":"/etc/headmesh-cellagent/client.crt","client_key":"/etc/headmesh-cellagent/client.key","server_keys":["` + base64.StdEncoding.EncodeToString(public) + `"]}`)
	files["headmesh/prod/backend.env"] = []byte("HEADMESH_GITHUB_CLIENT_SECRET_FILE='/etc/headmesh/oauth/fixture-client-secret'\n")
	files["headmesh/oauth/fixture-client-secret"] = []byte("fixture-client-credential\n")
	files["headmesh/tls/fullchain.pem"], files["headmesh-cellagent/client.crt"] = certificate, certificate
	files["headmesh/tls/privkey.pem"], files["headmesh-cellagent/client.key"] = key, key
	return files
}

func writeRuntimeFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
