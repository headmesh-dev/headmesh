// Package procutil has small /proc helpers shared by sessions and processes.
package procutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Signals maps the names tools accept to signal numbers.
var Signals = map[string]syscall.Signal{
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// ParseSignal accepts "TERM", "SIGTERM" or "15".
func ParseSignal(s string) (syscall.Signal, error) {
	name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "SIG")
	if sig, ok := Signals[name]; ok {
		return sig, nil
	}
	if n, err := strconv.Atoi(name); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	return 0, fmt.Errorf("unknown signal %q (use HUP, INT, QUIT, KILL, TERM, USR1 or USR2)", s)
}

// SignalName returns the short name for a signal number.
func SignalName(n int) string {
	for k, v := range Signals {
		if int(v) == n {
			return k
		}
	}
	return strconv.Itoa(n)
}

// statFields returns the fields after the command name of /proc/<pid>/stat.
func statFields(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err //nolint:wrapcheck // callers treat any error as "gone"
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil, fmt.Errorf("malformed stat for %d", pid)
	}
	return strings.Fields(string(b[i+1:])), nil
}

// SessionMembers lists live PIDs whose session ID is sid.
func SessionMembers(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		f, err := statFields(pid)
		// fields[0] is state (field 3); session is field 6.
		if err != nil || len(f) < 4 || f[0] == "Z" {
			continue
		}
		if s, err := strconv.Atoi(f[3]); err == nil && s == sid {
			out = append(out, pid)
		}
	}
	return out
}

// SignalSession sends sig to every process in session sid and returns how
// many were signalled.
func SignalSession(sid int, sig syscall.Signal) int {
	n := 0
	for _, pid := range SessionMembers(sid) {
		if syscall.Kill(pid, sig) == nil {
			n++
		}
	}
	return n
}

// Alive reports whether pid exists, isn't a zombie and, when startTicks is
// non-zero, still has that start time (so a reused PID doesn't count).
func Alive(pid int, startTicks uint64) bool {
	if pid <= 0 {
		return false
	}
	f, err := statFields(pid)
	if err != nil || len(f) < 20 || f[0] == "Z" {
		return false
	}
	if startTicks == 0 {
		return true
	}
	st, err := strconv.ParseUint(f[19], 10, 64)
	return err == nil && st == startTicks
}

// WriteJSONAtomic writes v as indented JSON via temp file + rename.
func WriteJSONAtomic(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ReadJSON decodes path into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: paths are built by sandboxd
	if err != nil {
		return err //nolint:wrapcheck // callers check os.ErrNotExist
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// ReadRange reads up to max bytes of path starting at offset. It reports
// the file size so callers can tell whether more data follows.
func ReadRange(path string, offset int64, max int) ([]byte, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: paths are built by sandboxd
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // callers check os.ErrNotExist
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat: %w", err)
	}
	size := fi.Size()
	if offset < 0 {
		offset = max64(0, size+offset)
	}
	if offset >= size {
		return nil, size, nil
	}
	n := min(int64(max), size-offset)
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, offset)
	if err != nil && read == 0 {
		return nil, size, fmt.Errorf("read: %w", err)
	}
	return buf[:read], size, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// TailLines returns the last n lines of data.
func TailLines(data []byte, n int) []byte {
	if n <= 0 {
		return data
	}
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}
