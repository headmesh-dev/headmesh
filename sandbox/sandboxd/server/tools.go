package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/agents"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/artifacts"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/execx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/fsx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/gitx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procs"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/sessions"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// Tool inputs and outputs. Fields without omitempty are required.

// StatusIn is the sandbox_status input.
type StatusIn struct {
	ProbeReach bool `json:"probe_reach,omitempty" jsonschema:"also probe every configured reach target (adds up to 15 s)"`
}

// ExecIn is the exec input.
type ExecIn struct {
	Command  string            `json:"command" jsonschema:"command line, run as bash -lc in a new process group"`
	CWD      string            `json:"cwd,omitempty" jsonschema:"working directory (default /home/agent)"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	TimeoutS int               `json:"timeout_s,omitempty" jsonschema:"timeout in seconds: default 120, max 3600 (110 through the OpenAI tunnel); the whole process group is killed on timeout"`
	Stdin    string            `json:"stdin,omitempty" jsonschema:"text fed to standard input"`
}

// ExecOut is the exec output.
type ExecOut struct {
	execx.Result
	TimeoutS int    `json:"timeout_s"`
	Note     string `json:"note,omitempty"`
}

// SessionCreateIn is the session_create input.
type SessionCreateIn struct {
	Name  string            `json:"name,omitempty" jsonschema:"label shown in tmux and session_list"`
	CWD   string            `json:"cwd,omitempty" jsonschema:"working directory (default /home/agent)"`
	Shell []string          `json:"shell,omitempty" jsonschema:"argv to run (default [bash -l])"`
	Env   map[string]string `json:"env,omitempty"`
	Cols  int               `json:"cols,omitempty" jsonschema:"terminal width (default 200)"`
	Rows  int               `json:"rows,omitempty" jsonschema:"terminal height (default 50)"`
}

// SessionSendIn is the session_send input.
type SessionSendIn struct {
	ID    string   `json:"id"`
	Input string   `json:"input,omitempty" jsonschema:"text typed literally"`
	Enter bool     `json:"enter,omitempty" jsonschema:"press Enter after the input"`
	Keys  []string `json:"keys,omitempty" jsonschema:"tmux key names sent after the input, e.g. C-c, Escape, Up, Tab"`
}

// SessionReadIn is the session_read input.
type SessionReadIn struct {
	ID       string `json:"id"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"byte offset into the session log (use next_offset from the previous read; negative counts from the end)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default 65536, max 1048576"`
	WaitMS   int    `json:"wait_ms,omitempty" jsonschema:"long-poll up to this many milliseconds for new output (max 30000)"`
	Raw      bool   `json:"raw,omitempty" jsonschema:"keep terminal escape sequences"`
	Screen   bool   `json:"screen,omitempty" jsonschema:"also return the current screen as plain text"`
}

// IDIn identifies a session, process, artifact or agent.
type IDIn struct {
	ID string `json:"id"`
}

// SessionKillIn is the session_kill input.
type SessionKillIn struct {
	ID     string `json:"id"`
	Signal string `json:"signal,omitempty" jsonschema:"HUP (default), INT, TERM, KILL, QUIT, USR1 or USR2"`
	Remove bool   `json:"remove,omitempty" jsonschema:"also close the tmux window"`
}

// SessionResizeIn is the session_resize input.
type SessionResizeIn struct {
	ID   string `json:"id"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// EmptyIn takes no arguments.
type EmptyIn struct{}

// SessionListOut is the session_list output.
type SessionListOut struct {
	Sessions []sessions.Info `json:"sessions"`
	Counts   map[string]int  `json:"counts"`
}

// ProcessStartIn is the process_start input.
type ProcessStartIn struct {
	Command string            `json:"command" jsonschema:"command line, run detached as bash -lc in its own session"`
	CWD     string            `json:"cwd,omitempty"`
	Name    string            `json:"name,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ProcessListOut is the process_list output.
type ProcessListOut struct {
	Processes []procs.Info `json:"processes"`
}

// ProcessLogsIn is the process_logs input.
type ProcessLogsIn struct {
	ID       string `json:"id"`
	Tail     int    `json:"tail,omitempty" jsonschema:"last N lines (default 200); ignored when since is set"`
	Since    *int64 `json:"since,omitempty" jsonschema:"byte offset to read from (use next_offset)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default 65536, max 1048576"`
}

// ProcessStopIn is the process_stop input.
type ProcessStopIn struct {
	ID     string `json:"id"`
	Signal string `json:"signal,omitempty" jsonschema:"TERM (default), INT, HUP or KILL"`
	GraceS int    `json:"grace_s,omitempty" jsonschema:"seconds before KILL (default 10, max 60)"`
}

// FsReadIn is the fs_read input.
type FsReadIn struct {
	Path   string `json:"path" jsonschema:"absolute path, or relative to /home/agent"`
	Offset int    `json:"offset,omitempty" jsonschema:"first line, 1-based"`
	Limit  int    `json:"limit,omitempty" jsonschema:"lines to return (default 2000, max 20000); output is capped at 2 MB"`
}

// FsWriteIn is the fs_write input.
type FsWriteIn struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Encoding   string `json:"encoding,omitempty" jsonschema:"utf-8 (default) or base64"`
	Mode       string `json:"mode,omitempty" jsonschema:"octal mode like 0644; default keeps the existing mode"`
	CreateDirs bool   `json:"create_dirs,omitempty"`
}

// FsEditIn is the fs_edit input.
type FsEditIn struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string" jsonschema:"exact text to replace; must be unique unless replace_all"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// FsListIn is the fs_list input.
type FsListIn struct {
	Path          string `json:"path"`
	Depth         int    `json:"depth,omitempty" jsonschema:"levels to descend (default 1, max 10)"`
	Glob          string `json:"glob,omitempty" jsonschema:"filter on the base name, or on the relative path when it contains /"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
	Limit         int    `json:"limit,omitempty" jsonschema:"default 1000, max 10000"`
}

// FsSearchIn is the fs_search input.
type FsSearchIn struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty" jsonschema:"directory or file (default /home/agent)"`
	Glob            string `json:"glob,omitempty" jsonschema:"ripgrep glob, e.g. *.go"`
	Regex           bool   `json:"regex,omitempty" jsonschema:"treat pattern as a regular expression (default: fixed string)"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	MaxResults      int    `json:"max_results,omitempty" jsonschema:"default 200, max 2000"`
	Context         int    `json:"context,omitempty" jsonschema:"context lines around each match (max 5)"`
}

// PathIn names one path.
type PathIn struct {
	Path string `json:"path"`
}

// FsMoveIn is the fs_move input.
type FsMoveIn struct {
	Src       string `json:"src"`
	Dst       string `json:"dst" jsonschema:"destination path, or an existing directory to move into"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// FsDeleteIn is the fs_delete input.
type FsDeleteIn struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// RepoIn names a repo.
type RepoIn struct {
	Repo string `json:"repo" jsonschema:"absolute path, owner/repo from [repos], or a bare repo name"`
}

// GitSyncIn is the git_sync input.
type GitSyncIn struct {
	Repo            string `json:"repo"`
	AbortOnConflict *bool  `json:"abort_on_conflict,omitempty" jsonschema:"abort the rebase on conflict so the shared working copy stays clean (default true)"`
}

// GitPushIn is the git_push_safe input.
type GitPushIn struct {
	Repo   string `json:"repo"`
	Remote string `json:"remote,omitempty" jsonschema:"default origin"`
	Branch string `json:"branch,omitempty" jsonschema:"default: the current branch (must be checked out)"`
}

// ArtifactPutIn is the artifact_put input.
type ArtifactPutIn struct {
	Name          string `json:"name"`
	Content       string `json:"content,omitempty" jsonschema:"text content"`
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"binary content, base64"`
	Path          string `json:"path,omitempty" jsonschema:"copy this file instead"`
}

// ArtifactListIn is the artifact_list input.
type ArtifactListIn struct {
	Prefix string `json:"prefix,omitempty"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 100"`
}

// ArtifactListOut is the artifact_list output.
type ArtifactListOut struct {
	Artifacts []artifacts.Meta `json:"artifacts"`
}

// ArtifactGetIn is the artifact_get input.
type ArtifactGetIn struct {
	ID       string `json:"id"`
	Offset   int64  `json:"offset,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"default and max 4 MiB"`
}

// NetCheckIn is the net_check input.
type NetCheckIn struct {
	Target string `json:"target" jsonschema:"host, MagicDNS name, IP, or host:port"`
	Port   int    `json:"port,omitempty" jsonschema:"TCP port to test"`
}

// AgentsRegisterIn is the agents_register input.
type AgentsRegisterIn struct {
	Agent   string   `json:"agent" jsonschema:"entry point, e.g. claude-runner, claude-rc, codex-ssh, codex-exec, chatgpt-tunnel, mcp"`
	Task    string   `json:"task" jsonschema:"one line describing the task"`
	Repos   []string `json:"repos,omitempty" jsonschema:"repos you will touch (paths or [repos] names)"`
	Session string   `json:"session,omitempty" jsonschema:"your own session id, if any"`
}

// AgentsHeartbeatIn is the agents_heartbeat input.
type AgentsHeartbeatIn struct {
	ID   string `json:"id"`
	Task string `json:"task,omitempty" jsonschema:"updated task line"`
}

// AgentsListIn is the agents_list input.
type AgentsListIn struct {
	Repo string `json:"repo,omitempty"`
}

// AgentsListOut is the agents_list output.
type AgentsListOut struct {
	Agents []agents.Agent `json:"agents"`
}

// OKOut acknowledges an action.
type OKOut struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func (s *Server) execTimeout(c authn.Caller, requested int) (int, string) {
	sd := s.cfg.Sandboxd
	maxS := sd.ExecMaxTimeoutS
	if c.Tunnel() {
		maxS = sd.TunnelExecMaxTimeoutS
	}
	if requested <= 0 {
		return min(sd.ExecDefaultTimeoutS, maxS), ""
	}
	if requested > maxS {
		return maxS, fmt.Sprintf("timeout_s %d lowered to %d, the limit for route %q; use process_start for longer work", requested, maxS, c.Route)
	}
	return requested, ""
}

func (s *Server) repo(name string) (string, error) {
	p, err := s.cfg.RepoPath(name)
	if err != nil {
		return "", err
	}
	return p, nil
}

func (s *Server) registerTools() {
	read, exec, write := authn.ScopeRead, authn.ScopeExec, authn.ScopeWrite

	addTool(s, toolSpec{
		name: "sandbox_status", title: "Sandbox status", scope: read, readOnly: true, idempotent: true,
		desc: "Mode (and YOLO TTL), boot_id and start times, tailnet identity and reachability, entry-point health, sessions and processes (running / exited / lost after a restart), active agents per repo, git summary, GPU and cache usage.",
	},
		func(in StatusIn) time.Duration { return 30 * time.Second },
		func(ctx context.Context, _ authn.Caller, in StatusIn) (status.Report, error) {
			return s.status.Collect(ctx, status.Options{ProbeReach: in.ProbeReach}), nil
		})

	addTool(s, toolSpec{
		name: "exec", title: "Run a command", scope: exec, destructive: true, openWorld: true,
		desc: "Run a shell command (bash -lc) as user agent. Returns exit code, duration and the last 64 KB of stdout and stderr; the full output stays on disk (stdout_path/stderr_path, readable with fs_read). Use process_start or session_create for anything long-running.",
	},
		func(in ExecIn) time.Duration { return time.Duration(max(in.TimeoutS, 0)+60) * time.Second },
		func(ctx context.Context, c authn.Caller, in ExecIn) (ExecOut, error) {
			t, note := s.execTimeout(c, in.TimeoutS)
			res, err := s.exec.Run(ctx, execx.Request{Command: in.Command, CWD: in.CWD, Env: in.Env, Timeout: time.Duration(t) * time.Second, Stdin: in.Stdin, Client: c.Client})
			if err != nil && res.OutputRef == "" {
				return ExecOut{}, err
			}
			res.Stdout, res.Stderr = s.redact(ctx, res.Stdout), s.redact(ctx, res.Stderr)
			return ExecOut{Result: res, TimeoutS: t, Note: note}, err
		})

	addTool(s, toolSpec{
		name: "session_create", title: "Create an interactive session", scope: exec, openWorld: true,
		desc: "Start an interactive terminal session (a tmux window) that survives client disconnects and sandboxd restarts. Aaryn can watch it live with `headmesh-sandbox attach`.",
	},
		nil, func(ctx context.Context, c authn.Caller, in SessionCreateIn) (sessions.Info, error) {
			return s.sessions.Create(ctx, sessions.CreateOpts{Name: in.Name, CWD: in.CWD, Shell: in.Shell, Env: in.Env, Cols: in.Cols, Rows: in.Rows, Client: c.Client})
		})

	addTool(s, toolSpec{
		name: "session_send", title: "Type into a session", scope: exec, destructive: true, openWorld: true,
		desc: "Type text and/or tmux key names (C-c, Enter, Up…) into a running session.",
	},
		nil, func(ctx context.Context, _ authn.Caller, in SessionSendIn) (sessions.Info, error) {
			return s.sessions.Send(ctx, in.ID, in.Input, in.Enter, in.Keys)
		})

	addTool(s, toolSpec{
		name: "session_read", title: "Read session output", scope: read, readOnly: true, idempotent: true,
		desc: "Read a session's output from a byte offset, long-polling up to wait_ms for new output. Returns data, next_offset, alive and exit_code; truncated/more tell you how to fetch the rest.",
	},
		func(in SessionReadIn) time.Duration {
			return time.Duration(min(max(in.WaitMS, 0), 30000))*time.Millisecond + 30*time.Second
		},
		func(ctx context.Context, _ authn.Caller, in SessionReadIn) (sessions.ReadResult, error) {
			r, err := s.sessions.Read(ctx, in.ID, sessions.ReadOpts{Offset: in.Offset, MaxBytes: in.MaxBytes, Wait: time.Duration(in.WaitMS) * time.Millisecond, Raw: in.Raw, Screen: in.Screen})
			r.Data, r.Screen = s.redact(ctx, r.Data), s.redact(ctx, r.Screen)
			return r, err
		})

	addTool(s, toolSpec{
		name: "session_list", title: "List sessions", scope: read, readOnly: true, idempotent: true,
		desc: "All sessions with state running, exited(code) or lost (with the reason, e.g. container restarted at …).",
	},
		nil, func(ctx context.Context, _ authn.Caller, _ EmptyIn) (SessionListOut, error) {
			list, err := s.sessions.List(ctx)
			if list == nil {
				list = []sessions.Info{}
			}
			return SessionListOut{Sessions: list, Counts: sessions.Summary(list)}, err
		})

	addTool(s, toolSpec{
		name: "session_kill", title: "Signal or close a session", scope: exec, destructive: true,
		desc: "Send a signal to every process in the session (default HUP) and optionally close its window.",
	},
		nil, func(ctx context.Context, _ authn.Caller, in SessionKillIn) (sessions.Info, error) {
			return s.sessions.Kill(ctx, in.ID, in.Signal, in.Remove)
		})

	addTool(s, toolSpec{
		name: "session_resize", title: "Resize a session", scope: exec, idempotent: true,
		desc: "Change a session's terminal size.",
	},
		nil, func(ctx context.Context, _ authn.Caller, in SessionResizeIn) (sessions.Info, error) {
			return s.sessions.Resize(ctx, in.ID, in.Cols, in.Rows)
		})

	addTool(s, toolSpec{
		name: "process_start", title: "Start a background process", scope: exec, openWorld: true,
		desc: "Start a detached command (bash -lc, own session and process group), logging stdout and stderr to a file. It keeps running after this call and after client disconnects.",
	},
		nil, func(ctx context.Context, c authn.Caller, in ProcessStartIn) (procs.Info, error) {
			return s.procs.Start(ctx, procs.StartOpts{Name: in.Name, Command: in.Command, CWD: in.CWD, Env: in.Env, Client: c.Client})
		})

	addTool(s, toolSpec{
		name: "process_list", title: "List background processes", scope: read, readOnly: true, idempotent: true,
		desc: "Background processes with state running, exited(code) or lost.",
	},
		nil, func(context.Context, authn.Caller, EmptyIn) (ProcessListOut, error) {
			list, err := s.procs.List()
			if list == nil {
				list = []procs.Info{}
			}
			return ProcessListOut{Processes: list}, err
		})

	addTool(s, toolSpec{
		name: "process_logs", title: "Read process logs", scope: read, readOnly: true, idempotent: true,
		desc: "The last lines of a process log, or bytes from an offset (since).",
	},
		nil, func(ctx context.Context, _ authn.Caller, in ProcessLogsIn) (procs.LogsResult, error) {
			since := int64(-1)
			if in.Since != nil {
				since = max(*in.Since, 0)
			}
			r, err := s.procs.Logs(in.ID, in.Tail, since, in.MaxBytes)
			r.Data = s.redact(ctx, r.Data)
			return r, err
		})

	addTool(s, toolSpec{
		name: "process_stop", title: "Stop a background process", scope: exec, destructive: true, idempotent: true,
		desc: "Signal the process group (default TERM), then KILL after grace_s.",
	},
		func(ProcessStopIn) time.Duration { return 90 * time.Second },
		func(ctx context.Context, _ authn.Caller, in ProcessStopIn) (procs.Info, error) {
			return s.procs.Stop(ctx, in.ID, in.Signal, time.Duration(in.GraceS)*time.Second)
		})

	addTool(s, toolSpec{
		name: "fs_read", title: "Read a file", scope: read, readOnly: true, idempotent: true,
		desc: "Read a text file with line numbers (like cat -n). Binary files are detected and not dumped. Up to 2 MB per call; next_offset continues.",
	},
		nil, func(ctx context.Context, _ authn.Caller, in FsReadIn) (fsx.ReadResult, error) {
			r, err := s.fs.Read(in.Path, in.Offset, in.Limit)
			r.Content = s.redact(ctx, r.Content)
			return r, err
		})

	addTool(s, toolSpec{
		name: "fs_write", title: "Write a file", scope: write, destructive: true, idempotent: true,
		desc: "Replace a file atomically (temp file + rename), keeping its mode and owner. Refuses read-only mounts and names the writable ones.",
	},
		nil, func(_ context.Context, _ authn.Caller, in FsWriteIn) (fsx.WriteResult, error) {
			return s.fs.Write(in.Path, in.Content, in.Encoding, in.Mode, in.CreateDirs)
		})

	addTool(s, toolSpec{
		name: "fs_edit", title: "Edit a file", scope: write, destructive: true,
		desc: "Replace exact text in a file. old_string must match exactly once unless replace_all is set.",
	},
		nil, func(_ context.Context, _ authn.Caller, in FsEditIn) (fsx.WriteResult, error) {
			return s.fs.Edit(in.Path, in.OldString, in.NewString, in.ReplaceAll)
		})

	addTool(s, toolSpec{
		name: "fs_list", title: "List a directory", scope: read, readOnly: true, idempotent: true,
		desc: "List entries to a depth, filtered by glob. Skips hidden entries unless include_hidden; never descends into .git.",
	},
		nil, func(_ context.Context, _ authn.Caller, in FsListIn) (fsx.ListResult, error) {
			return s.fs.List(in.Path, in.Depth, in.Glob, in.IncludeHidden, in.Limit)
		})

	addTool(s, toolSpec{
		name: "fs_search", title: "Search files", scope: read, readOnly: true, idempotent: true,
		desc: "Search file contents with ripgrep (respects .gitignore). Fixed string by default; set regex for a regular expression.",
	},
		func(FsSearchIn) time.Duration { return 75 * time.Second },
		func(ctx context.Context, _ authn.Caller, in FsSearchIn) (fsx.SearchResult, error) {
			r, err := s.fs.Search(ctx, fsx.SearchOpts{Pattern: in.Pattern, Path: in.Path, Glob: in.Glob, Regex: in.Regex, CaseInsensitive: in.CaseInsensitive, MaxResults: in.MaxResults, Context: in.Context})
			for i := range r.Matches {
				r.Matches[i].Text = s.redact(ctx, r.Matches[i].Text)
			}
			return r, err
		})

	addTool(s, toolSpec{
		name: "fs_stat", title: "Describe a path", scope: read, readOnly: true, idempotent: true,
		desc: "Type, size, mode, owner, mtime, symlink target, mount point, and whether the mount is read-only.",
	},
		nil, func(_ context.Context, _ authn.Caller, in PathIn) (fsx.StatResult, error) { return s.fs.Stat(in.Path) })

	addTool(s, toolSpec{
		name: "fs_move", title: "Move or rename", scope: write, destructive: true,
		desc: "Rename within one filesystem. Refuses to replace an existing destination unless overwrite is set.",
	},
		nil, func(_ context.Context, _ authn.Caller, in FsMoveIn) (fsx.MoveResult, error) {
			return s.fs.Move(in.Src, in.Dst, in.Overwrite)
		})

	addTool(s, toolSpec{
		name: "fs_delete", title: "Delete to trash", scope: write, destructive: true,
		desc: "Move a file or directory to <mount>/.headmesh-trash/<timestamp>/ so it can be restored with fs_move.",
	},
		nil, func(_ context.Context, _ authn.Caller, in FsDeleteIn) (fsx.DeleteResult, error) {
			return s.fs.Delete(in.Path, in.Recursive)
		})

	addTool(s, toolSpec{
		name: "git_status", title: "Git status", scope: read, readOnly: true, idempotent: true,
		desc: "Branch, ahead/behind, staged/unstaged/untracked files (porcelain v2), any rebase or merge in progress, and other agents active in the repo.",
	},
		nil, func(ctx context.Context, _ authn.Caller, in RepoIn) (gitx.Status, error) {
			p, err := s.repo(in.Repo)
			if err != nil {
				return gitx.Status{}, err
			}
			st, err := s.git.Status(ctx, p)
			if err != nil {
				return st, err
			}
			st.Agents = s.agents.ActiveIn(p, "")
			return st, nil
		})

	addTool(s, toolSpec{
		name: "git_sync", title: "Fetch and rebase", scope: write, openWorld: true,
		desc: "git fetch, then pull --rebase --autostash. On conflict it reports the files and stops (aborting the rebase by default). Never force-pushes.",
	},
		func(GitSyncIn) time.Duration { return 5 * time.Minute },
		func(ctx context.Context, _ authn.Caller, in GitSyncIn) (gitx.SyncResult, error) {
			p, err := s.repo(in.Repo)
			if err != nil {
				return gitx.SyncResult{}, err
			}
			abort := in.AbortOnConflict == nil || *in.AbortOnConflict
			return s.git.Sync(ctx, p, abort)
		})

	addTool(s, toolSpec{
		name: "git_push_safe", title: "Rebase and push", scope: write, openWorld: true,
		desc: "Rebase the current branch on <remote>/<branch>, run the hooks, and push without force. Conflicts and rejected pushes are reported, never forced.",
	},
		func(GitPushIn) time.Duration { return 5 * time.Minute },
		func(ctx context.Context, _ authn.Caller, in GitPushIn) (gitx.PushResult, error) {
			p, err := s.repo(in.Repo)
			if err != nil {
				return gitx.PushResult{}, err
			}
			return s.git.PushSafe(ctx, p, in.Remote, in.Branch)
		})

	addTool(s, toolSpec{
		name: "artifact_put", title: "Store an artifact", scope: write,
		desc: "Store text, base64 content, or a copy of a file. Returns an id and a signed download URL valid for 1 hour (tailnet only).",
	},
		nil, func(_ context.Context, c authn.Caller, in ArtifactPutIn) (artifacts.Meta, error) {
			set := 0
			for _, v := range []string{in.Content, in.ContentBase64, in.Path} {
				if v != "" {
					set++
				}
			}
			if set != 1 {
				return artifacts.Meta{}, errors.New("give exactly one of content, content_base64 or path")
			}
			switch {
			case in.Path != "":
				p, err := s.fs.Resolve(in.Path)
				if err != nil {
					return artifacts.Meta{}, err
				}
				return s.artifacts.Put(in.Name, nil, p, c.Client)
			case in.ContentBase64 != "":
				b, err := base64.StdEncoding.DecodeString(in.ContentBase64)
				if err != nil {
					return artifacts.Meta{}, fmt.Errorf("content_base64: %w", err)
				}
				return s.artifacts.Put(in.Name, b, "", c.Client)
			default:
				return s.artifacts.Put(in.Name, []byte(in.Content), "", c.Client)
			}
		})

	addTool(s, toolSpec{
		name: "artifact_list", title: "List artifacts", scope: read, readOnly: true, idempotent: true,
		desc: "Artifacts, newest first, with fresh signed URLs.",
	},
		nil, func(_ context.Context, _ authn.Caller, in ArtifactListIn) (ArtifactListOut, error) {
			list, err := s.artifacts.List(in.Prefix, in.Limit)
			return ArtifactListOut{Artifacts: list}, err
		})

	addTool(s, toolSpec{
		name: "artifact_get", title: "Read an artifact", scope: read, readOnly: true, idempotent: true,
		desc: "Read an artifact from an offset (text as UTF-8, binary as base64), with a fresh signed URL.",
	},
		nil, func(ctx context.Context, c authn.Caller, in ArtifactGetIn) (artifacts.Content, error) {
			maxB := in.MaxBytes
			if c.Tunnel() && (maxB <= 0 || maxB > 2<<20) {
				maxB = 2 << 20 // keep tunnel responses under 8 MiB after encoding
			}
			r, err := s.artifacts.Read(in.ID, in.Offset, maxB)
			if r.Encoding == "utf-8" {
				r.Data = s.redact(ctx, r.Data)
			}
			return r, err
		})

	addTool(s, toolSpec{
		name: "net_check", title: "Check reachability", scope: read, readOnly: true, idempotent: true, openWorld: true,
		desc: "Resolve a name with the system resolver and MagicDNS, tailscale ping it (direct or DERP), and test a TCP port.",
	},
		func(NetCheckIn) time.Duration { return 20 * time.Second },
		func(ctx context.Context, _ authn.Caller, in NetCheckIn) (netcheck.Result, error) {
			return s.net.Check(ctx, in.Target, in.Port)
		})

	addTool(s, toolSpec{
		name: "agents_register", title: "Register this agent", scope: read,
		desc: "Announce your entry point, task and repos. Returns the other agents active in the same repos and the protocol to follow. Heartbeat at least every 2 minutes.",
	},
		nil, func(_ context.Context, c authn.Caller, in AgentsRegisterIn) (agents.RegisterResult, error) {
			repos := make([]string, 0, len(in.Repos))
			for _, r := range in.Repos {
				p, err := s.repo(r)
				if err != nil {
					return agents.RegisterResult{}, err
				}
				repos = append(repos, p)
			}
			return s.agents.Register(in.Agent, in.Task, repos, c.Client, in.Session)
		})

	addTool(s, toolSpec{
		name: "agents_heartbeat", title: "Agent heartbeat", scope: read, idempotent: true,
		desc: "Keep a registration alive (active means a heartbeat in the last 2 minutes).",
	},
		nil, func(_ context.Context, _ authn.Caller, in AgentsHeartbeatIn) (agents.RegisterResult, error) {
			return s.agents.Heartbeat(in.ID, in.Task)
		})

	addTool(s, toolSpec{
		name: "agents_list", title: "List agents", scope: read, readOnly: true, idempotent: true,
		desc: "Registered agents (optionally for one repo) with whether each is active.",
	},
		nil, func(_ context.Context, _ authn.Caller, in AgentsListIn) (AgentsListOut, error) {
			repo := ""
			if in.Repo != "" {
				p, err := s.repo(in.Repo)
				if err != nil {
					return AgentsListOut{}, err
				}
				repo = p
			}
			list, err := s.agents.List(repo)
			return AgentsListOut{Agents: list}, err
		})

	addTool(s, toolSpec{
		name: "agents_unregister", title: "Unregister this agent", scope: read, idempotent: true,
		desc: "Remove a registration and release its repo locks when the task ends.",
	},
		nil, func(_ context.Context, _ authn.Caller, in IDIn) (OKOut, error) {
			if err := s.agents.Unregister(in.ID); err != nil {
				return OKOut{}, err
			}
			return OKOut{OK: true, Message: "unregistered " + in.ID}, nil
		})
}
