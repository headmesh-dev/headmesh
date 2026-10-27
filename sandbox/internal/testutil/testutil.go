// Package testutil holds helpers shared by sandbox tests.
package testutil

import (
	"os"
	"testing"
)

// ShortDir returns a fresh directory under /tmp for unix sockets, whose paths
// must stay under 108 bytes (t.TempDir can be longer when TMPDIR is deep).
func ShortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hm-")
	if err != nil {
		t.Fatalf("short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
