// Package procs runs detached background processes (process_start) in their
// own session and process group, logging to a file and recording the exit
// code through a wrapper so status survives sandboxd restarts.
package procs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/textx"
)

// States.
const (
	StateRunning = "running"
	StateExited  = "exited"
	StateLost    = "lost"
)

// ErrNotFound is returned for unknown process IDs.
var ErrNotFound = errors.New("process not found")

// Meta is persisted as <dir>/<id>/meta.json.
type Meta struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Command    string          `json:"command"`
	CWD        string          `json:"cwd"`
	Client     string          `json:"client"`
	PID        int             `json:"pid"`
	StartTicks uint64          `json:"start_ticks"`
	StartedAt  time.Time       `json:"started_at"`
	Boot       bootid.Identity `json:"boot"`
	StoppedBy  string          `json:"stopped_by,omitempty"`
}

// Info is Meta plus live state.
type Info struct {
	Meta
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	LostReason string `json:"lost_reason,omitempty"`
	LogPath    string `json:"log_path"`
	LogBytes   int64  `json:"log_bytes"`
}

// Manager owns the process directory.
type Manager struct {
	Dir        string
	Boot       bootid.Source
	DefaultCWD string
	Env        []string // base environment (KEY=VALUE)
	// NoLogin runs commands with `bash -c` instead of a login shell (tests).
	NoLogin bool
	Now     func() time.Time

	mu      sync.Mutex
	waiters map[string]chan struct{}
}

// StartOpts are the process_start inputs.
type StartOpts struct {
	Name    string
	Command string
	CWD     string
	Env     map[string]string
	Client  string
}

const wrapperScript = `ef=$1; flag=$2; shift 2
trap : TERM INT HUP
bash "$flag" "$1"; rc=$?
printf '%s\n' "$rc" > "$ef.tmp" && mv -f "$ef.tmp" "$ef"
exit "$rc"`

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "p-" + hex.EncodeToString(b)
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "p-") || len(id) != 14 {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}

func (m *Manager) path(id, name string) string { return filepath.Join(m.Dir, id, name) }

// LogPath is the combined stdout/stderr log.
func (m *Manager) LogPath(id string) string { return m.path(id, "output.log") }

// Start launches a detached process.
func (m *Manager) Start(_ context.Context, o StartOpts) (Info, error) {
	if strings.TrimSpace(o.Command) == "" {
		return Info{}, errors.New("command is required")
	}
	boot, err := m.Boot()
	if err != nil {
		return Info{}, fmt.Errorf("boot identity: %w", err)
	}
	cwd := o.CWD
	if cwd == "" {
		cwd = m.DefaultCWD
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return Info{}, fmt.Errorf("cwd %q is not a directory", cwd)
	}
	id := newID()
	if err := os.MkdirAll(filepath.Join(m.Dir, id), 0o750); err != nil {
		return Info{}, fmt.Errorf("process dir: %w", err)
	}
	logf, err := os.OpenFile(m.LogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Info{}, fmt.Errorf("process log: %w", err)
	}
	defer func() { _ = logf.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return Info{}, fmt.Errorf("open /dev/null: %w", err)
	}
	defer func() { _ = devnull.Close() }()

	env := slices.Clone(m.Env)
	if len(env) == 0 {
		env = os.Environ()
	}
	for k, v := range o.Env {
		if k == "" || strings.ContainsAny(k, "= \t\n") {
			return Info{}, fmt.Errorf("bad environment variable name %q", k)
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "HEADMESH_PROCESS="+id)
	if o.Client != "" {
		env = append(env, "HEADMESH_ENTRYPOINT=mcp:"+o.Client)
	}
	// Not CommandContext: the process must outlive the request.
	flag := "-lc"
	if m.NoLogin {
		flag = "-c"
	}
	cmd := exec.Command("bash", "-c", wrapperScript, "hm-process", m.path(id, "exit_code"), flag, o.Command) //nolint:gosec,noctx // running agent commands is the point
	cmd.Dir, cmd.Env = cwd, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Info{}, fmt.Errorf("start: %w", err)
	}
	ticks, _ := bootid.ProcessStartTicks(cmd.Process.Pid)
	name := o.Name
	if name == "" {
		name = firstWord(o.Command)
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	meta := Meta{
		ID: id, Name: name, Command: o.Command, CWD: cwd, Client: o.Client, PID: cmd.Process.Pid,
		StartTicks: ticks, StartedAt: now.UTC(), Boot: boot,
	}
	if err := procutil.WriteJSONAtomic(m.path(id, "meta.json"), meta, 0o640); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return Info{}, err
	}
	done := make(chan struct{})
	m.mu.Lock()
	if m.waiters == nil {
		m.waiters = map[string]chan struct{}{}
	}
	m.waiters[id] = done
	m.mu.Unlock()
	go func() {
		_ = cmd.Wait() // reap; the wrapper writes the exit code
		close(done)
		m.mu.Lock()
		delete(m.waiters, id)
		m.mu.Unlock()
	}()
	return m.info(meta, boot), nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "process"
	}
	w := filepath.Base(f[0])
	if len(w) > 32 {
		w = w[:32]
	}
	return w
}

func (m *Manager) info(meta Meta, boot bootid.Identity) Info {
	in := Info{Meta: meta, LogPath: m.LogPath(meta.ID)}
	if fi, err := os.Stat(in.LogPath); err == nil {
		in.LogBytes = fi.Size()
	}
	if b, err := os.ReadFile(m.path(meta.ID, "exit_code")); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			in.State, in.ExitCode = StateExited, &v
			return in
		}
	}
	if meta.Boot.Key != boot.Key {
		in.State = StateLost
		in.LostReason = fmt.Sprintf("container restarted at %s", boot.ContainerStarted.Format(time.RFC3339))
		return in
	}
	if procutil.Alive(meta.PID, meta.StartTicks) {
		in.State = StateRunning
		return in
	}
	in.State = StateExited
	if meta.StoppedBy != "" {
		in.LostReason = "killed by " + meta.StoppedBy + " before it could record an exit code"
	} else {
		in.LostReason = "exit code unknown (process was killed)"
	}
	return in
}

func (m *Manager) load(id string) (Meta, error) {
	if !validID(id) {
		return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	var meta Meta
	if err := procutil.ReadJSON(m.path(id, "meta.json"), &meta); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return Meta{}, err
	}
	return meta, nil
}

// Get returns one process.
func (m *Manager) Get(id string) (Info, error) {
	meta, err := m.load(id)
	if err != nil {
		return Info{}, err
	}
	boot, err := m.Boot()
	if err != nil {
		return Info{}, fmt.Errorf("boot identity: %w", err)
	}
	return m.info(meta, boot), nil
}

// List returns all processes, newest first.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	boot, err := m.Boot()
	if err != nil {
		return nil, fmt.Errorf("boot identity: %w", err)
	}
	var out []Info
	for _, e := range entries {
		if !validID(e.Name()) {
			continue
		}
		if meta, err := m.load(e.Name()); err == nil {
			out = append(out, m.info(meta, boot))
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return b.StartedAt.Compare(a.StartedAt) })
	return out, nil
}

// LogsResult is the process_logs output.
type LogsResult struct {
	ID         string `json:"id"`
	Data       string `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	LogBytes   int64  `json:"log_bytes"`
	Truncated  bool   `json:"truncated"`
	More       string `json:"more,omitempty"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// Logs returns the last tail lines (default), or bytes from since when since ≥ 0.
func (m *Manager) Logs(id string, tail int, since int64, maxBytes int) (LogsResult, error) {
	in, err := m.Get(id)
	if err != nil {
		return LogsResult{}, err
	}
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 64 << 10
	}
	res := LogsResult{ID: id, State: in.State, ExitCode: in.ExitCode}
	if since >= 0 {
		data, size, err := procutil.ReadRange(in.LogPath, since, maxBytes)
		if err != nil {
			return res, fmt.Errorf("read log: %w", err)
		}
		data = textx.CutUTF8(data, len(data))
		res.Data, res.Offset, res.NextOffset, res.LogBytes = string(data), min(since, size), min(since, size)+int64(len(data)), size
	} else {
		if tail <= 0 {
			tail = 200
		}
		// Read at most maxBytes from the end, then keep the last tail lines.
		data, size, err := procutil.ReadRange(in.LogPath, -int64(maxBytes), maxBytes)
		if err != nil {
			return res, fmt.Errorf("read log: %w", err)
		}
		data = textx.TailUTF8(procutil.TailLines(data, tail), maxBytes)
		res.Data, res.Offset, res.NextOffset, res.LogBytes = string(data), size-int64(len(data)), size, size
	}
	res.Truncated = res.Offset > 0 || res.NextOffset < res.LogBytes
	if res.NextOffset < res.LogBytes {
		res.More = fmt.Sprintf("call process_logs with since=%d for the next bytes", res.NextOffset)
	} else if res.Offset > 0 {
		res.More = fmt.Sprintf("earlier output: process_logs since=0 (log is %d bytes)", res.LogBytes)
	}
	return res, nil
}

// Stop sends signal (default TERM) to the process group, then KILL after
// grace if it is still alive.
func (m *Manager) Stop(ctx context.Context, id, signal string, grace time.Duration) (Info, error) {
	in, err := m.Get(id)
	if err != nil {
		return Info{}, err
	}
	if in.State != StateRunning {
		return in, nil
	}
	if signal == "" {
		signal = "TERM"
	}
	sig, err := procutil.ParseSignal(signal)
	if err != nil {
		return in, err
	}
	if grace <= 0 {
		grace = 10 * time.Second
	}
	grace = min(grace, 60*time.Second)
	members := procutil.SessionMembers(in.PID)
	for _, pid := range members {
		if pid != in.PID || sig == syscall.SIGKILL {
			_ = syscall.Kill(pid, sig)
		}
	}
	if !m.waitExit(ctx, in, grace) {
		meta := in.Meta
		meta.StoppedBy = "SIGKILL after " + grace.String()
		_ = procutil.WriteJSONAtomic(m.path(id, "meta.json"), meta, 0o640)
		procutil.SignalSession(in.PID, syscall.SIGKILL)
		m.waitExit(ctx, in, 2*time.Second)
	}
	return m.Get(id)
}

func (m *Manager) waitExit(ctx context.Context, in Info, d time.Duration) bool {
	m.mu.Lock()
	done := m.waiters[in.ID]
	m.mu.Unlock()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if done != nil {
			select {
			case <-done:
				return true
			default:
			}
		} else if !procutil.Alive(in.PID, in.StartTicks) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false
}
