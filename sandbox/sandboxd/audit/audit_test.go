package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestWriteConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "audit.jsonl")
	l, err := Open(path, "regular")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Write(Entry{Client: "c", Tool: "exec", Result: ResultOK, DurationMS: int64(i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	entries := readEntries(t, path)
	if len(entries) != 50 {
		t.Fatalf("got %d entries", len(entries))
	}
	for _, e := range entries {
		if e.Mode != "regular" || e.TS.IsZero() {
			t.Fatalf("entry not stamped: %+v", e)
		}
	}
}

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := Open(path, "yolo")
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = 300
	tick := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	for range 10 {
		if err := l.Write(Entry{Client: "c", Tool: "fs_read", Result: ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "audit.jsonl*"))
	if len(files) < 2 {
		t.Fatalf("expected rotation, got %v", files)
	}
	total := 0
	for _, f := range files {
		total += len(readEntries(t, f))
	}
	if total != 10 {
		t.Fatalf("entries lost across rotation: %d", total)
	}
}

func TestDigestIsStableAndOpaque(t *testing.T) {
	a := Digest(map[string]any{"command": "echo hms_secret", "cwd": "/x"})
	b := Digest(map[string]any{"cwd": "/x", "command": "echo hms_secret"})
	if a != b || len(a) != 64 {
		t.Fatalf("digest unstable: %s vs %s", a, b)
	}
	if strings.Contains(a, "secret") {
		t.Fatal("digest leaks input")
	}
	if DigestRaw(nil) != "" {
		t.Fatal("empty raw digest should be empty")
	}
}

func TestNilLogIsNoop(t *testing.T) {
	var l *Log
	if err := l.Write(Entry{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}
