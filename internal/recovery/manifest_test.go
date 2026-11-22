package recovery

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validManifest() Manifest {
	return Manifest{
		Version: 1, CapturedAt: time.Unix(1, 0),
		Headscale: HeadscaleState{
			NodeCount: 2, UserCount: 0,
			NoiseKeySHA256: strings.Repeat("ab", 32), PolicySHA256: strings.Repeat("cd", 32),
		},
		PostgresSystemID: "9876543210", PostgresDatabase: "headmesh_prod", PostgresLSN: "0/501020",
	}
}

func TestReadManifest(t *testing.T) {
	payload, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	raw := string(payload)
	if _, err := ReadManifest(bytes.NewReader(payload)); err != nil {
		t.Fatalf("allow a healthy tagged-only cell without audited grants: %v", err)
	}
	tests := map[string]string{
		"missing user count":      strings.Replace(raw, `"user_count":0,`, "", 1),
		"missing grant anchor":    strings.Replace(raw, `,"last_grant_boundary":null`, "", 1),
		"unsupported version":     strings.Replace(raw, `"version":1`, `"version":2`, 1),
		"no nodes":                strings.Replace(raw, `"node_count":2`, `"node_count":0`, 1),
		"invalid WAL position":    strings.Replace(raw, "0/501020", "0/0", 1),
		"no database identity":    strings.Replace(raw, "headmesh_prod", "", 1),
		"additional JSON payload": raw + "{}",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadManifest(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

func TestCompareHeadscale(t *testing.T) {
	expected := validManifest().Headscale
	expected.UserCount = 1
	tests := []struct {
		name   string
		change func(*HeadscaleState)
		pass   bool
	}{
		{"matching", func(_ *HeadscaleState) {}, true},
		{"additional nodes", func(s *HeadscaleState) { s.NodeCount++ }, true},
		{"fewer nodes", func(s *HeadscaleState) { s.NodeCount-- }, false},
		{"missing user", func(s *HeadscaleState) { s.UserCount = 0 }, false},
		{"different identity", func(s *HeadscaleState) { s.NoiseKeySHA256 = strings.Repeat("ef", 32) }, false},
		{"different policy", func(s *HeadscaleState) { s.PolicySHA256 = strings.Repeat("ff", 32) }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := expected
			test.change(&candidate)
			if err := compareHeadscale(expected, candidate); (err == nil) != test.pass {
				t.Fatalf("compareHeadscale err=%v, expected pass=%v", err, test.pass)
			}
		})
	}
}

func TestHeadscaleState(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is required to inspect a Headscale backup")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "noise")
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private := "privkey:" + hex.EncodeToString(key.Bytes())
	public := "mkey:" + hex.EncodeToString(key.PublicKey().Bytes())
	if err := os.WriteFile(keyPath, []byte(private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "restored headscale.sqlite")
	const policy = `{"grants":[]}`
	const fixture = `
CREATE TABLE nodes (id INTEGER PRIMARY KEY, deleted_at TEXT);
CREATE TABLE users (id INTEGER PRIMARY KEY, deleted_at TEXT);
CREATE TABLE policies (id INTEGER PRIMARY KEY, deleted_at TEXT, data TEXT);
INSERT INTO nodes VALUES (1,NULL), (2,NULL), (3,'removed');
INSERT INTO users VALUES (1,'removed');
INSERT INTO policies VALUES (1,NULL,'older'), (2,NULL,'` + policy + `'), (3,'removed','discarded');`
	// #nosec G204 -- The test writes to its own SQLite fixture with fixed SQL.
	if err := exec.CommandContext(t.Context(), "sqlite3", "-init", os.DevNull, "-batch", "-bail", dbPath, fixture).Run(); err != nil {
		t.Fatal(err)
	}
	current, err := headscaleState(t.Context(), dbPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	want := HeadscaleState{
		NodeCount: 2, UserCount: 0,
		NoiseKeySHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(public))),
		PolicySHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte(policy))),
	}
	if current != want {
		t.Fatalf("unexpected Headscale evidence: got %+v, want %+v", current, want)
	}
	if err := os.WriteFile(keyPath, []byte("privkey:"+strings.Repeat("0", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := headscaleState(t.Context(), dbPath, keyPath); err == nil {
		t.Fatal("accepted a zero Noise private key")
	}
}
