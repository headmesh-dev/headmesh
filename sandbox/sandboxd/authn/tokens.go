// Package authn authenticates sandboxd clients: per-client bearer tokens
// stored as argon2id hashes (M1), an OAuth 2.1 resource-server hook (M3),
// and implicit trust for the bind-mounted unix socket.
package authn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Scopes, from plan §6.8.
const (
	ScopeRead  = "sandbox:read"
	ScopeExec  = "sandbox:exec"
	ScopeWrite = "sandbox:write"
	ScopeAdmin = "sandbox:admin"
)

// AllScopes in privilege order.
var AllScopes = []string{ScopeRead, ScopeExec, ScopeWrite, ScopeAdmin}

// Routes a token may be bound to. The tunnel route gets tighter limits
// (exec ≤ 110 s, responses ≤ 8 MiB).
const (
	RouteMCP    = "mcp"
	RouteTunnel = "tunnel"
	RouteUnix   = "unix"
)

// TokenPrefix marks Headmesh sandbox tokens so scanners can recognize them.
const TokenPrefix = "hms_"

// prefixLen is how much of the token is kept in clear to pick candidate
// records without hashing every entry: "hms_" plus 8 random characters.
const prefixLen = len(TokenPrefix) + 8

// Argon2id parameters (OWASP: m ≥ 19 MiB, t ≥ 2). Encoded in each hash, so
// they can change without invalidating existing tokens.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// Record is one stored token (never the token itself).
type Record struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Hash      string     `json:"hash"`
	Scopes    []string   `json:"scopes"`
	Route     string     `json:"route"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the record can authenticate.
func (r Record) Active() bool { return r.RevokedAt == nil }

// File is the on-disk tokens.json.
type File struct {
	Version int      `json:"version"`
	Tokens  []Record `json:"tokens"`
}

// ErrInvalidToken is returned for unknown, revoked or malformed tokens.
var ErrInvalidToken = errors.New("invalid token")

// NewToken returns a fresh plaintext token: "hms_" + 43 base64url characters
// encoding 32 random bytes.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns a PHC-style argon2id hash.
func HashToken(token string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("random salt: %w", err)
	}
	key := argon2.IDKey([]byte(token), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyHash checks token against a PHC argon2id hash in constant time.
func VerifyHash(token, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var mem, iters uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iters, &threads); err != nil {
		return false, fmt.Errorf("bad argon2 params: %w", err)
	}
	if mem > 1<<20 || iters > 10 || threads == 0 || threads > 16 {
		return false, errors.New("argon2 params out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("bad salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("bad hash")
	}
	if len(want) > 64 {
		return false, errors.New("hash too long")
	}
	got := argon2.IDKey([]byte(token), salt, iters, mem, threads, uint32(len(want))) //nolint:gosec // G115: bounded to 64 above
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// LoadFile reads tokens.json. A missing file is an empty store.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-chosen tokens.json path
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse tokens: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("tokens file version %d unsupported", f.Version)
	}
	return &f, nil
}

// Save writes the file atomically (temp file + rename in the same
// directory), so a directory bind mount sees the new content at once.
func (f *File) Save(path string) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tokens: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("tokens dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("temp tokens file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tokens: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tokens: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tokens: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace tokens: %w", err)
	}
	return nil
}

// Create adds a token for name and returns the plaintext once.
func (f *File) Create(name string, scopes []string, route string, now time.Time) (string, Record, error) {
	if name == "" || strings.ContainsAny(name, " \t\n/") {
		return "", Record{}, fmt.Errorf("token name %q must be non-empty without spaces or slashes", name)
	}
	for _, r := range f.Tokens {
		if r.Name == name && r.Active() {
			return "", Record{}, fmt.Errorf("an active token named %q exists; revoke it first", name)
		}
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeRead, ScopeExec, ScopeWrite}
	}
	for _, s := range scopes {
		if !slices.Contains(AllScopes, s) {
			return "", Record{}, fmt.Errorf("unknown scope %q", s)
		}
	}
	if route == "" {
		route = RouteMCP
	}
	if route != RouteMCP && route != RouteTunnel {
		return "", Record{}, fmt.Errorf("route %q must be %s or %s", route, RouteMCP, RouteTunnel)
	}
	tok, err := NewToken()
	if err != nil {
		return "", Record{}, err
	}
	hash, err := HashToken(tok)
	if err != nil {
		return "", Record{}, err
	}
	id := sha256.Sum256([]byte(tok))
	rec := Record{
		ID:        "tok_" + base64.RawURLEncoding.EncodeToString(id[:6]),
		Name:      name,
		Prefix:    tok[:prefixLen],
		Hash:      hash,
		Scopes:    slices.Clone(scopes),
		Route:     route,
		CreatedAt: now.UTC(),
	}
	f.Tokens = append(f.Tokens, rec)
	return tok, rec, nil
}

// Revoke marks every active token named name (or with that ID) as revoked.
func (f *File) Revoke(nameOrID string, now time.Time) (int, error) {
	n := 0
	for i := range f.Tokens {
		r := &f.Tokens[i]
		if (r.Name == nameOrID || r.ID == nameOrID) && r.Active() {
			t := now.UTC()
			r.RevokedAt = &t
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no active token %q", nameOrID)
	}
	return n, nil
}

// Store verifies tokens against tokens.json, reloading it when its mtime
// changes, and caching successful verifications briefly so each MCP POST
// doesn't pay for argon2id.
type Store struct {
	path     string
	cacheTTL time.Duration
	now      func() time.Time

	mu      sync.Mutex
	file    *File
	modTime time.Time
	size    int64
	checked time.Time
	cache   map[[32]byte]cached
	loadErr error
	recheck time.Duration
}

type cached struct {
	rec     Record
	expires time.Time
}

// NewStore returns a store for path; the file may not exist yet.
func NewStore(path string) *Store {
	return &Store{
		path:     path,
		cacheTTL: 5 * time.Minute,
		recheck:  2 * time.Second,
		now:      time.Now,
		cache:    map[[32]byte]cached{},
	}
}

func (s *Store) reloadLocked() {
	defer func() {
		if s.file == nil {
			s.file = &File{Version: 1}
		}
	}()
	now := s.now()
	if s.file != nil && now.Sub(s.checked) < s.recheck {
		return
	}
	s.checked = now
	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if s.file == nil || len(s.file.Tokens) > 0 {
				s.file, s.cache, s.loadErr = &File{Version: 1}, map[[32]byte]cached{}, nil
			}
			return
		}
		s.loadErr = err
		return
	}
	if s.file != nil && fi.ModTime().Equal(s.modTime) && fi.Size() == s.size {
		return
	}
	f, err := LoadFile(s.path)
	if err != nil {
		// Keep serving the last good file; report the error in status.
		s.loadErr = err
		return
	}
	s.file, s.modTime, s.size, s.loadErr = f, fi.ModTime(), fi.Size(), nil
	s.cache = map[[32]byte]cached{} // revocations take effect immediately
}

// LoadError returns the last reload error, if any.
func (s *Store) LoadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// Count returns the number of active tokens.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	n := 0
	for _, r := range s.file.Tokens {
		if r.Active() {
			n++
		}
	}
	return n
}

// Verify returns the active record matching token.
func (s *Store) Verify(token string) (Record, error) {
	if !strings.HasPrefix(token, TokenPrefix) || len(token) < prefixLen+16 || len(token) > 256 {
		return Record{}, ErrInvalidToken
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	s.reloadLocked()
	if c, ok := s.cache[key]; ok && s.now().Before(c.expires) {
		s.mu.Unlock()
		return c.rec, nil
	}
	var candidates []Record
	for _, r := range s.file.Tokens {
		if r.Active() && subtle.ConstantTimeCompare([]byte(r.Prefix), []byte(token[:prefixLen])) == 1 {
			candidates = append(candidates, r)
		}
	}
	s.mu.Unlock()
	for _, r := range candidates {
		ok, err := VerifyHash(token, r.Hash)
		if err != nil || !ok {
			continue
		}
		s.mu.Lock()
		s.cache[key] = cached{rec: r, expires: s.now().Add(s.cacheTTL)}
		s.mu.Unlock()
		return r, nil
	}
	return Record{}, ErrInvalidToken
}
