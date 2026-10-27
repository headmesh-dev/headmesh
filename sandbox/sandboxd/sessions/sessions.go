// Package sessions implements interactive sessions as tmux windows. Each
// window keeps its pane after exit (remain-on-exit), appends everything it
// prints to output.log (pipe-pane), and records its exit code through a
// small wrapper. Metadata lives on disk keyed by sandboxd's own IDs, so
// sessions outlive MCP connections and sandboxd restarts; a container
// restart shows up as a boot identity change and the session as lost.
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

// States reported for a session.
const (
	StateRunning = "running"
	StateExited  = "exited"
	StateLost    = "lost"
)

// Limits.
const (
	MaxReadBytes = 1 << 20
	DefaultRead  = 64 << 10
	MaxWait      = 30 * time.Second
)

// ErrNotFound is returned for unknown session IDs.
var ErrNotFound = errors.New("session not found")

// Meta is persisted as <dir>/<id>/meta.json.
type Meta struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	CWD       string          `json:"cwd"`
	Shell     []string        `json:"shell"`
	EnvKeys   []string        `json:"env_keys,omitempty"`
	Client    string          `json:"client"`
	CreatedAt time.Time       `json:"created_at"`
	Boot      bootid.Identity `json:"boot"`
	WindowID  string          `json:"window_id"`
	PaneID    string          `json:"pane_id"`
	PanePID   int             `json:"pane_pid"`
	Cols      int             `json:"cols"`
	Rows      int             `json:"rows"`
	Removed   bool            `json:"removed,omitempty"`
}

// Info is Meta plus live state.
type Info struct {
	Meta
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     string `json:"signal,omitempty"`
	LostReason string `json:"lost_reason,omitempty"`
	LogBytes   int64  `json:"log_bytes"`
	LogPath    string `json:"log_path"`
	Attach     string `json:"attach"`
}

// Manager owns the session directory and the tmux client.
type Manager struct {
	Dir          string
	Tmux         *tmuxctl.Client
	Boot         bootid.Source
	DefaultCWD   string
	DefaultShell []string
	// Env is added to every session (e.g. HEADMESH_MODE).
	Env map[string]string
	Now func() time.Time

	mu sync.Mutex
}

// CreateOpts are the session_create inputs.
type CreateOpts struct {
	Name   string
	CWD    string
	Shell  []string
	Env    map[string]string
	Cols   int
	Rows   int
	Client string
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "s-" + hex.EncodeToString(b)
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "s-") || len(id) != 14 {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}

func (m *Manager) path(id, name string) string { return filepath.Join(m.Dir, id, name) }

// LogPath returns the output log of a session.
func (m *Manager) LogPath(id string) string { return m.path(id, "output.log") }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wrapperScript waits for pipe-pane, runs the command and records its exit
// code. It traps TERM/INT/HUP with a handler (not an ignore, which children
// would inherit) so it can still write the code after the child is signalled.
const wrapperScript = `i=0; while [ ! -e "$1" ] && [ "$i" -lt ` + "300" + ` ]; do sleep 0.01; i=$((i+1)); done
ef=$2; shift 2
trap : TERM INT HUP
"$@"; rc=$?
printf '%s\n' "$rc" > "$ef.tmp" && mv -f "$ef.tmp" "$ef"
exit "$rc"`

// Create starts a new session window.
func (m *Manager) Create(ctx context.Context, o CreateOpts) (Info, error) {
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
	shell := o.Shell
	if len(shell) == 0 {
		shell = m.DefaultShell
	}
	if len(shell) == 0 {
		shell = []string{"bash", "-l"}
	}
	cols, rows := o.Cols, o.Rows
	if cols <= 0 {
		cols = 200
	}
	if rows <= 0 {
		rows = 50
	}
	if cols > 1000 || rows > 500 {
		return Info{}, errors.New("cols must be ≤ 1000 and rows ≤ 500")
	}
	id := newID()
	dir := filepath.Join(m.Dir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Info{}, fmt.Errorf("session dir: %w", err)
	}
	logPath := m.LogPath(id)
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		return Info{}, fmt.Errorf("session log: %w", err)
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for k, v := range o.Env {
		if strings.ContainsAny(k, "= \t\n") || k == "" {
			return Info{}, fmt.Errorf("bad environment variable name %q", k)
		}
		env[k] = v
	}
	env["HEADMESH_SESSION"] = id
	if _, ok := env["HEADMESH_ENTRYPOINT"]; !ok && o.Client != "" {
		env["HEADMESH_ENTRYPOINT"] = "mcp:" + o.Client
	}
	name := o.Name
	if name == "" {
		name = id
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.Tmux.EnsureSession(ctx, m.DefaultCWD); err != nil {
		return Info{}, err
	}
	argv := append([]string{"bash", "-c", wrapperScript, "hm-session", m.path(id, "ready"), m.path(id, "exit_code")}, shell...)
	pane, err := m.Tmux.NewWindow(ctx, tmuxctl.WindowSpec{Name: "s-" + sanitizeWindowName(name, id), CWD: cwd, Env: env, Command: argv, Cols: cols, Rows: rows})
	if err != nil {
		return Info{}, err
	}
	if err := m.Tmux.PipePane(ctx, pane.PaneID, "cat >> "+shellQuote(logPath)); err != nil {
		_ = m.Tmux.KillWindow(ctx, pane.WindowID)
		return Info{}, err
	}
	if err := os.WriteFile(m.path(id, "ready"), nil, 0o600); err != nil {
		return Info{}, fmt.Errorf("ready marker: %w", err)
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	meta := Meta{
		ID: id, Name: name, CWD: cwd, Shell: shell, EnvKeys: keys, Client: o.Client,
		CreatedAt: m.now().UTC(), Boot: boot, WindowID: pane.WindowID, PaneID: pane.PaneID, PanePID: pane.PID,
		Cols: cols, Rows: rows,
	}
	if err := procutil.WriteJSONAtomic(m.path(id, "meta.json"), meta, 0o640); err != nil {
		return Info{}, err
	}
	return m.info(meta, boot, map[string]tmuxctl.Pane{pane.PaneID: pane}, true), nil
}

func sanitizeWindowName(name, id string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || s == id {
		return strings.TrimPrefix(id, "s-")
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
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

func (m *Manager) panes(ctx context.Context) (map[string]tmuxctl.Pane, bool) {
	ps, err := m.Tmux.Panes(ctx)
	if err != nil {
		return nil, false
	}
	out := make(map[string]tmuxctl.Pane, len(ps))
	for _, p := range ps {
		out[p.PaneID] = p
	}
	return out, true
}

func (m *Manager) info(meta Meta, boot bootid.Identity, panes map[string]tmuxctl.Pane, serverUp bool) Info {
	in := Info{Meta: meta, LogPath: m.LogPath(meta.ID)}
	if fi, err := os.Stat(in.LogPath); err == nil {
		in.LogBytes = fi.Size()
	}
	in.Attach = fmt.Sprintf("headmesh-sandbox attach  (then select window %s)", meta.WindowID)
	exitFromFile := func() *int {
		b, err := os.ReadFile(m.path(meta.ID, "exit_code"))
		if err != nil {
			return nil
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return nil
		}
		return &v
	}
	if meta.Boot.Key != boot.Key {
		in.State = StateLost
		in.LostReason = fmt.Sprintf("container restarted at %s (session started in an earlier container lifetime, boot %s)",
			boot.ContainerStarted.Format(time.RFC3339), meta.Boot.Key)
		return in
	}
	p, ok := panes[meta.PaneID]
	switch {
	case ok && !p.Dead && p.WindowID == meta.WindowID:
		in.State = StateRunning
	case ok && p.Dead:
		in.State = StateExited
		if code := exitFromFile(); code != nil {
			in.ExitCode = code
		} else if p.DeadStatus != nil {
			in.ExitCode = p.DeadStatus
		}
		if p.DeadSignal != nil {
			in.Signal = procutil.SignalName(*p.DeadSignal)
		}
	default:
		if code := exitFromFile(); code != nil {
			in.State, in.ExitCode = StateExited, code
			return in
		}
		in.State = StateLost
		switch {
		case !serverUp:
			in.LostReason = "tmux server is not running (it restarted or was stopped)"
		case meta.Removed:
			in.LostReason = "window was removed with session_kill remove=true"
		default:
			in.LostReason = "tmux window no longer exists (tmux server restarted or the window was closed)"
		}
	}
	return in
}

// Get returns one session.
func (m *Manager) Get(ctx context.Context, id string) (Info, error) {
	meta, err := m.load(id)
	if err != nil {
		return Info{}, err
	}
	boot, err := m.Boot()
	if err != nil {
		return Info{}, fmt.Errorf("boot identity: %w", err)
	}
	panes, up := m.panes(ctx)
	return m.info(meta, boot, panes, up), nil
}

// List returns all sessions, newest first.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	boot, err := m.Boot()
	if err != nil {
		return nil, fmt.Errorf("boot identity: %w", err)
	}
	panes, up := m.panes(ctx)
	var out []Info
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		meta, err := m.load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, m.info(meta, boot, panes, up))
	}
	slices.SortFunc(out, func(a, b Info) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// Send types input (literally) and/or tmux key names into a running session.
func (m *Manager) Send(ctx context.Context, id, input string, enter bool, keys []string) (Info, error) {
	in, err := m.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	if in.State != StateRunning {
		return in, fmt.Errorf("session %s is %s; it can't take input", id, in.State)
	}
	if len(input) > 64<<10 {
		return in, errors.New("input is limited to 64 KiB per call")
	}
	for _, k := range keys {
		if !validKey(k) {
			return in, fmt.Errorf("unsupported key %q (use tmux key names such as Enter, Tab, Escape, C-c, Up)", k)
		}
	}
	if err := m.Tmux.SendLiteral(ctx, in.PaneID, input); err != nil {
		return in, err
	}
	if len(keys) > 0 {
		if err := m.Tmux.SendKeys(ctx, in.PaneID, keys...); err != nil {
			return in, err
		}
	}
	if enter {
		if err := m.Tmux.SendKeys(ctx, in.PaneID, "Enter"); err != nil {
			return in, err
		}
	}
	return in, nil
}

func validKey(k string) bool {
	if k == "" || len(k) > 16 {
		return false
	}
	for _, r := range k {
		if r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ReadOpts are the session_read inputs.
type ReadOpts struct {
	Offset   int64
	MaxBytes int
	Wait     time.Duration
	Raw      bool
	Screen   bool
}

// ReadResult is the session_read output.
type ReadResult struct {
	ID         string `json:"id"`
	Data       string `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	LogBytes   int64  `json:"log_bytes"`
	Truncated  bool   `json:"truncated"`
	More       string `json:"more,omitempty"`
	Alive      bool   `json:"alive"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Screen     string `json:"screen,omitempty"`
}

// Read returns log bytes from offset, waiting up to Wait for new output
// (long poll). Offsets count raw log bytes; Data has escapes stripped unless Raw.
func (m *Manager) Read(ctx context.Context, id string, o ReadOpts) (ReadResult, error) {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultRead
	}
	o.MaxBytes = min(o.MaxBytes, MaxReadBytes)
	o.Wait = min(max(o.Wait, 0), MaxWait)
	in, err := m.Get(ctx, id)
	if err != nil {
		return ReadResult{}, err
	}
	logPath := m.LogPath(id)
	deadline := time.Now().Add(o.Wait)
	for {
		fi, err := os.Stat(logPath)
		if err != nil {
			return ReadResult{}, fmt.Errorf("session log: %w", err)
		}
		if fi.Size() > o.Offset || o.Offset < 0 || in.State != StateRunning || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ReadResult{}, fmt.Errorf("session_read: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		if in, err = m.Get(ctx, id); err != nil {
			return ReadResult{}, err
		}
	}
	// Give tmux a moment to flush the last bytes of an exited pane.
	if in.State == StateExited {
		time.Sleep(20 * time.Millisecond)
	}
	data, size, err := procutil.ReadRange(logPath, o.Offset, o.MaxBytes)
	if err != nil {
		return ReadResult{}, fmt.Errorf("read session log: %w", err)
	}
	start := o.Offset
	if start < 0 {
		start = max(0, size+start)
	}
	if start > size {
		start = size
	}
	if int64(len(data)) == int64(o.MaxBytes) {
		data = textx.CutUTF8(data, len(data))
	}
	next := start + int64(len(data))
	text := data
	if !o.Raw {
		text = textx.StripANSI(data)
	}
	res := ReadResult{
		ID: id, Data: string(text), Offset: start, NextOffset: next, LogBytes: size,
		Truncated: next < size, Alive: in.State == StateRunning, State: in.State, ExitCode: in.ExitCode,
	}
	if res.Truncated {
		res.More = fmt.Sprintf("%d more bytes; call session_read with offset=%d", size-next, next)
	}
	if o.Screen && in.State != StateLost {
		if s, err := m.Tmux.Capture(ctx, in.PaneID, 0); err == nil {
			res.Screen = s
		}
	}
	return res, nil
}

// Kill signals every process in the session's pane (default HUP, which
// ends interactive shells) and optionally removes the window.
func (m *Manager) Kill(ctx context.Context, id, signal string, remove bool) (Info, error) {
	in, err := m.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	if signal == "" {
		signal = "HUP"
	}
	sig, err := procutil.ParseSignal(signal)
	if err != nil {
		return in, err
	}
	if in.State == StateRunning {
		for _, pid := range procutil.SessionMembers(in.PanePID) {
			if pid == in.PanePID && sig != syscall.SIGKILL {
				continue // the wrapper records the exit code; the pane dies with it
			}
			_ = syscall.Kill(pid, sig)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if in, err = m.Get(ctx, id); err != nil || in.State != StateRunning {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if remove {
		if in.State != StateLost {
			if err := m.Tmux.KillWindow(ctx, in.WindowID); err != nil && in.State == StateRunning {
				return in, err
			}
		}
		meta := in.Meta
		meta.Removed = true
		if err := procutil.WriteJSONAtomic(m.path(id, "meta.json"), meta, 0o640); err != nil {
			return in, err
		}
		return m.Get(ctx, id)
	}
	return in, nil
}

// Resize changes the window size.
func (m *Manager) Resize(ctx context.Context, id string, cols, rows int) (Info, error) {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 500 {
		return Info{}, errors.New("cols must be 1–1000 and rows 1–500")
	}
	in, err := m.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	if in.State == StateLost {
		return in, fmt.Errorf("session %s is lost", id)
	}
	if err := m.Tmux.Resize(ctx, in.WindowID, cols, rows); err != nil {
		return in, err
	}
	meta := in.Meta
	meta.Cols, meta.Rows = cols, rows
	if err := procutil.WriteJSONAtomic(m.path(id, "meta.json"), meta, 0o640); err != nil {
		return in, err
	}
	return m.Get(ctx, id)
}

// Summary counts sessions by state.
func Summary(list []Info) map[string]int {
	out := map[string]int{StateRunning: 0, StateExited: 0, StateLost: 0}
	for _, s := range list {
		out[s.State]++
	}
	return out
}
