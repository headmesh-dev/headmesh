package artifacts

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "keys", "artifact.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	return &Store{Dir: filepath.Join(t.TempDir(), "artifacts"), BaseURL: "http://sbx:8780", Key: key, Now: func() time.Time { return now }}, &now
}

func TestPutGetList(t *testing.T) {
	s, _ := newStore(t)
	m, err := s.Put("../report final.md", []byte("# Report\nall good\n"), "", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "report_final.md" || m.Size != 18 || m.MIME != "text/markdown; charset=utf-8" || !strings.Contains(m.URL, "/artifacts/"+m.ID+"/report_final.md?exp=") {
		t.Fatalf("meta %+v", m)
	}
	src := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(src, []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	bm, err := s.Put("blob.bin", nil, src, "codex")
	if err != nil || bm.Size != 4 {
		t.Fatalf("put from path %+v %v", bm, err)
	}
	c, err := s.Read(bm.ID, 0, 0)
	if err != nil || c.Encoding != "base64" {
		t.Fatalf("binary read %+v %v", c, err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(c.Data); len(raw) != 4 {
		t.Fatalf("decoded %v", raw)
	}
	c, err = s.Read(m.ID, 2, 6)
	if err != nil || c.Data != "Report" || !c.Truncated || c.Next != 8 || !strings.Contains(c.More, "offset=8") {
		t.Fatalf("ranged read %+v %v", c, err)
	}
	list, err := s.List("report", 0)
	if err != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.Put("", []byte("x"), "", ""); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := s.Put("x", []byte("x"), src, ""); err == nil {
		t.Fatal("content and path accepted together")
	}
	if _, err := s.Get("a-../../etc"); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestSignedURL(t *testing.T) {
	s, now := newStore(t)
	m, err := s.Put("out.txt", []byte("payload"), "", "c")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	u, _ := url.Parse(m.URL)
	get := func(rawQuery, path string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path+"?"+rawQuery, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(u.RawQuery, u.Path); code != 200 || body != "payload" {
		t.Fatalf("valid link: %d %q", code, body)
	}
	q := u.Query()
	q.Set("sig", "AAAA")
	if code, _ := get(q.Encode(), u.Path); code != http.StatusForbidden {
		t.Fatalf("bad sig: %d", code)
	}
	if code, _ := get(u.RawQuery, "/artifacts/"+m.ID+"/other.txt"); code != http.StatusForbidden {
		t.Fatalf("renamed path: %d", code)
	}
	*now = now.Add(2 * time.Hour)
	if code, _ := get(u.RawQuery, u.Path); code != http.StatusGone {
		t.Fatalf("expired link: %d", code)
	}
}

func TestKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	a, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(p)
	if err != nil || string(a) != string(b) {
		t.Fatal("key changed between loads")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
}
