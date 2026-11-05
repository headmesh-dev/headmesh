package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOAuthKeysMustBeSeparate(t *testing.T) {
	t.Setenv("HEADMESH_DATABASE_URL", "postgres://test")
	t.Setenv("HEADMESH_OAUTH_ENABLED", "true")
	t.Setenv("HEADMESH_OP_SIGNING_KEY", "/private/operation-key")
	t.Setenv("HEADMESH_OAUTH_SIGNING_KEY_FILE", "/private/operation-key")
	t.Setenv("HEADMESH_OAUTH_CRYPTO_KEY_FILE", "/private/oauth-crypto")
	if _, err := loadConfig(); err == nil {
		t.Fatal("OAuth reused the operation-signing key")
	}
	t.Setenv("HEADMESH_OAUTH_SIGNING_KEY_FILE", "/private/oauth-signing")
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestLoginSecretFileRequiresPrivateNonemptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-secret")
	if err := os.WriteFile(path, []byte("private-client-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loginSecret(path); err == nil {
		t.Fatal("world-readable client secret accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loginSecret(path); err != nil || got != "private-client-secret" {
		t.Fatalf("private file failed: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loginSecret(path); err == nil {
		t.Fatal("empty secret accepted")
	}
}
