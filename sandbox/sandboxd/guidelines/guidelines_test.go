package guidelines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

func exampleConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse(config.Example())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEmbeddedTemplateMatchesImage(t *testing.T) {
	img, err := os.ReadFile("../../image/rootfs/etc/headmesh/agent-guidelines.md.tmpl")
	if err != nil {
		t.Skipf("image template not found: %v", err)
	}
	if string(img) != defaultTemplate {
		t.Fatal("sandboxd/guidelines/default.md.tmpl drifted from image/rootfs/etc/headmesh/agent-guidelines.md.tmpl; copy it over")
	}
}

func TestRenderModes(t *testing.T) {
	cfg := exampleConfig(t)
	reg, err := Render("", FromConfig(cfg, config.ModeRegular, time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Active mode: Regular.**", "`/mnt/storage/projects/xcelsior`", "`aabiro/xcelsior` → `/mnt/storage/projects/xcelsior`", EndMarker} {
		if !strings.Contains(reg, want) {
			t.Errorf("regular render lacks %q", want)
		}
	}
	if strings.Contains(reg, "Do not ingest untrusted content") {
		t.Error("regular render has the YOLO warning")
	}
	exp := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	yolo, err := Render("", FromConfig(cfg, config.ModeYOLO, exp))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Active mode: YOLO.**", "Do not ingest untrusted content", "halts at", "`/home/aaryn`"} {
		if !strings.Contains(yolo, want) {
			t.Errorf("yolo render lacks %q", want)
		}
	}
	if _, err := Render("{{.Nope}}", Data{}); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestMergeKeepsUserContent(t *testing.T) {
	block := BeginMarker + " x -->\nmanaged v1\n" + EndMarker + "\n"
	merged := Merge("my own notes\n", block)
	if merged != block+"\nmy own notes\n" {
		t.Fatalf("prepend: %q", merged)
	}
	block2 := BeginMarker + " x -->\nmanaged v2\n" + EndMarker + "\n"
	again := Merge(merged, block2)
	if again != block2+"\nmy own notes\n" {
		t.Fatalf("replace: %q", again)
	}
	if Merge("", block) != block {
		t.Fatal("empty file")
	}
	p := filepath.Join(t.TempDir(), ".claude", "CLAUDE.md")
	if err := WriteInto(p, block); err != nil {
		t.Fatal(err)
	}
	if err := WriteInto(p, block2); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != block2 {
		t.Fatalf("file %q", b)
	}
}

func TestBannerAndInstructions(t *testing.T) {
	d := FromConfig(exampleConfig(t), config.ModeYOLO, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if b := Banner(d, "1 other agent active"); !strings.Contains(b, "mode YOLO until") || !strings.Contains(b, "1 other agent") {
		t.Fatalf("banner %q", b)
	}
	if in := Instructions(d); !strings.Contains(in, "Active mode: YOLO") || !strings.Contains(in, "agents_register") {
		t.Fatalf("instructions %q", in)
	}
}
