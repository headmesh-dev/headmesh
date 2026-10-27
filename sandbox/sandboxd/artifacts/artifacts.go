// Package artifacts stores files agents hand back (reports, acceptance
// results, build outputs) and serves them over the tailnet through
// HMAC-signed URLs that expire after an hour.
package artifacts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/textx"
)

// Limits.
const (
	MaxArtifactBytes = 100 << 20
	MaxGetBytes      = 4 << 20
	URLTTL           = time.Hour
)

// ErrNotFound is returned for unknown artifacts.
var ErrNotFound = errors.New("artifact not found")

// Meta is <dir>/<id>/meta.json.
type Meta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	MIME      string    `json:"mime"`
	Client    string    `json:"client"`
	CreatedAt time.Time `json:"created_at"`
	Path      string    `json:"path"`
	URL       string    `json:"url,omitempty"`
	URLExpiry time.Time `json:"url_expires_at,omitzero"`
}

// Store is the artifact directory plus the URL signing key.
type Store struct {
	Dir     string
	BaseURL string // e.g. http://asus-sandbox.mesh.xcelsior.ca:8780
	Key     []byte
	Now     func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// LoadOrCreateKey reads a 32-byte key from path, creating it (0600) if missing.
func LoadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 { //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
		return b[:32], nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("random key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("key dir: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	return key, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "a-" + hex.EncodeToString(b)
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "a-") || len(id) != 14 {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}

// CleanName keeps a safe file name.
func CleanName(name string) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || name == "/" {
		return "", errors.New("artifact name is required")
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '.' || r == '-' || r == '_' || r == '+' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out, nil
}

// Put stores content (or copies the file at src) under name.
func (s *Store) Put(name string, content []byte, src, client string) (Meta, error) {
	clean, err := CleanName(name)
	if err != nil {
		return Meta{}, err
	}
	if src != "" && content != nil {
		return Meta{}, errors.New("give content or path, not both")
	}
	id := newID()
	dir := filepath.Join(s.Dir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Meta{}, fmt.Errorf("artifact dir: %w", err)
	}
	dst := filepath.Join(dir, clean)
	h := sha256.New()
	var size int64
	if src != "" {
		in, err := os.Open(src) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
		if err != nil {
			_ = os.RemoveAll(dir)
			return Meta{}, fmt.Errorf("open source: %w", err)
		}
		defer func() { _ = in.Close() }()
		if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
			_ = os.RemoveAll(dir)
			return Meta{}, fmt.Errorf("%s is not a regular file", src)
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
		if err != nil {
			_ = os.RemoveAll(dir)
			return Meta{}, fmt.Errorf("create artifact: %w", err)
		}
		size, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(in, MaxArtifactBytes+1))
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
		if err != nil || size > MaxArtifactBytes {
			_ = os.RemoveAll(dir)
			if err == nil {
				err = fmt.Errorf("artifact exceeds %d bytes", MaxArtifactBytes)
			}
			return Meta{}, fmt.Errorf("copy artifact: %w", err)
		}
	} else {
		if len(content) > MaxArtifactBytes {
			_ = os.RemoveAll(dir)
			return Meta{}, fmt.Errorf("artifact exceeds %d bytes", MaxArtifactBytes)
		}
		if err := os.WriteFile(dst, content, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return Meta{}, fmt.Errorf("write artifact: %w", err)
		}
		h.Write(content)
		size = int64(len(content))
	}
	mt := mime.TypeByExtension(filepath.Ext(clean))
	if mt == "" {
		mt = "application/octet-stream"
	}
	m := Meta{
		ID: id, Name: clean, Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), MIME: mt, Client: client,
		CreatedAt: s.now().UTC(), Path: dst,
	}
	if err := procutil.WriteJSONAtomic(filepath.Join(dir, "meta.json"), m, 0o640); err != nil {
		return Meta{}, err
	}
	return s.withURL(m), nil
}

func (s *Store) withURL(m Meta) Meta {
	if s.BaseURL == "" || len(s.Key) == 0 {
		return m
	}
	exp := s.now().Add(URLTTL).Unix()
	m.URL = fmt.Sprintf("%s/artifacts/%s/%s?exp=%d&sig=%s", strings.TrimSuffix(s.BaseURL, "/"), m.ID, url.PathEscape(m.Name), exp, s.sign(m.ID, m.Name, exp))
	m.URLExpiry = time.Unix(exp, 0).UTC()
	return m
}

func (s *Store) sign(id, name string, exp int64) string {
	mac := hmac.New(sha256.New, s.Key)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%d", id, name, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Get loads metadata (with a fresh signed URL).
func (s *Store) Get(id string) (Meta, error) {
	if !validID(id) {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	var m Meta
	if err := procutil.ReadJSON(filepath.Join(s.Dir, id, "meta.json"), &m); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return Meta{}, err
	}
	return s.withURL(m), nil
}

// List returns artifacts newest first, optionally filtered by name prefix.
func (s *Store) List(prefix string, limit int) ([]Meta, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Meta{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	out := []Meta{}
	for _, e := range entries {
		if !validID(e.Name()) {
			continue
		}
		m, err := s.Get(e.Name())
		if err != nil || !strings.HasPrefix(m.Name, prefix) {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Meta) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Content is the artifact_get output.
type Content struct {
	Meta
	Encoding  string `json:"encoding"` // "utf-8" or "base64"
	Data      string `json:"data"`
	Offset    int64  `json:"offset"`
	Next      int64  `json:"next_offset"`
	Truncated bool   `json:"truncated"`
	More      string `json:"more,omitempty"`
}

// Read returns up to maxBytes of the artifact from offset.
func (s *Store) Read(id string, offset int64, maxBytes int) (Content, error) {
	m, err := s.Get(id)
	if err != nil {
		return Content{}, err
	}
	if maxBytes <= 0 || maxBytes > MaxGetBytes {
		maxBytes = MaxGetBytes
	}
	data, size, err := procutil.ReadRange(m.Path, max(offset, 0), maxBytes)
	if err != nil {
		return Content{}, fmt.Errorf("read artifact: %w", err)
	}
	c := Content{Meta: m, Offset: max(offset, 0)}
	if textx.LooksBinary(data) {
		c.Encoding, c.Data = "base64", base64.StdEncoding.EncodeToString(data)
	} else {
		if int64(len(data)) < size-c.Offset {
			data = textx.CutUTF8(data, len(data))
		}
		c.Encoding, c.Data = "utf-8", string(data)
	}
	c.Next = c.Offset + int64(len(data))
	c.Truncated = c.Next < size
	if c.Truncated {
		c.More = fmt.Sprintf("%d more bytes; call artifact_get with offset=%d, or download %s", size-c.Next, c.Next, m.URL)
	}
	return c, nil
}

// Handler serves GET /artifacts/<id>/<name>?exp=&sig= without bearer auth;
// the signature is the credential.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/artifacts/")
		id, name, ok := strings.Cut(rest, "/")
		exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
		if !ok || err != nil || !validID(id) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if s.now().Unix() > exp {
			http.Error(w, "link expired; ask the agent for a new one (artifact_get)", http.StatusGone)
			return
		}
		want := s.sign(id, name, exp)
		if !hmac.Equal([]byte(want), []byte(r.URL.Query().Get("sig"))) {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		m, err := s.Get(id)
		if err != nil || m.Name != name {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", m.MIME)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		http.ServeFile(w, r, m.Path)
	})
}
