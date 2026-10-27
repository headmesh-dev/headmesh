// Package audit appends one JSON line per tool call to audit.jsonl. Arguments
// are recorded as a digest only, so the log never holds file contents,
// commands with inline secrets, or tokens.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Result codes.
const (
	ResultOK          = "ok"
	ResultError       = "error"
	ResultDenied      = "denied"
	ResultRateLimited = "rate_limited"
)

// Entry is one audit line.
type Entry struct {
	TS         time.Time `json:"ts"`
	Client     string    `json:"client"`
	Route      string    `json:"route,omitempty"`
	Tool       string    `json:"tool"`
	ArgsSHA256 string    `json:"args_sha256,omitempty"`
	Mode       string    `json:"mode"`
	DurationMS int64     `json:"duration_ms"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// Log is an append-only JSONL writer with size-based rotation.
type Log struct {
	mu       sync.Mutex
	path     string
	mode     string
	maxBytes int64
	f        *os.File
	size     int64
	now      func() time.Time
}

// DefaultMaxBytes rotates the log at 50 MiB.
const DefaultMaxBytes = 50 << 20

// Open opens (creating if needed) the log at path. mode is stamped on every entry.
func Open(path, mode string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	l := &Log{path: path, mode: mode, maxBytes: DefaultMaxBytes, now: time.Now}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat audit log: %w", err)
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Write appends e, filling TS and Mode when empty.
func (l *Log) Write(e Entry) error {
	if l == nil {
		return nil
	}
	if e.TS.IsZero() {
		e.TS = l.now().UTC()
	}
	if e.Mode == "" {
		e.Mode = l.mode
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode audit entry: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(line)) > l.maxBytes && l.size > 0 {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	return nil
}

func (l *Log) rotate() error {
	_ = l.f.Close()
	rotated := fmt.Sprintf("%s.%s", l.path, l.now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(l.path, rotated); err != nil {
		return fmt.Errorf("rotate audit log: %w", err)
	}
	return l.open()
}

// Close flushes and closes the file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("close audit log: %w", err)
	}
	return nil
}

// Digest returns the hex SHA-256 of the canonical JSON encoding of v.
func Digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DigestRaw hashes already-encoded arguments.
func DigestRaw(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
