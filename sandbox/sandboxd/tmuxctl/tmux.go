// Package tmuxctl drives the shared tmux server (`tmux -L headmesh`) that
// backs interactive sessions, so agents and Aaryn (`headmesh-sandbox
// attach`) see the same windows.
package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client runs tmux commands against one server.
type Client struct {
	// Bin is the tmux binary (default "tmux").
	Bin string
	// Socket is the -L socket name (default "headmesh"); ignored when SocketPath is set.
	Socket string
	// SocketPath is a -S path, used by tests.
	SocketPath string
	// Session is the tmux session holding sandboxd windows (default "headmesh").
	Session string
	// Env is extra environment for the tmux client process (e.g. TMUX_TMPDIR).
	Env []string
	// Timeout bounds each tmux command (default 10s).
	Timeout time.Duration
}

// ErrNoServer means the tmux server isn't running.
var ErrNoServer = errors.New("tmux server not running")

func (c *Client) bin() string {
	if c.Bin == "" {
		return "tmux"
	}
	return c.Bin
}

// SessionName returns the session holding sandboxd windows.
func (c *Client) SessionName() string {
	if c.Session == "" {
		return "headmesh"
	}
	return c.Session
}

func (c *Client) baseArgs() []string {
	if c.SocketPath != "" {
		return []string{"-S", c.SocketPath}
	}
	s := c.Socket
	if s == "" {
		s = "headmesh"
	}
	return []string{"-L", s}
}

// AttachArgs are the argv to attach interactively to the session.
func (c *Client) AttachArgs() []string {
	return append(append([]string{c.bin()}, c.baseArgs()...), "attach", "-t", c.SessionName())
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin(), append(c.baseArgs(), args...)...) //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
	if len(c.Env) > 0 {
		cmd.Env = append(cmd.Environ(), c.Env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return "", fmt.Errorf("%w: %s", ErrNoServer, msg)
		}
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

// Running reports whether the server answers.
func (c *Client) Running(ctx context.Context) bool {
	_, err := c.run(ctx, "list-sessions", "-F", "#{session_name}")
	return err == nil
}

// EnsureSession makes sure the server and the sandboxd session exist and
// that dead panes stay visible (remain-on-exit) so their exit status can be
// read. cwd is the first window's working directory.
func (c *Client) EnsureSession(ctx context.Context, cwd string) error {
	if _, err := c.run(ctx, "has-session", "-t", "="+c.SessionName()); err != nil {
		args := []string{"new-session", "-d", "-s", c.SessionName(), "-n", "main"}
		if cwd != "" {
			args = append(args, "-c", cwd)
		}
		if _, err2 := c.run(ctx, args...); err2 != nil {
			// Lost a race with another creator: fine if it exists now.
			if _, err3 := c.run(ctx, "has-session", "-t", "="+c.SessionName()); err3 != nil {
				return err2
			}
		}
	}
	if _, err := c.run(ctx, "set-option", "-s", "exit-empty", "off"); err != nil {
		return err
	}
	if _, err := c.run(ctx, "set-option", "-g", "remain-on-exit", "on"); err != nil {
		return err
	}
	return nil
}

// WindowSpec describes a new window.
type WindowSpec struct {
	Name    string
	CWD     string
	Env     map[string]string
	Command []string // argv; run directly by tmux
	Cols    int
	Rows    int
}

// Pane is one pane as reported by list-panes.
type Pane struct {
	Session    string
	WindowID   string
	WindowName string
	PaneID     string
	PID        int
	Dead       bool
	DeadStatus *int
	DeadSignal *int
	Command    string
	Width      int
	Height     int
}

const paneFormat = "#{session_name}\t#{window_id}\t#{window_name}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{pane_current_command}\t#{window_width}\t#{window_height}"

// NewWindow creates a detached window in the session and returns its pane.
func (c *Client) NewWindow(ctx context.Context, spec WindowSpec) (Pane, error) {
	args := []string{"new-window", "-d", "-P", "-F", paneFormat, "-t", c.SessionName() + ":"}
	if spec.Name != "" {
		args = append(args, "-n", spec.Name)
	}
	if spec.CWD != "" {
		args = append(args, "-c", spec.CWD)
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Command...)
	out, err := c.run(ctx, args...)
	if err != nil {
		return Pane{}, err
	}
	panes := parsePanes(out)
	if len(panes) != 1 {
		return Pane{}, fmt.Errorf("new-window returned %q", out)
	}
	p := panes[0]
	if spec.Cols > 0 && spec.Rows > 0 {
		if err := c.Resize(ctx, p.WindowID, spec.Cols, spec.Rows); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Panes lists every pane on the server.
func (c *Client) Panes(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return parsePanes(out), nil
}

func parsePanes(out string) []Pane {
	var panes []Pane
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 11 {
			continue
		}
		p := Pane{Session: f[0], WindowID: f[1], WindowName: f[2], PaneID: f[3], Dead: f[5] == "1", Command: f[8]}
		p.PID, _ = strconv.Atoi(f[4])
		if p.Dead {
			if v, err := strconv.Atoi(f[6]); err == nil && f[6] != "" {
				p.DeadStatus = &v
			}
			if v, err := strconv.Atoi(f[7]); err == nil && f[7] != "" && v != 0 {
				p.DeadSignal = &v
			}
		}
		p.Width, _ = strconv.Atoi(f[9])
		p.Height, _ = strconv.Atoi(f[10])
		panes = append(panes, p)
	}
	return panes
}

// PipePane appends the pane's output to a shell command (-o: only if not
// already piping).
func (c *Client) PipePane(ctx context.Context, paneID, shellCmd string) error {
	_, err := c.run(ctx, "pipe-pane", "-o", "-t", paneID, shellCmd)
	return err
}

// SendLiteral types text into the pane without key-name interpretation.
func (c *Client) SendLiteral(ctx context.Context, paneID, text string) error {
	if text == "" {
		return nil
	}
	_, err := c.run(ctx, "send-keys", "-t", paneID, "-l", "--", text)
	return err
}

// SendKeys sends tmux key names such as "Enter", "C-c" or "Up".
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := c.run(ctx, append([]string{"send-keys", "-t", paneID, "--"}, keys...)...)
	return err
}

// Resize sets a window's size.
func (c *Client) Resize(ctx context.Context, windowID string, cols, rows int) error {
	_, err := c.run(ctx, "resize-window", "-t", windowID, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	return err
}

// KillWindow removes a window.
func (c *Client) KillWindow(ctx context.Context, windowID string) error {
	_, err := c.run(ctx, "kill-window", "-t", windowID)
	return err
}

// Capture returns the visible screen plus up to history lines of scrollback,
// as plain text.
func (c *Client) Capture(ctx context.Context, paneID string, history int) (string, error) {
	return c.run(ctx, "capture-pane", "-p", "-J", "-t", paneID, "-S", strconv.Itoa(-history))
}

// RespawnPane restarts a dead pane with a new command.
func (c *Client) RespawnPane(ctx context.Context, paneID string, command ...string) error {
	_, err := c.run(ctx, append([]string{"respawn-pane", "-k", "-t", paneID}, command...)...)
	return err
}

// KillServer stops the server (tests only).
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	return err
}
