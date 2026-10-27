package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestExampleParses(t *testing.T) {
	cfg, err := Parse(Example())
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Name != "asus-sandbox" || cfg.Agent.UID != 1000 {
		t.Fatalf("unexpected values: %+v", cfg)
	}
	if got := cfg.Hostname(ModeYOLO); got != "asus-sandbox-yolo" {
		t.Errorf("yolo hostname = %q", got)
	}
	if got := cfg.ContainerName(ModeRegular); got != "headmesh-sandbox" {
		t.Errorf("container name = %q", got)
	}
	if cfg.Sandboxd.MaxConcurrentPerClient != 8 || cfg.Sandboxd.RatePerMinute != 600 {
		t.Errorf("limits = %d/%d", cfg.Sandboxd.MaxConcurrentPerClient, cfg.Sandboxd.RatePerMinute)
	}
	if cfg.Sandboxd.TunnelExecMaxTimeoutS != 110 {
		t.Errorf("tunnel timeout = %d", cfg.Sandboxd.TunnelExecMaxTimeoutS)
	}
	if len(cfg.EnabledEntrypoints()) != 0 {
		t.Errorf("entry points should be off by default: %v", cfg.EnabledEntrypoints())
	}
}

func mutate(t *testing.T, from, to string) []byte {
	t.Helper()
	src := string(Example())
	if !strings.Contains(src, from) {
		t.Fatalf("example has no %q", from)
	}
	return []byte(strings.Replace(src, from, to, 1))
}

func TestSchemaRejects(t *testing.T) {
	cases := map[string][]byte{
		"unknown top-level key": append(Example(), []byte("\nsurprise = true\n")...),
		"bad gpu":               mutate(t, `gpu = "off"`, `gpu = "all"`),
		"bad repo_lock":         mutate(t, `repo_lock = "advisory"`, `repo_lock = "hard"`),
		"relative mount":        mutate(t, `"/mnt/storage/projects/headmesh",`, `"projects/headmesh",`),
		"tag without prefix":    mutate(t, `tags = ["tag:sandbox"]`, `tags = ["sandbox"]`),
		"bad duration":          mutate(t, `default_ttl = "4h"`, `default_ttl = "four hours"`),
		"http login server":     mutate(t, `login_server = "https://hs.xcelsior.ca"`, `login_server = "http://hs.xcelsior.ca"`),
		"tunnel timeout > 115":  mutate(t, `tunnel_exec_max_timeout_s = 110`, `tunnel_exec_max_timeout_s = 300`),
		"bad repo key":          mutate(t, `"aabiro/xcelsior" =`, `"xcelsior" =`),
		"schema version":        mutate(t, `schema_version = 1`, `schema_version = 2`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(data); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestSemanticRejects(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want string
	}{
		"ttl above max": {
			mutate(t, `default_ttl = "4h"`, `default_ttl = "13h"`), "exceeds max_ttl",
		},
		"same hostname": {
			mutate(t, `hostname = "asus-sandbox-yolo"`, `hostname = "asus-sandbox"`), "different hostnames",
		},
		"shared tag": {
			mutate(t, `tags = ["tag:sandbox-yolo"]`, `tags = ["tag:sandbox"]`), "used by both modes",
		},
		"repo outside rw": {
			mutate(t, `"headmesh-dev/headmesh" = "/mnt/storage/projects/headmesh"`, `"headmesh-dev/headmesh" = "/srv/elsewhere"`),
			"not inside a read-write mount",
		},
		"mount overlaps root": {
			mutate(t, `ro = []`+"\n# Files", `ro = ["/mnt/storage/headmesh"]`+"\n# Files"), "overlaps the sandbox root",
		},
		"unclean path": {
			mutate(t, `"/mnt/storage/projects/headmesh",`, `"/mnt/storage/projects/headmesh/",`), "not clean",
		},
		"tunnel without id": {
			mutate(t, "enabled = false\ntunnel_id = \"\"", "enabled = true\ntunnel_id = \"\""), "needs tunnel_id",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRepoPath(t *testing.T) {
	cfg, err := Parse(Example())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"aabiro/xcelsior":        "/mnt/storage/projects/xcelsior",
		"xcelsior":               "/mnt/storage/projects/xcelsior",
		"headmesh":               "/mnt/storage/projects/headmesh",
		"/tmp/x/../y":            "/tmp/y",
		"headmesh-dev/headmesh":  "/mnt/storage/projects/headmesh",
		"/mnt/storage/projects/": "/mnt/storage/projects",
	}
	for in, want := range cases {
		got, err := cfg.RepoPath(in)
		if err != nil || got != want {
			t.Errorf("RepoPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := cfg.RepoPath("nope"); err == nil {
		t.Error("unknown repo should fail")
	}
	if _, err := cfg.RepoPath(""); err == nil {
		t.Error("empty repo should fail")
	}
}

func TestParseTTL(t *testing.T) {
	cfg, err := Parse(Example())
	if err != nil {
		t.Fatal(err)
	}
	if d, err := cfg.ParseTTL(""); err != nil || d != 4*time.Hour {
		t.Errorf("default ttl = %v, %v", d, err)
	}
	if _, err := cfg.ParseTTL("13h"); err == nil {
		t.Error("13h should exceed max_ttl")
	}
	if _, err := cfg.ParseTTL("-1h"); err == nil {
		t.Error("negative ttl should fail")
	}
}

func TestMountsPerMode(t *testing.T) {
	cfg, err := Parse(mutate(t, `rw = ["/home/aaryn"]`, `rw = ["/home/aaryn", "/mnt/storage/projects/xcelsior"]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/mnt/storage/projects/xcelsior", "/mnt/storage/projects/headmesh", "/home/aaryn"}
	if diff := cmp.Diff(want, cfg.RWMounts(ModeYOLO)); diff != "" {
		t.Errorf("yolo rw mounts (-want +got):\n%s", diff)
	}
	if got := cfg.ReadableRoots(ModeYOLO); got[len(got)-1] != "/" {
		t.Errorf("yolo readable roots should end with /: %v", got)
	}
}

func TestDenylistInside(t *testing.T) {
	d := NewDenylist(DefaultDenylist, "/home/u", nil)
	cases := map[string]string{
		"/home/u/.ssh":                      "~/.ssh",
		"/home/u/.ssh/id_ed25519":           "~/.ssh",
		"/home/u/.claude/.credentials.json": "~/.claude/.credentials.json",
		"/var/lib/tailscale-xcelsior/x":     "/var/lib/tailscale*",
		"/etc/ssh/ssh_host_ed25519_key":     "/etc/ssh/ssh_host_*",
		"/var/run/docker.sock":              "/var/run/docker.sock",
		"/root/.bashrc":                     "/root",
	}
	for p, want := range cases {
		got, ok := d.Inside(p)
		if !ok || got != want {
			t.Errorf("Inside(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"/home/u", "/home/u/.sshx", "/home/u/.claude/settings.json", "/etc/ssh/sshd_config", "/mnt/storage/projects"} {
		if pat, ok := d.Inside(p); ok {
			t.Errorf("Inside(%q) unexpectedly matched %q", p, pat)
		}
	}
}

func TestDenylistAllowAndUnder(t *testing.T) {
	home := t.TempDir()
	for _, p := range []string{".ssh/id_ed25519", ".config/gh/hosts.yml", ".netrc", ".claude/.credentials.json", "projects/app/main.go"} {
		full := filepath.Join(home, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := NewDenylist(DefaultDenylist, home, []string{"~/.config/gh"})
	got := d.Under(home)
	var paths []string
	for _, m := range got {
		paths = append(paths, strings.TrimPrefix(m.Path, home))
	}
	want := []string{"/.claude/.credentials.json", "/.netrc", "/.ssh"}
	if diff := cmp.Diff(want, paths); diff != "" {
		t.Errorf("Under (-want +got):\n%s", diff)
	}
	if !got[2].IsDir || got[1].IsDir {
		t.Errorf("IsDir wrong: %+v", got)
	}
	if err := d.CheckMount(filepath.Join(home, ".ssh")); err == nil {
		t.Error("mounting ~/.ssh must be refused")
	}
	if err := d.CheckMount(filepath.Join(home, ".config/gh")); err != nil {
		t.Errorf("allowed secret refused: %v", err)
	}
	if err := d.CheckMount(filepath.Join(home, "projects")); err != nil {
		t.Errorf("projects refused: %v", err)
	}
	link := filepath.Join(home, "projects", "keys")
	if err := os.Symlink(filepath.Join(home, ".ssh"), link); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckMount(link); err == nil {
		t.Error("symlink into ~/.ssh must be refused")
	}
}

func TestConfigDenylistIncludesSecretsDir(t *testing.T) {
	cfg, err := Parse(Example())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Denylist().Inside("/mnt/storage/headmesh/sandbox/secrets/ts_authkey"); !ok {
		t.Error("the sandbox secrets dir must be denylisted")
	}
}
