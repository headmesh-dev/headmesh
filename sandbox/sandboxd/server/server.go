// Package server wires headmesh-sandboxd together: the MCP server (stateless
// Streamable HTTP for 2026-07-28, with the 2025-11-25 handshake still
// answered), bearer/OAuth authentication, per-client limits, the audit log,
// signed artifact downloads, and the unix-socket API used by the host CLI.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/agents"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/artifacts"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/audit"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/execx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/fsx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/gitx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/guidelines"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/limits"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procs"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/redact"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/sessions"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

// MaxRequestBytes is the request body limit (plan §6.8).
const MaxRequestBytes = 10 << 20

// Options configures a Server. Zero paths take the container defaults.
type Options struct {
	Config        *config.Config
	Mode          config.Mode
	YOLOExpiresAt time.Time
	Version       string

	Home         string // /home/agent
	SessionsDir  string // /var/lib/headmesh/sessions
	ArtifactsDir string // /var/lib/headmesh/artifacts
	LogsDir      string // /var/log/headmesh
	RunDir       string // /run/headmesh
	TokensPath   string // /etc/headmesh/config/tokens.json

	GuidelinesTemplate string // template file; empty = embedded copy
	CacheDirs          []string
	S6ServiceDir       string

	Boot      bootid.Source
	Tmux      *tmuxctl.Client
	Tailscale netcheck.Tailscale
	Env       []string // environment for commands; os.Environ() when empty

	// Test knobs.
	NoLogin  bool
	SkipPing bool
	Logger   *slog.Logger
}

func (o *Options) defaults() {
	set := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	set(&o.Home, "/home/agent")
	set(&o.SessionsDir, "/var/lib/headmesh/sessions")
	set(&o.ArtifactsDir, "/var/lib/headmesh/artifacts")
	set(&o.LogsDir, "/var/log/headmesh")
	set(&o.RunDir, "/run/headmesh")
	set(&o.TokensPath, "/etc/headmesh/config/tokens.json")
	if o.Boot == nil {
		o.Boot = bootid.Proc
	}
	if o.Tmux == nil {
		o.Tmux = &tmuxctl.Client{}
	}
	if o.Mode == "" {
		o.Mode = config.ModeRegular
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if len(o.CacheDirs) == 0 {
		for _, c := range []string{"mise", "uv", "npm", "pnpm", "go", "cargo"} {
			o.CacheDirs = append(o.CacheDirs, "/cache/"+c)
		}
	}
}

// Server is a running sandboxd.
type Server struct {
	opts      Options
	cfg       *config.Config
	startedAt time.Time

	sessions  *sessions.Manager
	procs     *procs.Manager
	exec      *execx.Runner
	fs        *fsx.FS
	git       *gitx.Git
	artifacts *artifacts.Store
	agents    *agents.Registry
	net       *netcheck.Checker
	tokens    *authn.Store
	rs        authn.ResourceServer
	audit     *audit.Log
	limiter   *limits.Limiter
	redactor  *redact.Redactor
	status    *status.Collector
	guide     guidelines.Data
	guideText string

	mcp   *mcp.Server
	tools map[string]*restTool
}

// New builds a server and its state directories.
func New(o Options) (*Server, error) {
	o.defaults()
	cfg := o.Config
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	for _, d := range []string{o.SessionsDir, o.ArtifactsDir, o.LogsDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("state dir %s: %w", d, err)
		}
	}
	al, err := audit.Open(filepath.Join(o.LogsDir, "audit.jsonl"), string(o.Mode))
	if err != nil {
		return nil, err
	}
	key, err := artifacts.LoadOrCreateKey(filepath.Join(o.SessionsDir, ".keys", "artifact-url.key"))
	if err != nil {
		return nil, err
	}
	env := o.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	env = append(slices.Clone(env), "HEADMESH_MODE="+string(o.Mode))
	s := &Server{
		opts: o, cfg: cfg, startedAt: time.Now(),
		sessions: &sessions.Manager{
			Dir: filepath.Join(o.SessionsDir, "tty"), Tmux: o.Tmux, Boot: o.Boot, DefaultCWD: o.Home,
			DefaultShell: []string{"bash", "-l"}, Env: map[string]string{"HEADMESH_MODE": string(o.Mode)},
		},
		procs: &procs.Manager{Dir: filepath.Join(o.SessionsDir, "procs"), Boot: o.Boot, DefaultCWD: o.Home, Env: env, NoLogin: o.NoLogin},
		exec:  &execx.Runner{OutputDir: filepath.Join(o.SessionsDir, "exec"), DefaultCWD: o.Home, Env: env, NoLogin: o.NoLogin},
		fs: &fsx.FS{
			Home: o.Home, WritableRoots: append(cfg.WritableRoots(o.Mode), o.Home),
			FallbackTrash: filepath.Join(o.SessionsDir, "trash"),
		},
		git:       &gitx.Git{Env: env},
		artifacts: &artifacts.Store{Dir: o.ArtifactsDir, BaseURL: cfg.Sandboxd.PublicURL, Key: key},
		agents:    &agents.Registry{Path: filepath.Join(o.SessionsDir, "agents.json"), LockMode: cfg.RepoLock},
		net:       &netcheck.Checker{TS: o.Tailscale, SkipPing: o.SkipPing},
		tokens:    authn.NewStore(o.TokensPath),
		audit:     al,
		limiter:   limits.New(cfg.Sandboxd.MaxConcurrentPerClient, cfg.Sandboxd.RatePerMinute, 30*time.Second),
		tools:     map[string]*restTool{},
	}
	if cfg.Sandboxd.RedactOutput {
		s.redactor = redact.New()
	}
	oa := cfg.Sandboxd.OAuth
	resource := oa.Resource
	if resource == "" {
		resource = strings.TrimSuffix(cfg.Sandboxd.PublicURL, "/") + "/mcp"
	}
	s.rs = authn.StubResourceServer{Resource: resource, AuthorizationServers: oa.AuthorizationServers, JWKSURL: oa.JWKSURL}

	s.guide = guidelines.FromConfig(cfg, o.Mode, o.YOLOExpiresAt)
	tmpl := ""
	if o.GuidelinesTemplate != "" {
		if b, err := os.ReadFile(o.GuidelinesTemplate); err == nil {
			tmpl = string(b)
		}
	}
	if s.guideText, err = guidelines.Render(tmpl, s.guide); err != nil {
		return nil, err
	}

	var repos []string
	for _, p := range cfg.Repos {
		repos = append(repos, p)
	}
	slices.Sort(repos)
	reach := slices.Clone(cfg.Mode.Regular.Reach)
	if o.Mode == config.ModeYOLO {
		reach = nil
	}
	s.status = &status.Collector{
		Name: cfg.Hostname(o.Mode), Mode: string(o.Mode), YOLOExpiresAt: o.YOLOExpiresAt, StartedAt: s.startedAt,
		Boot: o.Boot, Sessions: s.sessions, Procs: s.procs, Agents: s.agents, Git: s.git, Repos: slices.Compact(repos),
		Tailscale: o.Tailscale, Net: s.net, Reach: reach, Tmux: o.Tmux,
		DNSCheckFile: filepath.Join(o.RunDir, "dns-check.json"), GPUMode: cfg.GPU, CacheDirs: o.CacheDirs,
		Entrypoints: cfg.EnabledEntrypoints(), RunnerHealth: "http://127.0.0.1:8081/healthz", S6ServiceDir: o.S6ServiceDir,
		TokensActive: func() (int, error) { return s.tokens.Count(), s.tokens.LoadError() },
	}

	// Stateless mode logs a connect/disconnect pair per request at INFO.
	sdkLog := slog.New(levelFilter{h: o.Logger.Handler(), min: slog.LevelWarn})
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name: "headmesh-sandboxd", Title: "Headmesh agent sandbox", Version: o.Version,
		Description: "Shell, files, git, sessions and tailnet access inside Aaryn's persistent agent sandbox.",
	}, &mcp.ServerOptions{
		Instructions: guidelines.Instructions(s.guide),
		Logger:       sdkLog,
		SchemaCache:  mcp.NewSchemaCache(),
	})
	s.registerTools()
	s.registerResources()
	return s, nil
}

// Close releases the audit log.
func (s *Server) Close() error { return s.audit.Close() }

// MCP exposes the MCP server (tests use in-memory transports).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Tokens exposes the token store.
func (s *Server) Tokens() *authn.Store { return s.tokens }

func (s *Server) streamable() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		Logger:              slog.New(levelFilter{h: s.opts.Logger.Handler(), min: slog.LevelWarn}),
		MaxRequestBodyBytes: MaxRequestBytes,
	})
}

// resourceMetadataURL is where clients find the RFC 9728 document.
func (s *Server) resourceMetadataURL() string {
	return strings.TrimSuffix(s.cfg.Sandboxd.PublicURL, "/") + "/.well-known/oauth-protected-resource/mcp"
}

// Handler serves the tailnet listener: /mcp (authenticated), RFC 9728
// metadata, signed artifact downloads and a bare /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mcpH := authn.Middleware(s.tokens, s.rs, s.resourceMetadataURL(), s.streamable())
	mux.Handle("/mcp", s.originCheck(mcpH))
	meta := authn.MetadataHandler(s.rs)
	mux.Handle("/.well-known/oauth-protected-resource", meta)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", meta)
	mux.Handle("/artifacts/", s.artifacts.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
	})
	return mux
}

// originCheck rejects browser requests from origins not in allowed_origins.
func (s *Server) originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.cfg.Sandboxd.AllowedOrigins, o) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// UnixHandler serves the host CLI over the bind-mounted unix socket: the
// same MCP endpoint plus a small JSON API. Access is governed by the
// socket's file permissions (UID 1000 on both sides).
func (s *Server) UnixHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.streamable())
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		rep := s.status.Collect(r.Context(), status.Options{ProbeReach: r.URL.Query().Get("probe_reach") == "1"})
		writeJSON(w, http.StatusOK, rep)
	})
	mux.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, _ *http.Request) {
		names := make([]string, 0, len(s.tools))
		for n := range s.tools {
			names = append(names, n)
		}
		slices.Sort(names)
		writeJSON(w, http.StatusOK, map[string]any{"tools": names})
	})
	mux.HandleFunc("POST /v1/call/{tool}", s.handleCall)
	return authn.InjectUnixBearer(authn.Middleware(s.tokens, nil, "", mux))
}

// Health is the /v1/health body.
type Health struct {
	OK       bool     `json:"ok"`
	Mode     string   `json:"mode"`
	Problems []string `json:"problems"`
}

// CheckHealth runs the light status probes.
func (s *Server) CheckHealth(ctx context.Context) Health {
	rep := s.status.Collect(ctx, status.Options{Light: true})
	h := Health{Mode: rep.Mode, Problems: []string{}}
	for _, p := range rep.Problems {
		// Entry points that need Aaryn's first-run setup don't make the container unhealthy.
		if strings.HasPrefix(p, "claude-") || strings.HasPrefix(p, "openai-tunnel") {
			continue
		}
		h.Problems = append(h.Problems, p)
	}
	h.OK = len(h.Problems) == 0
	return h
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	h := s.CheckHealth(ctx)
	code := http.StatusOK
	if !h.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tools[r.PathValue("tool")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown tool " + r.PathValue("tool")})
		return
	}
	raw, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	caller := authn.FromTokenInfo(auth.TokenInfoFromContext(r.Context()))
	out, err := t.call(r.Context(), caller, raw)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, errForbidden):
			code = http.StatusForbidden
		case errors.Is(err, limits.ErrRateLimited), errors.Is(err, limits.ErrTooManyConcurrent):
			code = http.StatusTooManyRequests
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": out})
}

func readBody(r *http.Request) (json.RawMessage, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, MaxRequestBytes))
	if err != nil {
		return nil, fmt.Errorf("request body: %w", err)
	}
	if n == 0 {
		return json.RawMessage("{}"), nil
	}
	return buf.Bytes(), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

var errForbidden = errors.New("forbidden")

// toolSpec describes a tool's metadata and policy.
type toolSpec struct {
	name, title, desc string
	scope             string
	readOnly          bool
	destructive       bool
	idempotent        bool
	openWorld         bool
}

func (sp toolSpec) annotations() *mcp.ToolAnnotations {
	d, o := sp.destructive, sp.openWorld
	return &mcp.ToolAnnotations{Title: sp.title, ReadOnlyHint: sp.readOnly, DestructiveHint: &d, IdempotentHint: sp.idempotent, OpenWorldHint: &o}
}

type restTool struct {
	spec toolSpec
	call func(ctx context.Context, c authn.Caller, raw json.RawMessage) (any, error)
}

// callTimeout bounds a call when the tool doesn't choose its own.
const callTimeout = 2 * time.Minute

// addTool registers fn as an MCP tool and as POST /v1/call/<name>.
func addTool[In, Out any](s *Server, sp toolSpec, timeout func(In) time.Duration, fn func(ctx context.Context, c authn.Caller, in In) (Out, error)) {
	run := func(ctx context.Context, caller authn.Caller, in In) (Out, error) {
		var zero Out
		d := callTimeout
		if timeout != nil {
			d = timeout(in)
		}
		out, err := s.invoke(ctx, sp, caller, in, d, func(ctx context.Context) (any, error) { return fn(ctx, caller, in) })
		if err != nil {
			return zero, err
		}
		return out.(Out), nil
	}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: sp.name, Title: sp.title, Description: sp.desc, Annotations: sp.annotations()},
		func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			var ti *auth.TokenInfo
			if req.Extra != nil {
				ti = req.Extra.TokenInfo
			}
			out, err := run(ctx, authn.FromTokenInfo(ti), in)
			return nil, out, err
		})
	s.tools[sp.name] = &restTool{spec: sp, call: func(ctx context.Context, c authn.Caller, raw json.RawMessage) (any, error) {
		var in In
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("arguments for %s: %w", sp.name, err)
		}
		return run(ctx, c, in)
	}}
}

// invoke enforces scope, limits, deadline and response size, and audits.
func (s *Server) invoke(ctx context.Context, sp toolSpec, c authn.Caller, args any, timeout time.Duration, fn func(context.Context) (any, error)) (out any, err error) {
	start := time.Now()
	result := audit.ResultOK
	defer func() {
		e := audit.Entry{
			Client: c.Client, Route: c.Route, Tool: sp.name, ArgsSHA256: audit.Digest(args),
			DurationMS: time.Since(start).Milliseconds(), Result: result,
		}
		if err != nil {
			e.Error = truncate(err.Error(), 300)
		}
		if werr := s.audit.Write(e); werr != nil {
			s.opts.Logger.Error("audit write failed", "err", werr)
		}
	}()
	if !c.Has(sp.scope) {
		result = audit.ResultDenied
		return nil, fmt.Errorf("%w: %s needs scope %s; token %q has %v", errForbidden, sp.name, sp.scope, c.Client, c.Scopes)
	}
	release, err := s.limiter.Acquire(ctx, c.Client)
	if err != nil {
		result = audit.ResultRateLimited
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err = fn(ctx)
	if err != nil {
		result = audit.ResultError
		return nil, err
	}
	if err = s.checkSize(c, out); err != nil {
		result = audit.ResultError
		return nil, err
	}
	return out, nil
}

// checkSize keeps responses under the route's cap. MCP results carry the
// JSON twice (structuredContent and text), so the budget is halved.
func (s *Server) checkSize(c authn.Caller, out any) error {
	limit := s.cfg.Sandboxd.MaxResponseBytes
	if c.Tunnel() {
		limit = s.cfg.Sandboxd.TunnelMaxResponseBytes
	}
	b, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if 2*len(b) > limit {
		return fmt.Errorf("result is %d bytes, over the %d byte response limit for route %q; ask for less (max_bytes, limit, tail or offset)", len(b), limit/2, c.Route)
	}
	return nil
}

func (s *Server) redact(ctx context.Context, str string) string {
	if s.redactor == nil {
		return str
	}
	return s.redactor.String(ctx, str)
}

// levelFilter drops records below min.
type levelFilter struct {
	h   slog.Handler
	min slog.Level
}

func (f levelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min && f.h.Enabled(ctx, l)
}

func (f levelFilter) Handle(ctx context.Context, r slog.Record) error {
	return f.h.Handle(ctx, r) //nolint:wrapcheck // pass-through
}

func (f levelFilter) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFilter{h: f.h.WithAttrs(a), min: f.min}
}

func (f levelFilter) WithGroup(n string) slog.Handler {
	return levelFilter{h: f.h.WithGroup(n), min: f.min}
}

// EnsureTmux creates the shared tmux session (the s6 tmux service runs the server).
func (s *Server) EnsureTmux(ctx context.Context) error {
	return s.opts.Tmux.EnsureSession(ctx, s.opts.Home)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
