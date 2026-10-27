package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/sessions"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// Mounts is the sandbox://mounts document.
type Mounts struct {
	Mode      string   `json:"mode"`
	ReadWrite []string `json:"read_write"`
	ReadOnly  []string `json:"read_only"`
	HostRoot  string   `json:"host_root,omitempty"`
	Masks     []string `json:"masks"`
	Denylist  []string `json:"secret_denylist"`
	Home      string   `json:"home"`
}

func (s *Server) mounts() Mounts {
	m := Mounts{
		Mode: string(s.opts.Mode), ReadWrite: s.cfg.RWMounts(s.opts.Mode), ReadOnly: s.cfg.ROMounts(s.opts.Mode),
		Masks: s.cfg.Mounts.Masks, Denylist: s.cfg.Denylist().Patterns(), Home: s.opts.Home,
	}
	if s.opts.Mode == config.ModeYOLO {
		m.HostRoot = "/host (read-only, recursive)"
	}
	if m.Masks == nil {
		m.Masks = []string{}
	}
	return m
}

func callerOf(req *mcp.ReadResourceRequest) authn.Caller {
	var ti *auth.TokenInfo
	if req.Extra != nil {
		ti = req.Extra.TokenInfo
	}
	return authn.FromTokenInfo(ti)
}

func (s *Server) resource(name string, fn func(ctx context.Context, uri string) (*mcp.ResourceContents, error)) mcp.ResourceHandler {
	sp := toolSpec{name: "resource:" + name, scope: authn.ScopeRead}
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		out, err := s.invoke(ctx, sp, callerOf(req), req.Params.URI, 30*time.Second, func(ctx context.Context) (any, error) {
			return fn(ctx, req.Params.URI)
		})
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{out.(*mcp.ResourceContents)}}, nil
	}
}

func jsonContents(uri string, v any) (*mcp.ResourceContents, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", uri, err)
	}
	return &mcp.ResourceContents{URI: uri, MIMEType: "application/json", Text: string(b)}, nil
}

func (s *Server) registerResources() {
	s.mcp.AddResource(&mcp.Resource{
		URI: "sandbox://status", Name: "status", Title: "Sandbox status", MIMEType: "application/json",
		Description: "Same report as the sandbox_status tool (without reach probes).",
	},
		s.resource("status", func(ctx context.Context, uri string) (*mcp.ResourceContents, error) {
			return jsonContents(uri, s.status.Collect(ctx, status.Options{}))
		}))
	s.mcp.AddResource(&mcp.Resource{
		URI: "sandbox://mounts", Name: "mounts", Title: "Mounts and masks", MIMEType: "application/json",
		Description: "Read-write and read-only mounts for the active mode, masked secret paths and the denylist.",
	},
		s.resource("mounts", func(_ context.Context, uri string) (*mcp.ResourceContents, error) {
			return jsonContents(uri, s.mounts())
		}))
	s.mcp.AddResource(&mcp.Resource{
		URI: "sandbox://guidelines", Name: "guidelines", Title: "Agent guidelines", MIMEType: "text/markdown",
		Description: "The multi-agent working agreement and the active mode.",
	},
		s.resource("guidelines", func(_ context.Context, uri string) (*mcp.ResourceContents, error) {
			return &mcp.ResourceContents{URI: uri, MIMEType: "text/markdown", Text: s.guideText}, nil
		}))
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "sandbox://sessions/{id}/log", Name: "session-log", Title: "Session log",
		MIMEType: "text/plain", Description: "The last 256 KB of a session's output, escapes stripped.",
	},
		s.resource("session-log", func(ctx context.Context, uri string) (*mcp.ResourceContents, error) {
			id := strings.TrimSuffix(strings.TrimPrefix(uri, "sandbox://sessions/"), "/log")
			r, err := s.sessions.Read(ctx, id, sessions.ReadOpts{Offset: -256 << 10, MaxBytes: 256 << 10})
			if err != nil {
				return nil, mcp.ResourceNotFoundError(uri)
			}
			return &mcp.ResourceContents{URI: uri, MIMEType: "text/plain", Text: s.redact(ctx, r.Data)}, nil
		}))
	s.mcp.AddPrompt(&mcp.Prompt{
		Name: "multi-agent-protocol", Title: "Multi-agent protocol",
		Description: "How to work in the shared sandbox when other agents may be active.",
	},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: "Headmesh sandbox working agreement (" + string(s.opts.Mode) + " mode)",
				Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: s.guideText}}},
			}, nil
		})
}
