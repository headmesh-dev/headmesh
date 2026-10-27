// Package config loads and validates sandbox.toml, the single source of truth
// for the agent sandbox: mounts, secret denylist, repo mapping, reach list,
// GPU and lock settings, and the per-mode tailnet identities.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

//go:embed sandbox.schema.json
var schemaJSON []byte

//go:embed sandbox.example.toml
var exampleTOML []byte

// Schema returns the JSON Schema document for sandbox.toml.
func Schema() []byte { return slices.Clone(schemaJSON) }

// Example returns the annotated example configuration.
func Example() []byte { return slices.Clone(exampleTOML) }

// DefaultPath is where the container sees sandbox.toml.
const DefaultPath = "/etc/headmesh/config/sandbox.toml"

// Mode is the sandbox profile.
type Mode string

// The two profiles. Regular is always the default.
const (
	ModeRegular Mode = "regular"
	ModeYOLO    Mode = "yolo"
)

// ParseMode accepts "regular" or "yolo"; the empty string means Regular.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeRegular:
		return ModeRegular, nil
	case ModeYOLO:
		return ModeYOLO, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want regular or yolo)", s)
	}
}

// Config mirrors sandbox.toml.
type Config struct {
	SchemaVersion int               `koanf:"schema_version"`
	Name          string            `koanf:"name"`
	Root          string            `koanf:"root"`
	HostHome      string            `koanf:"host_home"`
	Image         string            `koanf:"image"`
	Timezone      string            `koanf:"timezone"`
	GPU           string            `koanf:"gpu"`
	RepoLock      string            `koanf:"repo_lock"`
	Agent         Agent             `koanf:"agent"`
	Git           Git               `koanf:"git"`
	Tailnet       Tailnet           `koanf:"tailnet"`
	Resources     Resources         `koanf:"resources"`
	Mounts        Mounts            `koanf:"mounts"`
	Mode          Modes             `koanf:"mode"`
	Repos         map[string]string `koanf:"repos"`
	Sandboxd      Sandboxd          `koanf:"sandboxd"`
	Entrypoints   Entrypoints       `koanf:"entrypoints"`
	Backup        Backup            `koanf:"backup"`
	Acceptance    Acceptance        `koanf:"acceptance"`
}

// Agent is the in-container user.
type Agent struct {
	UID int `koanf:"uid"`
	GID int `koanf:"gid"`
}

// Git is the commit identity for agent commits.
type Git struct {
	UserName  string `koanf:"user_name"`
	UserEmail string `koanf:"user_email"`
}

// Tailnet describes the Headscale cell the sandbox joins.
type Tailnet struct {
	LoginServer    string `koanf:"login_server"`
	MagicDNSSuffix string `koanf:"magicdns_suffix"`
	DNSCheckName   string `koanf:"dns_check_name"`
	DNSCheckIP     string `koanf:"dns_check_ip"`
}

// Resources caps the container.
type Resources struct {
	Memory string  `koanf:"memory"`
	CPUs   float64 `koanf:"cpus"`
}

// Mounts are shared by both modes.
type Mounts struct {
	RW            []string `koanf:"rw"`
	RO            []string `koanf:"ro"`
	Masks         []string `koanf:"masks"`
	AllowSecrets  []string `koanf:"allow_secrets"`
	ExtraDenylist []string `koanf:"extra_denylist"`
}

// Modes holds the per-profile settings.
type Modes struct {
	Regular RegularMode `koanf:"regular"`
	YOLO    YOLOMode    `koanf:"yolo"`
}

// RegularMode is the default, restricted profile.
type RegularMode struct {
	Hostname string   `koanf:"hostname"`
	Tags     []string `koanf:"tags"`
	Reach    []string `koanf:"reach"`
	RW       []string `koanf:"rw"`
	RO       []string `koanf:"ro"`
}

// YOLOMode is the opt-in, wide profile.
type YOLOMode struct {
	Hostname     string   `koanf:"hostname"`
	Tags         []string `koanf:"tags"`
	DefaultTTL   string   `koanf:"default_ttl"`
	MaxTTL       string   `koanf:"max_ttl"`
	RW           []string `koanf:"rw"`
	RO           []string `koanf:"ro"`
	AllowLAN     bool     `koanf:"allow_lan"`
	BackupMaxAge string   `koanf:"backup_max_age"`
	SSHKey       string   `koanf:"ssh_key"`
}

// Sandboxd configures the MCP daemon.
type Sandboxd struct {
	Listen                 string   `koanf:"listen"`
	PublicURL              string   `koanf:"public_url"`
	AllowedOrigins         []string `koanf:"allowed_origins"`
	MaxConcurrentPerClient int      `koanf:"max_concurrent_per_client"`
	RatePerMinute          int      `koanf:"rate_per_minute"`
	ExecDefaultTimeoutS    int      `koanf:"exec_default_timeout_s"`
	ExecMaxTimeoutS        int      `koanf:"exec_max_timeout_s"`
	TunnelExecMaxTimeoutS  int      `koanf:"tunnel_exec_max_timeout_s"`
	MaxResponseBytes       int      `koanf:"max_response_bytes"`
	TunnelMaxResponseBytes int      `koanf:"tunnel_max_response_bytes"`
	RedactOutput           bool     `koanf:"redact_output"`
	OAuth                  OAuth    `koanf:"oauth"`
}

// OAuth configures the M3 resource server.
type OAuth struct {
	Enabled              bool     `koanf:"enabled"`
	Issuer               string   `koanf:"issuer"`
	JWKSURL              string   `koanf:"jwks_url"`
	Resource             string   `koanf:"resource"`
	AuthorizationServers []string `koanf:"authorization_servers"`
}

// Entrypoints toggles the cloud front doors.
type Entrypoints struct {
	ClaudeRunner ClaudeRunner `koanf:"claude_runner"`
	ClaudeRC     ClaudeRC     `koanf:"claude_rc"`
	OpenAITunnel OpenAITunnel `koanf:"openai_tunnel"`
}

// ClaudeRunner configures `claude self-hosted-runner`.
type ClaudeRunner struct {
	Enabled             bool `koanf:"enabled"`
	Capacity            int  `koanf:"capacity"`
	KillSessionAfterMin int  `koanf:"kill_session_after_min"`
	ConfigureGit        bool `koanf:"configure_git"`
}

// ClaudeRC configures Claude Remote Control inside the sandbox.
type ClaudeRC struct {
	Enabled  bool   `koanf:"enabled"`
	Capacity int    `koanf:"capacity"`
	Spawn    string `koanf:"spawn"`
	CWD      string `koanf:"cwd"`
}

// OpenAITunnel configures tunnel-client.
type OpenAITunnel struct {
	Enabled  bool   `koanf:"enabled"`
	TunnelID string `koanf:"tunnel_id"`
}

// Backup locates the restic home backup.
type Backup struct {
	ResticRepo string `koanf:"restic_repo"`
	StatusFile string `koanf:"status_file"`
}

// Acceptance parameterizes the five-check harness.
type Acceptance struct {
	Project           string `koanf:"project"`
	File              string `koanf:"file"`
	DependencyCommand string `koanf:"dependency_command"`
	MagicDNSName      string `koanf:"magicdns_name"`
	AllowedTarget     string `koanf:"allowed_target"`
	DeniedTarget      string `koanf:"denied_target"`
}

// Load reads and validates the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator chooses the config path
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes TOML, validates it against the JSON Schema, applies
// defaults and runs the semantic checks.
func Parse(data []byte) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), toml.Parser()); err != nil {
		return nil, fmt.Errorf("parse toml: %w", err)
	}
	if err := validateSchema(k.Raw()); err != nil {
		return nil, err
	}
	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateSchema(raw map[string]any) error {
	var s jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return fmt.Errorf("embedded schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("embedded schema: %w", err)
	}
	// Round-trip through JSON so TOML integers and dates become plain JSON values.
	buf, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode config for validation: %w", err)
	}
	var instance map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := dec.Decode(&instance); err != nil {
		return fmt.Errorf("decode config for validation: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	setStr := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	setInt := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	setStr(&c.Timezone, "America/Toronto")
	setStr(&c.GPU, "off")
	setStr(&c.RepoLock, "advisory")
	setStr(&c.Resources.Memory, "10g")
	if c.Resources.CPUs == 0 {
		c.Resources.CPUs = 6
	}
	setStr(&c.Mode.YOLO.DefaultTTL, "4h")
	setStr(&c.Mode.YOLO.MaxTTL, "12h")
	setStr(&c.Mode.YOLO.BackupMaxAge, "24h")
	s := &c.Sandboxd
	setStr(&s.Listen, ":8780")
	if s.PublicURL == "" {
		port := "8780"
		if _, p, err := net.SplitHostPort(s.Listen); err == nil && p != "" {
			port = p
		}
		s.PublicURL = fmt.Sprintf("http://%s.%s:%s", c.Mode.Regular.Hostname, c.Tailnet.MagicDNSSuffix, port)
	}
	setInt(&s.MaxConcurrentPerClient, 8)
	setInt(&s.RatePerMinute, 600)
	setInt(&s.ExecDefaultTimeoutS, 120)
	setInt(&s.ExecMaxTimeoutS, 3600)
	setInt(&s.TunnelExecMaxTimeoutS, 110)
	setInt(&s.MaxResponseBytes, 10<<20)
	setInt(&s.TunnelMaxResponseBytes, 8<<20)
	setInt(&c.Entrypoints.ClaudeRunner.Capacity, 1)
	setInt(&c.Entrypoints.ClaudeRC.Capacity, 4)
	setStr(&c.Entrypoints.ClaudeRC.Spawn, "same-dir")
	setStr(&c.Entrypoints.ClaudeRC.CWD, "/home/agent")
	setStr(&c.Backup.ResticRepo, "/mnt/storage/backups/restic-home")
	setStr(&c.Backup.StatusFile, filepath.Join(c.Root, "state", "backup-status.json"))
	setStr(&c.Acceptance.File, "README.md")
	if c.Repos == nil {
		c.Repos = map[string]string{}
	}
}

// Validate runs the checks the JSON Schema can't express.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for _, p := range c.allPaths() {
		if filepath.Clean(p) != p {
			add("path %q is not clean (want %q)", p, filepath.Clean(p))
		}
	}
	if c.Root == "/" || c.Root == c.HostHome {
		add("root %q must be a dedicated directory", c.Root)
	}
	for _, rw := range c.Mounts.RW {
		if slices.Contains(c.Mounts.RO, rw) {
			add("mount %q is listed as both rw and ro", rw)
		}
	}
	for _, m := range slices.Concat(c.RWMounts(ModeRegular), c.ROMounts(ModeRegular), c.RWMounts(ModeYOLO), c.ROMounts(ModeYOLO)) {
		if isWithin(c.Root, m) || isWithin(m, c.Root) {
			add("mount %q overlaps the sandbox root %q; the root's subdirectories are mounted individually", m, c.Root)
		}
	}
	def, err := time.ParseDuration(c.Mode.YOLO.DefaultTTL)
	if err != nil {
		add("mode.yolo.default_ttl: %v", err)
	}
	maxTTL, err := time.ParseDuration(c.Mode.YOLO.MaxTTL)
	if err != nil {
		add("mode.yolo.max_ttl: %v", err)
	}
	if err == nil && def > maxTTL {
		add("mode.yolo.default_ttl %s exceeds max_ttl %s", def, maxTTL)
	}
	if _, err := time.ParseDuration(c.Mode.YOLO.BackupMaxAge); err != nil {
		add("mode.yolo.backup_max_age: %v", err)
	}
	if c.Mode.Regular.Hostname == c.Mode.YOLO.Hostname {
		add("regular and yolo need different hostnames (each mode is its own node)")
	}
	for _, t := range c.Mode.Regular.Tags {
		if slices.Contains(c.Mode.YOLO.Tags, t) {
			add("tag %q is used by both modes; YOLO must have its own tag", t)
		}
	}
	for _, r := range c.Mode.Regular.Reach {
		if _, port, err := net.SplitHostPort(r); err != nil {
			add("reach %q: %v", r, err)
		} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			add("reach %q: bad port", r)
		}
	}
	writable := c.WritableRoots(ModeRegular)
	for repo, dir := range c.Repos {
		if !anyWithin(writable, dir) {
			add("repos[%q] = %q is not inside a read-write mount", repo, dir)
		}
	}
	if c.Acceptance.Project != "" && !anyWithin(c.ReadableRoots(ModeRegular), c.Acceptance.Project) {
		add("acceptance.project %q is not mounted", c.Acceptance.Project)
	}
	s := c.Sandboxd
	if s.ExecDefaultTimeoutS > s.ExecMaxTimeoutS {
		add("sandboxd.exec_default_timeout_s exceeds exec_max_timeout_s")
	}
	if s.TunnelExecMaxTimeoutS > s.ExecMaxTimeoutS {
		add("sandboxd.tunnel_exec_max_timeout_s exceeds exec_max_timeout_s")
	}
	if s.OAuth.Enabled && (s.OAuth.Issuer == "" || s.OAuth.JWKSURL == "" || s.OAuth.Resource == "") {
		add("sandboxd.oauth.enabled needs issuer, jwks_url and resource")
	}
	if c.Entrypoints.OpenAITunnel.Enabled && c.Entrypoints.OpenAITunnel.TunnelID == "" {
		add("entrypoints.openai_tunnel.enabled needs tunnel_id")
	}
	return errors.Join(errs...)
}

func (c *Config) allPaths() []string {
	out := []string{c.Root, c.HostHome}
	out = append(out, c.Mounts.RW...)
	out = append(out, c.Mounts.RO...)
	out = append(out, c.Mounts.Masks...)
	out = append(out, c.Mode.Regular.RW...)
	out = append(out, c.Mode.Regular.RO...)
	out = append(out, c.Mode.YOLO.RW...)
	out = append(out, c.Mode.YOLO.RO...)
	for _, v := range c.Repos {
		out = append(out, v)
	}
	return out
}

// RWMounts returns the read-write host paths for a mode, in config order.
func (c *Config) RWMounts(m Mode) []string {
	out := slices.Clone(c.Mounts.RW)
	if m == ModeYOLO {
		out = append(out, c.Mode.YOLO.RW...)
	} else {
		out = append(out, c.Mode.Regular.RW...)
	}
	return dedupe(out)
}

// ROMounts returns the read-only host paths for a mode (excluding YOLO's /host).
func (c *Config) ROMounts(m Mode) []string {
	out := slices.Clone(c.Mounts.RO)
	if m == ModeYOLO {
		out = append(out, c.Mode.YOLO.RO...)
	} else {
		out = append(out, c.Mode.Regular.RO...)
	}
	return dedupe(out)
}

// WritableRoots are the paths agents may write to in a mode.
func (c *Config) WritableRoots(m Mode) []string { return c.RWMounts(m) }

// ReadableRoots are all mounted host paths in a mode.
func (c *Config) ReadableRoots(m Mode) []string {
	out := append(c.RWMounts(m), c.ROMounts(m)...)
	if m == ModeYOLO {
		out = append(out, "/")
	}
	return dedupe(out)
}

// Hostname returns the tailnet hostname for a mode.
func (c *Config) Hostname(m Mode) string {
	if m == ModeYOLO {
		return c.Mode.YOLO.Hostname
	}
	return c.Mode.Regular.Hostname
}

// Tags returns the advertised tags for a mode.
func (c *Config) Tags(m Mode) []string {
	if m == ModeYOLO {
		return c.Mode.YOLO.Tags
	}
	return c.Mode.Regular.Tags
}

// ContainerName is the fixed container name for a mode.
func (c *Config) ContainerName(m Mode) string {
	if m == ModeYOLO {
		return "headmesh-sandbox-yolo"
	}
	return "headmesh-sandbox"
}

// EnabledEntrypoints lists the s6 services that should run, in a stable order.
func (c *Config) EnabledEntrypoints() []string {
	var out []string
	if c.Entrypoints.ClaudeRunner.Enabled {
		out = append(out, "claude-runner")
	}
	if c.Entrypoints.ClaudeRC.Enabled {
		out = append(out, "claude-rc")
	}
	if c.Entrypoints.OpenAITunnel.Enabled {
		out = append(out, "openai-tunnel")
	}
	return out
}

// RepoPath resolves a repo given as an absolute path, "owner/repo" or a bare
// repo name that matches exactly one mapping.
func (c *Config) RepoPath(repo string) (string, error) {
	if repo == "" {
		return "", errors.New("repo is required")
	}
	if filepath.IsAbs(repo) {
		return filepath.Clean(repo), nil
	}
	if p, ok := c.Repos[repo]; ok {
		return p, nil
	}
	var matches []string
	for k, v := range c.Repos {
		if strings.EqualFold(filepath.Base(k), repo) || filepath.Base(v) == repo {
			matches = append(matches, v)
		}
	}
	matches = dedupe(matches)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("repo %q is not in [repos]; pass an absolute path", repo)
	default:
		slices.Sort(matches)
		return "", fmt.Errorf("repo %q is ambiguous: %s", repo, strings.Join(matches, ", "))
	}
}

// ParseTTL validates a requested YOLO TTL against max_ttl.
func (c *Config) ParseTTL(s string) (time.Duration, error) {
	if s == "" {
		s = c.Mode.YOLO.DefaultTTL
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl: %w", err)
	}
	maxTTL, _ := time.ParseDuration(c.Mode.YOLO.MaxTTL)
	if d <= 0 || d > maxTTL {
		return 0, fmt.Errorf("ttl %s must be between 1s and max_ttl %s", d, maxTTL)
	}
	return d, nil
}

// isWithin reports whether p equals root or lies below it.
func isWithin(root, p string) bool {
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

// IsWithin is the exported form of the path containment check.
func IsWithin(root, p string) bool { return isWithin(root, p) }

func anyWithin(roots []string, p string) bool {
	for _, r := range roots {
		if isWithin(r, p) {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
