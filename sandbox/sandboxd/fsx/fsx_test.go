package fsx

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFS(t *testing.T) (*FS, string) {
	t.Helper()
	root := t.TempDir()
	return &FS{
		Home: root, WritableRoots: []string{root}, FallbackTrash: filepath.Join(t.TempDir(), "trash"),
		Now: func() time.Time { return time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC) },
	}, root
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadNumberedAndPaged(t *testing.T) {
	f, root := newFS(t)
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\r\n")
	}
	write(t, filepath.Join(root, "a.txt"), b.String())
	res, err := f.Read("a.txt", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "     3\tline xxx\n     4\tline xxxx\n" || !res.Truncated || res.NextOffset != 5 || res.StartLine != 3 {
		t.Fatalf("paged read %+v", res)
	}
	res, err = f.Read(filepath.Join(root, "a.txt"), 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.TotalLines != 10 || res.EndLine != 10 {
		t.Fatalf("tail read %+v", res)
	}
	res, _ = f.Read("a.txt", 50, 10)
	if !strings.Contains(res.More, "past the end") {
		t.Fatalf("past end %+v", res)
	}
}

func TestReadBinaryAndLongLines(t *testing.T) {
	f, root := newFS(t)
	write(t, filepath.Join(root, "bin"), "ELF\x00\x01\x02")
	res, err := f.Read("bin", 0, 0)
	if err != nil || !res.Binary || res.Content != "" {
		t.Fatalf("binary %+v %v", res, err)
	}
	write(t, filepath.Join(root, "long"), strings.Repeat("é", MaxLineChars)+"\n")
	res, err = f.Read("long", 0, 0)
	if err != nil || !strings.Contains(res.Content, "[line truncated]") {
		t.Fatalf("long line %+v %v", res, err)
	}
	if _, err := f.Read(root, 0, 0); err == nil {
		t.Fatal("reading a directory should fail")
	}
}

func TestWriteAtomicPreservesMode(t *testing.T) {
	f, root := newFS(t)
	p := filepath.Join(root, "script.sh")
	write(t, p, "old")
	if err := os.Chmod(p, 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := f.Write("script.sh", "#!/bin/sh\necho hi\n", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if res.Created || fi.Mode().Perm() != 0o750 || res.Bytes != 18 || len(res.SHA256) != 64 {
		t.Fatalf("write %+v mode %v", res, fi.Mode())
	}
	if _, err := f.Write("new/dir/file.txt", "x", "", "", false); err == nil {
		t.Fatal("missing dir without create_dirs accepted")
	}
	res, err = f.Write("new/dir/file.bin", base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), "base64", "0600", true)
	if err != nil || !res.Created {
		t.Fatalf("base64 write %+v %v", res, err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "new/dir/file.bin")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	for _, bad := range [][3]string{{"x", "!!", "base64"}, {"x", "y", "utf-16"}} {
		if _, err := f.Write(bad[0], bad[1], bad[2], "", false); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := f.Write("m", "x", "", "999", false); err == nil {
		t.Error("bad mode accepted")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".hm-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteThroughSymlink(t *testing.T) {
	f, root := newFS(t)
	write(t, filepath.Join(root, "real.txt"), "a")
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write("link.txt", "b", "", "", false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "link.txt")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "real.txt")); string(b) != "b" {
		t.Fatalf("target = %q", b)
	}
}

func TestReadOnlyMountRefused(t *testing.T) {
	f, _ := newFS(t)
	// /proc is mounted without write support for regular files; /sys is read-only in most containers.
	// Use a path whose filesystem reports ST_RDONLY when available.
	for _, p := range []string{"/sys/kernel/x", "/proc/sys/x"} {
		var ro bool
		if st, err := f.Stat(filepath.Dir(p)); err == nil && st.ReadOnlyMount {
			ro = true
		}
		if !ro {
			continue
		}
		_, err := f.Write(p, "x", "", "", false)
		var roErr *ReadOnlyError
		if !errors.As(err, &roErr) || !strings.Contains(err.Error(), "writable mounts:") {
			t.Fatalf("write to %s: %v", p, err)
		}
		return
	}
	t.Skip("no read-only mount available on this host")
}

func TestEdit(t *testing.T) {
	f, root := newFS(t)
	p := filepath.Join(root, "main.go")
	write(t, p, "a := 1\nb := 1\nc := 2\n")
	if _, err := f.Edit("main.go", ":= 1", ":= 3", false); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("ambiguous edit: %v", err)
	}
	if _, err := f.Edit("main.go", "zzz", "y", false); err == nil {
		t.Fatal("missing old_string accepted")
	}
	if _, err := f.Edit("main.go", "x", "x", false); err == nil {
		t.Fatal("identical strings accepted")
	}
	res, err := f.Edit("main.go", "c := 2", "c := 5", false)
	if err != nil || res.Replacements != 1 {
		t.Fatalf("edit %+v %v", res, err)
	}
	res, err = f.Edit("main.go", ":= 1", ":= 9", true)
	if err != nil || res.Replacements != 2 {
		t.Fatalf("replace_all %+v %v", res, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "a := 9\nb := 9\nc := 5\n" {
		t.Fatalf("content %q", b)
	}
}

func TestListAndStat(t *testing.T) {
	f, root := newFS(t)
	write(t, filepath.Join(root, "a.go"), "")
	write(t, filepath.Join(root, "sub/b.go"), "")
	write(t, filepath.Join(root, "sub/deep/c.txt"), "")
	write(t, filepath.Join(root, ".hidden/d.go"), "")
	write(t, filepath.Join(root, ".git/config"), "")
	res, err := f.List(root, 3, "*.go", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Entries {
		got = append(got, strings.TrimPrefix(e.Path, root+"/"))
	}
	if strings.Join(got, ",") != "a.go,sub/b.go" {
		t.Fatalf("list = %v", got)
	}
	res, _ = f.List(root, 1, "", true, 2)
	if !res.Truncated || len(res.Entries) != 2 {
		t.Fatalf("limit %+v", res)
	}
	if _, err := f.List(root, 1, "[", false, 0); err == nil {
		t.Fatal("bad glob accepted")
	}
	st, err := f.Stat("sub/b.go")
	if err != nil || !st.Exists || st.Type != "file" || !st.Writable {
		t.Fatalf("stat %+v %v", st, err)
	}
	st, _ = f.Stat("nope")
	if st.Exists {
		t.Fatal("missing file exists")
	}
}

func TestMoveAndDelete(t *testing.T) {
	f, root := newFS(t)
	write(t, filepath.Join(root, "proj/a.txt"), "a")
	write(t, filepath.Join(root, "proj/b.txt"), "b")
	if _, err := f.Move("proj/a.txt", "proj/b.txt", false); err == nil {
		t.Fatal("overwrite without flag accepted")
	}
	mv, err := f.Move("proj/a.txt", "proj/c.txt", false)
	if err != nil || mv.To != filepath.Join(root, "proj/c.txt") {
		t.Fatalf("move %+v %v", mv, err)
	}
	if err := os.Mkdir(filepath.Join(root, "dest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if mv, err = f.Move("proj/c.txt", "dest", false); err != nil || mv.To != filepath.Join(root, "dest/c.txt") {
		t.Fatalf("move into dir %+v %v", mv, err)
	}
	if _, err := f.Delete("proj", false); err == nil {
		t.Fatal("non-empty dir deleted without recursive")
	}
	del, err := f.Delete("proj", true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, TrashDirName, "20261027T010203-")
	if !strings.HasPrefix(del.TrashedTo, want) || !strings.HasSuffix(del.TrashedTo, "/proj") {
		t.Fatalf("trash path %s", del.TrashedTo)
	}
	if b, err := os.ReadFile(filepath.Join(del.TrashedTo, "b.txt")); err != nil || string(b) != "b" {
		t.Fatalf("trashed content %q %v", b, err)
	}
	if _, err := f.Delete(root, true); err == nil {
		t.Fatal("deleting a mount root accepted")
	}
}

func TestSearch(t *testing.T) {
	f, root := newFS(t)
	write(t, filepath.Join(root, "a.go"), "package a\nfunc Hello() {}\n// hello again\n")
	write(t, filepath.Join(root, "b.txt"), "Hello.World\n")
	write(t, filepath.Join(root, "node_modules/x.go"), "Hello\n")
	for _, engine := range []string{"rg", ""} {
		old := RipgrepBin
		RipgrepBin = engine
		t.Cleanup(func() { RipgrepBin = old })
		res, err := f.Search(context.Background(), SearchOpts{Pattern: "Hello.", Path: root})
		if err != nil {
			t.Fatal(err)
		}
		if res.Count != 1 || res.Matches[0].Line != 1 || !strings.HasSuffix(res.Matches[0].Path, "b.txt") {
			t.Fatalf("%s fixed-string search %+v", res.Engine, res)
		}
		res, err = f.Search(context.Background(), SearchOpts{Pattern: "hello", Path: root, Glob: "*.go", CaseInsensitive: true, MaxResults: 1})
		if err != nil {
			t.Fatal(err)
		}
		if res.Count != 1 || !res.Truncated {
			t.Fatalf("%s limited search %+v", res.Engine, res)
		}
		res, err = f.Search(context.Background(), SearchOpts{Pattern: `func \w+\(`, Path: root, Regex: true})
		if err != nil || res.Count != 1 {
			t.Fatalf("%s regex search %+v %v", res.Engine, res, err)
		}
		res, err = f.Search(context.Background(), SearchOpts{Pattern: "absent-string", Path: root})
		if err != nil || res.Count != 0 {
			t.Fatalf("%s no-match search %+v %v", res.Engine, res, err)
		}
	}
	if _, err := f.Search(context.Background(), SearchOpts{}); err == nil {
		t.Fatal("empty pattern accepted")
	}
}

func TestResolve(t *testing.T) {
	f := &FS{Home: "/home/agent"}
	cases := map[string]string{"x": "/home/agent/x", "~/y": "/home/agent/y", "/a/../b": "/b", "~": "/home/agent"}
	for in, want := range cases {
		if got, err := f.Resolve(in); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := f.Resolve(""); err == nil {
		t.Error("empty path accepted")
	}
	if _, err := f.Resolve("a\x00b"); err == nil {
		t.Error("NUL accepted")
	}
}
