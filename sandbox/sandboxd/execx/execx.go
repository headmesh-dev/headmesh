// Package execx implements the exec tool: run `bash -lc <command>` in a new
// process group, capture stdout and stderr to files, return the last 64 KB
// of each, and kill the whole group on timeout.
package execx

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
	"strings"
	"syscall"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/textx"
)

// TailBytes is how much of each stream the response carries.
const TailBytes = 64 << 10

// Runner executes commands.
type Runner struct {
	// OutputDir holds <id>/stdout.log and <id>/stderr.log.
	OutputDir  string
	DefaultCWD string
	Env        []string // base environment; os.Environ() when empty
	// NoLogin uses `bash -c` instead of a login shell (tests).
	NoLogin bool
	// KillGrace is the gap between SIGTERM and SIGKILL on timeout (default 2s).
	KillGrace time.Duration
	// Keep is how many past outputs to retain (default 500).
	Keep int
}

// Request is one exec call.
type Request struct {
	Command string
	CWD     string
	Env     map[string]string
	Timeout time.Duration
	Stdin   string
	Client  string
}

// Result is the exec tool output.
type Result struct {
	ExitCode        int    `json:"exit_code"`
	Signal          string `json:"signal,omitempty"`
	TimedOut        bool   `json:"timed_out"`
	DurationMS      int64  `json:"duration_ms"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutBytes     int64  `json:"stdout_bytes"`
	StderrBytes     int64  `json:"stderr_bytes"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	OutputRef       string `json:"output_ref"`
	StdoutPath      string `json:"stdout_path"`
	StderrPath      string `json:"stderr_path"`
	More            string `json:"more,omitempty"`
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405.000000") + "-" + hex.EncodeToString(b)
}

// Run executes the request. A non-zero exit is not an error; errors mean the
// command couldn't be started or its output couldn't be read.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, errors.New("command is required")
	}
	if req.Timeout <= 0 {
		return Result{}, errors.New("timeout must be positive")
	}
	cwd := req.CWD
	if cwd == "" {
		cwd = r.DefaultCWD
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return Result{}, fmt.Errorf("cwd %q is not a directory", cwd)
	}
	r.prune()
	id := newID()
	dir := filepath.Join(r.OutputDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Result{}, fmt.Errorf("output dir: %w", err)
	}
	stdoutPath, stderrPath := filepath.Join(dir, "stdout.log"), filepath.Join(dir, "stderr.log")
	outF, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
	if err != nil {
		return Result{}, fmt.Errorf("stdout file: %w", err)
	}
	defer func() { _ = outF.Close() }()
	errF, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
	if err != nil {
		return Result{}, fmt.Errorf("stderr file: %w", err)
	}
	defer func() { _ = errF.Close() }()

	env := slices.Clone(r.Env)
	if len(env) == 0 {
		env = os.Environ()
	}
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "= \t\n") {
			return Result{}, fmt.Errorf("bad environment variable name %q", k)
		}
		env = append(env, k+"="+v)
	}
	if req.Client != "" {
		env = append(env, "HEADMESH_ENTRYPOINT=mcp:"+req.Client)
	}
	flag := "-lc"
	if r.NoLogin {
		flag = "-c"
	}
	// Timeout is handled by hand so the whole process group is killed.
	cmd := exec.Command("bash", flag, req.Command) //nolint:gosec,noctx // running agent commands is the point
	cmd.Dir, cmd.Env = cwd, env
	// Files, not pipes: Wait returns when bash exits even if a background
	// child keeps the descriptors open.
	cmd.Stdout, cmd.Stderr = outF, errF
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
		cmd.WaitDelay = 5 * time.Second
	} else {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			return Result{}, fmt.Errorf("open /dev/null: %w", err)
		}
		defer func() { _ = devnull.Close() }()
		cmd.Stdin = devnull
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start: %w", err)
	}
	pgid := cmd.Process.Pid
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()
	grace := r.KillGrace
	if grace == 0 {
		grace = 2 * time.Second
	}
	var timedOut, canceled bool
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		canceled = true
	}
	if timedOut || canceled {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case waitErr = <-waitDone:
		case <-time.After(grace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			waitErr = <-waitDone
		}
	}
	dur := time.Since(start)

	res := Result{TimedOut: timedOut, DurationMS: dur.Milliseconds(), OutputRef: "exec/" + id, StdoutPath: stdoutPath, StderrPath: stderrPath}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = procutil.SignalName(int(ws.Signal()))
			res.ExitCode = 128 + int(ws.Signal())
		} else {
			res.ExitCode = exitErr.ExitCode()
		}
	case errors.Is(waitErr, exec.ErrWaitDelay):
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		return res, fmt.Errorf("wait: %w", waitErr)
	}
	if timedOut && res.Signal == "" {
		res.ExitCode = 124 // like timeout(1)
	}
	if res.Stdout, res.StdoutBytes, res.StdoutTruncated, err = tail(stdoutPath); err != nil {
		return res, err
	}
	if res.Stderr, res.StderrBytes, res.StderrTruncated, err = tail(stderrPath); err != nil {
		return res, err
	}
	if res.StdoutTruncated || res.StderrTruncated {
		res.More = fmt.Sprintf("output was cut to the last %d KB per stream; read the full logs with fs_read path=%s (or %s)", TailBytes>>10, stdoutPath, stderrPath)
	}
	if canceled {
		return res, fmt.Errorf("exec canceled: %w", ctx.Err())
	}
	return res, nil
}

func tail(path string) (string, int64, bool, error) {
	data, size, err := procutil.ReadRange(path, -TailBytes, TailBytes)
	if err != nil {
		return "", 0, false, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if size > TailBytes {
		data = textx.TailUTF8(data, len(data))
	}
	return string(data), size, size > TailBytes, nil
}

// prune keeps the newest Keep-1 output directories (IDs sort by time),
// making room for the one about to be created.
func (r *Runner) prune() {
	keep := r.Keep
	if keep <= 0 {
		keep = 500
	}
	keep--
	entries, err := os.ReadDir(r.OutputDir)
	if err != nil || len(entries) <= keep {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, n := range names[:max(0, len(names)-keep)] {
		_ = os.RemoveAll(filepath.Join(r.OutputDir, n))
	}
}
