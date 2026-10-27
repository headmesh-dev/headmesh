// Package guidelines renders the agent working agreement (plan §6.10) into
// ~/.claude/CLAUDE.md, ~/.codex/AGENTS.md, the MCP instructions and the
// shell banner. It owns only a marked block in each file, so anything Aaryn
// or an agent adds outside the block survives re-rendering.
package guidelines

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

//go:embed default.md.tmpl
var defaultTemplate string

// Markers delimit the managed block.
const (
	BeginMarker = "<!-- headmesh:guidelines"
	EndMarker   = "<!-- /headmesh:guidelines -->"
)

// Repo is a name → path pair for the template.
type Repo struct {
	Name string
	Path string
}

// Data feeds the template.
type Data struct {
	Hostname  string
	Mode      string
	YOLO      bool
	ExpiresAt string
	RepoLock  string
	Writable  []string
	ReadOnly  []string
	Repos     []Repo
}

// FromConfig builds template data for a mode.
func FromConfig(cfg *config.Config, mode config.Mode, expires time.Time) Data {
	d := Data{
		Hostname: cfg.Hostname(mode),
		Mode:     string(mode),
		YOLO:     mode == config.ModeYOLO,
		RepoLock: cfg.RepoLock,
		Writable: cfg.RWMounts(mode),
		ReadOnly: cfg.ROMounts(mode),
	}
	if d.YOLO {
		d.ReadOnly = append(d.ReadOnly, "/host (the host's root filesystem)")
	}
	if !expires.IsZero() {
		d.ExpiresAt = expires.Local().Format("2006-01-02 15:04 MST")
	}
	for name, p := range cfg.Repos {
		d.Repos = append(d.Repos, Repo{Name: name, Path: p})
	}
	slices.SortFunc(d.Repos, func(a, b Repo) int { return strings.Compare(a.Name, b.Name) })
	return d
}

// Render executes tmpl (the built-in template when empty).
func Render(tmpl string, d Data) (string, error) {
	if tmpl == "" {
		tmpl = defaultTemplate
	}
	t, err := template.New("guidelines").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, BeginMarker) {
		out = BeginMarker + " (managed by headmesh-sandboxd; edit outside this block) -->\n" + out
	}
	return strings.TrimRight(out, "\n") + "\n" + EndMarker + "\n", nil
}

// Merge replaces the managed block in existing (or prepends it).
func Merge(existing, block string) string {
	start := strings.Index(existing, BeginMarker)
	end := strings.Index(existing, EndMarker)
	if start >= 0 && end > start {
		return existing[:start] + block + strings.TrimPrefix(existing[end+len(EndMarker):], "\n")
	}
	if strings.TrimSpace(existing) == "" {
		return block
	}
	return block + "\n" + existing
}

// WriteInto merges the block into path, creating parent directories.
func WriteInto(path, block string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	old, err := os.ReadFile(path) //nolint:gosec // G304: ~/.claude/CLAUDE.md or ~/.codex/AGENTS.md
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	merged := Merge(string(old), block)
	if merged == string(old) {
		return nil
	}
	tmp := path + ".hm-tmp"
	if err := os.WriteFile(tmp, []byte(merged), 0o600); err != nil { //nolint:gosec // G703: path is the agent's own guidelines file
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Banner is the login-shell banner.
func Banner(d Data, others string) string {
	mode := "REGULAR"
	if d.YOLO {
		mode = "YOLO"
		if d.ExpiresAt != "" {
			mode += " until " + d.ExpiresAt
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Headmesh sandbox %s · mode %s\n", d.Hostname, mode)
	if others != "" {
		b.WriteString(others + "\n")
	}
	b.WriteString("Guidelines: ~/.claude/CLAUDE.md · status: hm status · attach: tmux -L headmesh attach\n")
	return b.String()
}

// Instructions is the MCP `instructions` text: mode first, then the protocol.
func Instructions(d Data) string {
	mode := "Regular"
	if d.YOLO {
		mode = "YOLO (temporary; host root is read-only at /host; do not ingest untrusted content)"
		if d.ExpiresAt != "" {
			mode += ", halts at " + d.ExpiresAt
		}
	}
	return fmt.Sprintf(`Headmesh agent sandbox %s. Active mode: %s.
Shared by several agents. Call agents_register first (agent, task, repos), then work.
When another agent is active in the same repo: stick to your own files, commit small and often, run git_sync (pull --rebase) before git_push_safe, don't reformat files you aren't changing, and ask Aaryn instead of overwriting changes you didn't make. Work on main unless asked for a branch; never force-push.
Use session_* (tmux) or process_start for anything that must outlive this connection; sandbox_status shows whether earlier sessions are running, exited or lost.
Every response says if it was truncated and how to fetch the rest. Full guidelines: resource sandbox://guidelines.`, d.Hostname, mode)
}
