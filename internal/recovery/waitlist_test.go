package recovery

import (
	"bytes"
	"encoding/pem"
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

func TestEncryptedWaitlistSnapshotRoundTrip(t *testing.T) {
	for _, tool := range []string{"sqlite3", "curl", "age", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for waitlist recovery verification", tool)
		}
	}
	private := t.TempDir()
	var identities, recipients []string
	for _, role := range []string{"owner", "recovery"} {
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
	var ciphertext []byte
	conflict := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != waitlistObjectPath {
			t.Errorf("unexpected object path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			if len(ciphertext) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"fixture-etag"`)
		case http.MethodPut:
			if (len(ciphertext) == 0 && r.Header.Get("If-None-Match") != "*") ||
				(len(ciphertext) != 0 && r.Header.Get("If-Match") != `"fixture-etag"`) {
				t.Error("the encrypted snapshot write omitted its condition")
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if conflict {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			var err error
			ciphertext, err = io.ReadAll(r.Body)
			if err != nil || !bytes.HasPrefix(ciphertext, []byte("age-encryption.org/v1\n")) || bytes.Contains(ciphertext, []byte("test@example.invalid")) {
				t.Errorf("waitlist object did not contain a private age envelope: %v", err)
				w.WriteHeader(http.StatusBadRequest)
			}
		case http.MethodGet:
			if _, err := w.Write(ciphertext); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected recovery object method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	caFile := filepath.Join(private, "fixture.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURL_CA_BUNDLE", caFile)
	netrc := filepath.Join(private, "fixture.netrc")
	if err := os.WriteFile(netrc, []byte("machine 127.0.0.1 login fixture password fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(private, "source.db")
	sql := `CREATE TABLE schema_migrations (name TEXT);
INSERT INTO schema_migrations VALUES ('0001_create_waitlist.sql');
CREATE TABLE waitlist (id TEXT, email TEXT, consent_version TEXT, confirmed_at TEXT, unsubscribed_at TEXT);
INSERT INTO waitlist VALUES ('fixture', 'test@example.invalid', 'v1', '2026-11-22T00:00:00Z', NULL);`
	if out, err := exec.CommandContext(t.Context(), "sqlite3", "-batch", "-bail", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("create waitlist fixture: %v (%s)", err, out)
	}
	for range 2 { // Both initial creation and ordinary replacement are conditional.
		if err := PublishWaitlist(t.Context(), db, recipients, server.URL, netrc); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range identities {
		staged := filepath.Join(private, filepath.Base(identity)+".db")
		if err := StageWaitlist(t.Context(), staged, identity, server.URL, netrc); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(staged)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("staged snapshot is not a private file: %v", err)
		}
		out, err := exec.CommandContext(t.Context(), "sqlite3", "-readonly", staged, "SELECT count(*) FROM waitlist WHERE confirmed_at IS NOT NULL;").Output()
		if err != nil || strings.TrimSpace(string(out)) != "1" {
			t.Fatalf("confirmed signup count = %q, error = %v", out, err)
		}
		if err := StageWaitlist(t.Context(), staged, identity, server.URL, netrc); err == nil {
			t.Fatal("replaced an existing staged snapshot")
		}
	}
	mu.Lock()
	conflict = true
	mu.Unlock()
	if err := PublishWaitlist(t.Context(), db, recipients, server.URL, netrc); err == nil || !strings.Contains(err.Error(), "HTTP 412") {
		t.Fatalf("stale write error = %v", err)
	}
	mu.Lock()
	ciphertext[len(ciphertext)-1] ^= 1
	mu.Unlock()
	bad := filepath.Join(private, "corrupt.db")
	if err := StageWaitlist(t.Context(), bad, identities[0], server.URL, netrc); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("corrupt snapshot error = %v", err)
	}
	if _, err := os.Lstat(bad); !os.IsNotExist(err) {
		t.Fatalf("corrupt stage was left behind: %v", err)
	}
}
