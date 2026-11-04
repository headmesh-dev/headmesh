package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

func TestMaskProbeDistinguishesHiddenAndVisibleFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		dir  bool
		full bool
		want string
	}{
		{"empty-file", false, false, "0"},
		{"visible-file", false, true, "7"},
		{"empty-dir", true, false, "0"},
		{"visible-dir", true, true, "directory-visible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if tc.dir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if tc.full {
					if err := os.WriteFile(filepath.Join(path, "entry"), []byte("visible"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				var content []byte
				if tc.full {
					content = []byte("visible")
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command("sh", "-c", maskProbe(path)).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("probe = %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestSecretEnv(t *testing.T) {
	a := &app{root: t.TempDir()}
	if err := a.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	write := func(name, v string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(a.path("secrets", name), []byte(v), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("ts_authkey", "  key-123\n", 0o600)
	write("github_ssh_key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----", 0o600)

	env, missing, err := a.secretEnv(config.ModeRegular)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "HM_SECRET_TS_AUTHKEY=key-123") || !slices.Contains(env, "HM_SECRET_GITHUB_SSH_KEY=") {
		t.Fatalf("regular env %q", env)
	}
	if strings.Join(missing, ",") != "github_token,claude_env_secret,openai_tunnel_key" {
		t.Fatalf("missing %v", missing)
	}
	env, _, err = a.secretEnv(config.ModeYOLO)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "HM_SECRET_GITHUB_SSH_KEY=") && !strings.HasSuffix(e, "-----END OPENSSH PRIVATE KEY-----\n") {
			t.Fatalf("yolo ssh key %q", e)
		}
	}

	write("github_token", "x", 0o644)
	if err := os.Chmod(a.path("secrets", "github_token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.secretEnv(config.ModeRegular); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable secret accepted: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(a.root, "secrets")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode %v", fi.Mode().Perm())
	}
}

func TestLineDiff(t *testing.T) {
	got := lineDiff("a\nb\nc\n", "a\nc\nd\n")
	if got != "- b\n+ d\n" {
		t.Fatalf("diff %q", got)
	}
	if lineDiff("same\n", "same\n") != "" {
		t.Fatal("identical inputs produced a diff")
	}
}

func TestProfileFor(t *testing.T) {
	if p, s := profileFor(config.ModeYOLO); p != "yolo" || s != "sandbox-yolo" {
		t.Fatal(p, s)
	}
	a := &app{root: "/r"}
	args := strings.Join(a.composeArgs([]string{"regular"}, "up", "-d"), " ")
	want := "compose --project-name headmesh-sandbox --project-directory /r/compose -f /r/compose/compose.yaml -f /r/compose/compose.override.generated.yaml --profile regular up -d"
	if args != want {
		t.Fatalf("compose args\n%s\nwant\n%s", args, want)
	}
}
