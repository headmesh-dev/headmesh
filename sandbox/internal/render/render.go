// Package render turns sandbox.toml into compose.override.generated.yaml:
// per-mode environment, project mounts, the sandbox's own state mounts,
// GPU, and secret masks. It refuses mounts inside denylisted secret paths
// or the Docker data-root, and builds YOLO's read-only host view from
// per-subtree binds (spike T-7: a single "/" bind is refused with rprivate,
// leaks with rshared and exposes later host mounts with rslave).
package render

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/yamlw"
)

// Container paths (sandbox/docs/container-contract.md).
const (
	ConfigDir    = "/etc/headmesh/config"
	HostView     = "/host"
	SSHKeyTarget = "/etc/headmesh/ssh/id_ed25519"
)

// Host abstracts the filesystem render inspects, for tests.
type Host struct {
	ReadDir   func(string) ([]fs.DirEntry, error)
	Lstat     func(string) (fs.FileInfo, error)
	DataRoots []string // Docker and containerd roots; never visible, never mounted
	Root      string   // host root for the YOLO view ("/" unless testing)
}

// OS is the real host.
func OS(dataRoots []string) Host {
	return Host{ReadDir: os.ReadDir, Lstat: os.Lstat, DataRoots: dataRoots, Root: "/"}
}

// Mount is one generated volume entry.
type Mount struct {
	Type        string // bind or tmpfs
	Source      string
	Target      string
	ReadOnly    bool
	Recursive   string // bind.recursive
	Propagation string // bind.propagation
	Purpose     string // shown in previews
	Infra       bool   // the sandbox's own state; never masked
}

// Service is the rendered view of one mode.
type Service struct {
	Mode     config.Mode
	Name     string
	Hostname string
	Memory   string
	CPUs     float64
	Env      yamlw.Map
	Mounts   []Mount
	GPU      bool
}

// Result is the full rendering.
type Result struct {
	Services []Service
	Warnings []string
	Override []byte
}

// skipTop are top-level host entries never shown under /host.
var skipTop = map[string]bool{"proc": true, "sys": true, "dev": true, "run": true, "tmp": true, "lost+found": true}

// Render validates cfg against the host and produces the override.
func Render(cfg *config.Config, host Host) (Result, error) {
	r := &renderer{cfg: cfg, host: host, deny: denylist(cfg)}
	var res Result
	var errs []error
	for _, m := range []config.Mode{config.ModeRegular, config.ModeYOLO} {
		svc, err := r.service(m)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m, err))
			continue
		}
		res.Services = append(res.Services, svc)
	}
	res.Warnings = r.warnings
	if err := errors.Join(errs...); err != nil {
		return res, err
	}
	res.Override = override(res.Services)
	return res, nil
}

// denylist adds the sandbox's own tailnet state and the backup repository
// to the configured denylist.
func denylist(cfg *config.Config) *config.Denylist {
	extra := []string{filepath.Join(cfg.Root, "state"), "/var/lib/docker", "/var/lib/containerd"}
	if cfg.Backup.ResticRepo != "" {
		extra = append(extra, cfg.Backup.ResticRepo)
	}
	c := *cfg
	c.Mounts.ExtraDenylist = append(slices.Clone(cfg.Mounts.ExtraDenylist), extra...)
	return c.Denylist()
}

type renderer struct {
	cfg      *config.Config
	host     Host
	deny     *config.Denylist
	warnings []string
}

func (r *renderer) warn(format string, a ...any) {
	w := fmt.Sprintf(format, a...)
	if !slices.Contains(r.warnings, w) {
		r.warnings = append(r.warnings, w)
	}
}

func serviceName(m config.Mode) string {
	if m == config.ModeYOLO {
		return "sandbox-yolo"
	}
	return "sandbox"
}

func (r *renderer) service(m config.Mode) (Service, error) {
	cfg := r.cfg
	svc := Service{Mode: m, Name: serviceName(m), Hostname: cfg.Hostname(m), Memory: cfg.Resources.Memory,
		CPUs: cfg.Resources.CPUs, GPU: cfg.GPU == "shared"}
	svc.Env = yamlw.Map{
		{K: "HEADMESH_MODE", V: string(m)},
		{K: "HEADMESH_ENTRYPOINTS", V: strings.Join(cfg.EnabledEntrypoints(), ",")},
		{K: "HEADMESH_GPU", V: cfg.GPU},
		{K: "HEADMESH_REPO_LOCK", V: cfg.RepoLock},
		{K: "HEADMESH_SANDBOX_CONFIG", V: ConfigDir + "/sandbox.toml"},
		{K: "HEADMESH_RUNNER_CAPACITY", V: strconv.Itoa(cfg.Entrypoints.ClaudeRunner.Capacity)},
		{K: "HEADMESH_RUNNER_KILL_AFTER_MIN", V: strconv.Itoa(cfg.Entrypoints.ClaudeRunner.KillSessionAfterMin)},
		{K: "HEADMESH_RC_CAPACITY", V: strconv.Itoa(cfg.Entrypoints.ClaudeRC.Capacity)},
		{K: "HEADMESH_RC_SPAWN", V: cfg.Entrypoints.ClaudeRC.Spawn},
		{K: "HEADMESH_RC_CWD", V: cfg.Entrypoints.ClaudeRC.CWD},
		{K: "HEADMESH_TUNNEL_ID", V: cfg.Entrypoints.OpenAITunnel.TunnelID},
		{K: "HEADMESH_GIT_USER_NAME", V: cfg.Git.UserName},
		{K: "HEADMESH_GIT_USER_EMAIL", V: cfg.Git.UserEmail},
		{K: "TS_LOGIN_SERVER", V: cfg.Tailnet.LoginServer},
		{K: "TS_HOSTNAME", V: cfg.Hostname(m)},
		{K: "TS_TAGS", V: strings.Join(cfg.Tags(m), ",")},
		{K: "TZ", V: cfg.Timezone},
	}
	if m == config.ModeYOLO {
		svc.Env = append(svc.Env, yamlw.KV{K: "HEADMESH_YOLO_EXPIRES_AT", V: "${HEADMESH_YOLO_EXPIRES_AT:-}"})
	}

	root := cfg.Root
	state := "tailscale-" + string(m)
	infra := []Mount{
		{Type: "bind", Source: filepath.Join(root, "config"), Target: ConfigDir, ReadOnly: true, Purpose: "sandbox.toml and token hashes"},
		{Type: "bind", Source: filepath.Join(root, "home"), Target: "/home/agent", Purpose: "agent home"},
		{Type: "bind", Source: filepath.Join(root, "state", state), Target: "/var/lib/tailscale", Purpose: "tailnet identity (" + string(m) + ")"},
		{Type: "bind", Source: filepath.Join(root, "sessions"), Target: "/var/lib/headmesh/sessions", Purpose: "sessions, processes, exec output"},
		{Type: "bind", Source: filepath.Join(root, "artifacts"), Target: "/var/lib/headmesh/artifacts", Purpose: "artifacts"},
		{Type: "bind", Source: filepath.Join(root, "logs"), Target: "/var/log/headmesh", Purpose: "service logs, audit.jsonl"},
		{Type: "bind", Source: filepath.Join(root, "run"), Target: "/run/headmesh", Purpose: "sandboxd.sock for the host CLI"},
		{Type: "bind", Source: filepath.Join(root, "runner"), Target: "/srv/runner", Purpose: "Claude runner base dir"},
		{Type: "bind", Source: filepath.Join(root, "runner-work"), Target: "/srv/runner-work", Purpose: "clones of unmapped repos"},
	}
	for i := range infra {
		infra[i].Infra = true
	}
	svc.Mounts = append(svc.Mounts, infra...)

	var errs []error
	for _, p := range cfg.RWMounts(m) {
		if err := r.checkSource(p); err != nil {
			errs = append(errs, err)
			continue
		}
		svc.Mounts = append(svc.Mounts, Mount{Type: "bind", Source: p, Target: p, Purpose: "read-write"})
	}
	for _, p := range cfg.ROMounts(m) {
		if err := r.checkSource(p); err != nil {
			errs = append(errs, err)
			continue
		}
		svc.Mounts = append(svc.Mounts, Mount{Type: "bind", Source: p, Target: p, ReadOnly: true, Recursive: "readonly", Purpose: "read-only"})
	}
	if m == config.ModeYOLO {
		hostRoot := r.host.Root
		if hostRoot == "" {
			hostRoot = "/"
		}
		hv, err := r.hostView(hostRoot, hostRoot)
		if err != nil {
			errs = append(errs, err)
		}
		svc.Mounts = append(svc.Mounts, hv...)
		if key := cfg.Mode.YOLO.SSHKey; key != "" {
			if _, err := r.host.Lstat(key); err != nil {
				errs = append(errs, fmt.Errorf("mode.yolo.ssh_key %s: %w", key, err))
			} else {
				svc.Mounts = append(svc.Mounts, Mount{Type: "bind", Source: key, Target: SSHKeyTarget, ReadOnly: true, Purpose: "YOLO deploy key"})
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return svc, err
	}
	svc.Mounts = append(svc.Mounts, r.masks(svc.Mounts)...)
	return svc, nil
}

// checkSource refuses missing paths, denylisted secrets and anything that
// overlaps a Docker data-root.
func (r *renderer) checkSource(p string) error {
	if _, err := r.host.Lstat(p); err != nil {
		return fmt.Errorf("mount %s: %w", p, err)
	}
	if err := r.deny.CheckMount(p); err != nil {
		return err //nolint:wrapcheck // MountViolation is self-describing
	}
	for _, d := range r.host.DataRoots {
		if config.IsWithin(d, p) || config.IsWithin(p, d) {
			return fmt.Errorf("mount %s overlaps the Docker data-root %s (other containers' layers and volumes)", p, d)
		}
	}
	return nil
}

// hostView binds each subtree of dir under /host read-only (recursive,
// rprivate), descending around data-roots and skipping denied subtrees.
func (r *renderer) hostView(root, dir string) ([]Mount, error) {
	entries, err := r.host.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s for the YOLO host view: %w", dir, err)
	}
	var out []Mount
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if dir == root && skipTop[e.Name()] {
			continue
		}
		// Symlinks (/bin → usr/bin) and files (swap.img) are left out; their
		// targets are visible through the real directories.
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if slices.Contains(r.host.DataRoots, p) {
			r.warn("YOLO host view leaves out the Docker data-root %s", p)
			continue
		}
		if pat, ok := r.deny.Inside(p); ok {
			r.warn("YOLO host view leaves out %s (denylisted: %s)", p, pat)
			continue
		}
		contains := false
		for _, d := range r.host.DataRoots {
			if config.IsWithin(p, d) {
				contains = true
			}
		}
		if contains {
			sub, err := r.hostView(root, p)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}
		rel := strings.TrimPrefix(p, strings.TrimSuffix(root, "/"))
		out = append(out, Mount{Type: "bind", Source: p, Target: HostView + rel, ReadOnly: true, Recursive: "readonly",
			Propagation: "rprivate", Purpose: "YOLO host view"})
	}
	return out, nil
}

// masks hides denylisted paths and configured masks wherever a bind exposes them.
func (r *renderer) masks(mounts []Mount) []Mount {
	type cand struct {
		path  string
		isDir bool
		why   string
	}
	var cands []cand
	for _, b := range mounts {
		if b.Type != "bind" || b.Infra || b.Source == "/dev/null" {
			continue
		}
		for _, m := range r.deny.Under(b.Source) {
			cands = append(cands, cand{m.Path, m.IsDir, "denylist " + m.Pattern})
		}
	}
	for _, p := range r.cfg.Mounts.Masks {
		fi, err := r.host.Lstat(p)
		if err != nil {
			r.warn("mask %s does not exist on the host; skipped", p)
			continue
		}
		cands = append(cands, cand{p, fi.IsDir(), "[mounts] masks"})
	}
	var out []Mount
	seen := map[string]bool{}
	for _, c := range cands {
		for _, b := range mounts {
			if b.Type != "bind" || b.Infra || !config.IsWithin(b.Source, c.path) || c.path == b.Source {
				continue
			}
			target := b.Target + strings.TrimPrefix(c.path, b.Source)
			if seen[target] {
				continue
			}
			seen[target] = true
			if c.isDir {
				out = append(out, Mount{Type: "tmpfs", Target: target, ReadOnly: true, Purpose: "mask: " + c.why})
			} else {
				out = append(out, Mount{Type: "bind", Source: "/dev/null", Target: target, ReadOnly: true, Purpose: "mask: " + c.why})
			}
		}
	}
	slices.SortFunc(out, func(a, b Mount) int { return strings.Compare(a.Target, b.Target) })
	return out
}

// esc protects literal "$" from Compose interpolation.
func esc(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func override(svcs []Service) []byte {
	services := yamlw.Map{}
	for _, s := range svcs {
		vols := yamlw.List{}
		for _, m := range s.Mounts {
			v := yamlw.Map{{K: "type", V: m.Type}}
			if m.Type == "bind" {
				v = append(v, yamlw.KV{K: "source", V: esc(m.Source)})
			}
			v = append(v, yamlw.KV{K: "target", V: esc(m.Target)})
			if m.ReadOnly {
				v = append(v, yamlw.KV{K: "read_only", V: true})
			}
			switch m.Type {
			case "bind":
				b := yamlw.Map{{K: "create_host_path", V: false}}
				if m.Propagation != "" {
					b = append(b, yamlw.KV{K: "propagation", V: m.Propagation})
				}
				if m.Recursive != "" {
					b = append(b, yamlw.KV{K: "recursive", V: m.Recursive})
				}
				v = append(v, yamlw.KV{K: "bind", V: b})
			case "tmpfs":
				v = append(v, yamlw.KV{K: "tmpfs", V: yamlw.Map{{K: "size", V: 4096}, {K: "mode", V: 0o500}}})
			}
			vols = append(vols, v)
		}
		body := yamlw.Map{
			{K: "hostname", V: s.Hostname},
			{K: "mem_limit", V: s.Memory},
			{K: "cpus", V: s.CPUs},
			{K: "environment", V: s.Env},
		}
		if s.GPU {
			body = append(body, yamlw.KV{K: "devices", V: []string{"nvidia.com/gpu=all"}})
		}
		body = append(body, yamlw.KV{K: "volumes", V: vols})
		services = append(services, yamlw.KV{K: s.Name, V: body})
	}
	header := "# Generated by `headmesh-sandbox render` from sandbox.toml. Do not edit:\n" +
		"# change sandbox.toml and render again.\n"
	return append([]byte(header), yamlw.Marshal(yamlw.Map{{K: "services", V: services}})...)
}

// ImageEnv is the interpolation variable the base file needs.
func ImageEnv(cfg *config.Config) string { return "HEADMESH_SANDBOX_IMAGE=" + cfg.Image }
