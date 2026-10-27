package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

// fakeHost builds a small host tree under a temp dir that stands in for "/".
func fakeHost(t *testing.T) (string, *config.Config) {
	t.Helper()
	T := t.TempDir()
	mk := func(p string, file bool) {
		t.Helper()
		full := filepath.Join(T, p)
		if file {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"etc", "usr/bin", "proc", "tmp", "home/u/.ssh", "home/u/.config/gh", "mnt/storage/docker/overlay2",
		"mnt/storage/projects/app/.git", "mnt/storage/projects/data", "mnt/storage/backups/restic-home", "srv/hm/home", "var/lib"} {
		mk(d, false)
	}
	for _, f := range []string{"home/u/.netrc", "home/u/.ssh/id_ed25519", "mnt/storage/projects/app/.env", "swap.img"} {
		mk(f, true)
	}
	if err := os.Symlink("usr/bin", filepath.Join(T, "bin")); err != nil {
		t.Fatal(err)
	}
	src := string(config.Example())
	repl := []string{
		`root = "/mnt/storage/headmesh/sandbox"`, `root = "` + T + `/srv/hm"`,
		`host_home = "/home/aaryn"`, `host_home = "` + T + `/home/u"`,
		"rw = [\n  \"/mnt/storage/projects/xcelsior\",\n  \"/mnt/storage/projects/headmesh\",\n]", `rw = ["` + T + `/mnt/storage/projects/app"]`,
		"ro = []\n# Files", `ro = ["` + T + "/mnt/storage/projects/data\"]\n# Files",
		`"/mnt/storage/projects/xcelsior/.env",`, `"` + T + `/mnt/storage/projects/app/.env",`,
		`rw = ["/home/aaryn"]`, `rw = ["` + T + `/home/u"]`,
		"\"aabiro/xcelsior\" = \"/mnt/storage/projects/xcelsior\"\n\"headmesh-dev/headmesh\" = \"/mnt/storage/projects/headmesh\"", `"acme/app" = "` + T + `/mnt/storage/projects/app"`,
		`project = "/mnt/storage/projects/xcelsior"`, `project = "` + T + `/mnt/storage/projects/app"`,
		`restic_repo = "/mnt/storage/backups/restic-home"`, `restic_repo = "` + T + `/mnt/storage/backups/restic-home"`,
		`gpu = "off"`, `gpu = "shared"`,
	}
	for i := 0; i < len(repl); i += 2 {
		if !strings.Contains(src, repl[i]) {
			t.Fatalf("example lacks %q", repl[i])
		}
		src = strings.Replace(src, repl[i], repl[i+1], 1)
	}
	cfg, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return T, cfg
}

func hostFor(T string) Host {
	h := OS([]string{filepath.Join(T, "mnt/storage/docker")})
	h.Root = T
	return h
}

func targets(svc Service) map[string]Mount {
	out := map[string]Mount{}
	for _, m := range svc.Mounts {
		out[m.Target] = m
	}
	return out
}

func TestRenderRegularAndYOLO(t *testing.T) {
	T, cfg := fakeHost(t)
	res, err := Render(cfg, hostFor(T))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Services) != 2 {
		t.Fatalf("services %v", res.Services)
	}
	reg, yolo := targets(res.Services[0]), targets(res.Services[1])
	app := T + "/mnt/storage/projects/app"

	// Regular: project rw, data ro, the .env mask, infra mounts, no host view.
	if m := reg[app]; m.ReadOnly || m.Source != app {
		t.Fatalf("regular app mount %+v", m)
	}
	if m := reg[T+"/mnt/storage/projects/data"]; !m.ReadOnly || m.Recursive != "readonly" {
		t.Fatalf("regular data mount %+v", m)
	}
	if m := reg[app+"/.env"]; m.Source != "/dev/null" || !m.ReadOnly {
		t.Fatalf(".env mask %+v", m)
	}
	if m := reg["/var/lib/tailscale"]; !strings.HasSuffix(m.Source, "state/tailscale-regular") {
		t.Fatalf("regular state %+v", m)
	}
	for tgt := range reg {
		if strings.HasPrefix(tgt, HostView) {
			t.Fatalf("regular has host view %s", tgt)
		}
	}

	// YOLO: per-subtree host view, data-root left out, masks under /host and home.
	if m := yolo["/var/lib/tailscale"]; !strings.HasSuffix(m.Source, "state/tailscale-yolo") {
		t.Fatalf("yolo state %+v", m)
	}
	for _, want := range []string{"/host/etc", "/host/usr", "/host/home", "/host/mnt/storage/projects", "/host/var"} {
		m, ok := yolo[want]
		if !ok || !m.ReadOnly || m.Recursive != "readonly" || m.Propagation != "rprivate" {
			t.Errorf("host view %s = %+v (present %v)", want, m, ok)
		}
	}
	for _, absent := range []string{"/host", "/host/mnt", "/host/mnt/storage", "/host/mnt/storage/docker", "/host/proc", "/host/tmp", "/host/bin", "/host/swap.img", "/host/srv/hm"} {
		if _, ok := yolo[absent]; ok && !strings.HasPrefix(yolo[absent].Purpose, "mask") {
			t.Errorf("host view should not bind %s", absent)
		}
	}
	for _, mask := range []string{"/host/mnt/storage/backups/restic-home", T + "/home/u/.ssh", "/host/home/u/.ssh", "/host/home/u/.netrc", "/host/mnt/storage/projects/app/.env", app + "/.env"} {
		if m, ok := yolo[mask]; !ok || !strings.HasPrefix(m.Purpose, "mask") {
			t.Errorf("missing mask %s (%+v)", mask, m)
		}
	}
	if m := yolo[T+"/home/u/.ssh"]; m.Type != "tmpfs" {
		t.Errorf("dir mask should be tmpfs: %+v", m)
	}
	if m := yolo["/host/home/u/.netrc"]; m.Source != "/dev/null" {
		t.Errorf("file mask should bind /dev/null: %+v", m)
	}
	// ~/.config/gh is denylisted; it is masked too.
	if _, ok := yolo["/host/home/u/.config/gh"]; !ok {
		t.Error("~/.config/gh not masked")
	}

	out := string(res.Override)
	for _, want := range []string{`HEADMESH_MODE: "yolo"`, `HEADMESH_YOLO_EXPIRES_AT: "${HEADMESH_YOLO_EXPIRES_AT:-}"`, `- "nvidia.com/gpu=all"`,
		`recursive: "readonly"`, `propagation: "rprivate"`, `hostname: "asus-sandbox-yolo"`, `mem_limit: "10g"`, "create_host_path: false"} {
		if !strings.Contains(out, want) {
			t.Errorf("override lacks %q", want)
		}
	}
	if strings.Contains(out, "rshared") {
		t.Error("override must never use rshared")
	}
}

func TestRenderRefusals(t *testing.T) {
	T, cfg := fakeHost(t)
	cfg.Mounts.RW = append(cfg.Mounts.RW, T+"/home/u/.ssh")
	if _, err := Render(cfg, hostFor(T)); err == nil || !strings.Contains(err.Error(), "denylisted") {
		t.Fatalf("denylisted mount: %v", err)
	}
	T, cfg = fakeHost(t)
	cfg.Mounts.RO = append(cfg.Mounts.RO, T+"/mnt/storage")
	if _, err := Render(cfg, hostFor(T)); err == nil || !strings.Contains(err.Error(), "data-root") {
		t.Fatalf("data-root overlap: %v", err)
	}
	T, cfg = fakeHost(t)
	cfg.Mounts.RW = append(cfg.Mounts.RW, T+"/nope")
	if _, err := Render(cfg, hostFor(T)); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing mount: %v", err)
	}
	T, cfg = fakeHost(t)
	cfg.Mounts.AllowSecrets = []string{"~/.config/gh"}
	cfg.Mounts.RO = append(cfg.Mounts.RO, T+"/home/u/.config/gh")
	if _, err := Render(cfg, hostFor(T)); err != nil {
		t.Fatalf("allow_secrets not honoured: %v", err)
	}
}

func TestMissingMaskIsAWarning(t *testing.T) {
	T, cfg := fakeHost(t)
	cfg.Mounts.Masks = append(cfg.Mounts.Masks, T+"/mnt/storage/projects/app/.env.gone")
	res, err := Render(cfg, hostFor(T))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), ".env.gone does not exist") {
		t.Fatalf("warnings %v", res.Warnings)
	}
}
