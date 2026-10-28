package scansecrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

func TestByName(t *testing.T) {
	root := t.TempDir()
	files := []string{
		".env", ".env.staging", ".env.example", "app/.env.worker.staging", "certs/server.pem", "certs/server.pub",
		"keys/id_ed25519", "keys/id_ed25519.pub", "gcp/credentials-prod.json", ".netrc", "node_modules/x/.env",
		"src/main.go", "deploy/secrets.yaml", "tests/fixtures/test.key",
	}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := ByName([]string{root}, 0)
	var names []string
	for _, f := range got {
		names = append(names, strings.TrimPrefix(f.Path, root+"/"))
	}
	want := ".env,.env.staging,.netrc,app/.env.worker.staging,certs/server.pem,deploy/secrets.yaml,gcp/credentials-prod.json,keys/id_ed25519"
	if strings.Join(names, ",") != want {
		t.Fatalf("got %v", names)
	}
}

func TestApplyMasksKeepsTheFileValid(t *testing.T) {
	src := string(config.Example())
	out, n, ok := ApplyMasks(src, []string{"/mnt/storage/projects/xcelsior/.env", "/mnt/storage/projects/headmesh/.env"})
	if !ok || n != 1 {
		t.Fatalf("ok=%v n=%d", ok, n)
	}
	if !strings.Contains(out, `"/mnt/storage/projects/headmesh/.env",`) || !strings.Contains(out, "# Files replaced by /dev/null") {
		t.Fatalf("masks not added or comments lost:\n%s", out)
	}
	cfg, err := config.Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mounts.Masks) != 5 {
		t.Fatalf("masks %v", cfg.Mounts.Masks)
	}
	empty := strings.Replace(src, src[strings.Index(src, "masks = ["):strings.Index(src, "]\n# Denylist")+1], "masks = []", 1)
	out, n, ok = ApplyMasks(empty, []string{"/x/.env"})
	if !ok || n != 1 || !strings.Contains(out, "masks = [\n  \"/x/.env\",\n]") {
		t.Fatalf("empty array: %v %d\n%s", ok, n, out[strings.Index(out, "masks"):strings.Index(out, "masks")+40])
	}
}
