package bootid

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartTicks(t *testing.T) {
	line := []byte("1234 (we(ird) name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 10\n")
	got, err := StartTicks(line)
	if err != nil {
		t.Fatal(err)
	}
	if got != 98765 {
		t.Fatalf("StartTicks = %d, want 98765", got)
	}
	if _, err := StartTicks([]byte("garbage")); err == nil {
		t.Error("expected error for malformed line")
	}
	if _, err := StartTicks([]byte("1 (x) S 1 2")); err == nil {
		t.Error("expected error for short line")
	}
}

func TestReadFakeProc(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sys/kernel/random/boot_id", "6f1c\n")
	write("1/stat", "1 (s6-svscan) S 0 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 500 1000 10\n")
	write("stat", "cpu 1 2 3\nbtime 1700000000\nprocesses 5\n")
	id, err := read(root)
	if err != nil {
		t.Fatal(err)
	}
	if id.BootID != "6f1c" || id.Key != "6f1c/500" {
		t.Fatalf("identity = %+v", id)
	}
	want := time.Unix(1700000005, 0).UTC()
	if !id.ContainerStarted.Equal(want) {
		t.Fatalf("started = %v, want %v", id.ContainerStarted, want)
	}
}

func TestProcLive(t *testing.T) {
	id, err := Proc()
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	if id.BootID == "" || id.ContainerStarted.After(time.Now()) {
		t.Fatalf("implausible identity %+v", id)
	}
	if _, err := ProcessStartTicks(os.Getpid()); err != nil {
		t.Fatal(err)
	}
}
