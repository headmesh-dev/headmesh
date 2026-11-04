package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
