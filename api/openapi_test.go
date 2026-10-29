package api_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/headmesh-dev/headmesh/api"
	"github.com/headmesh-dev/headmesh/internal/service"
)

var update = flag.Bool("update", false, "rewrite api/openapi/openapi.yaml from the registered operations")

// specVersion is the info.version of the committed document. It names the API generation (/v1), not a build, so the
// file only changes when the API does.
const specVersion = "1"

// TestOpenAPIIsCommitted keeps api/openapi/openapi.yaml in step with the code (plan §9.8). CI also compares it with
// the base branch's copy and fails on breaking changes (ci-go.yml, oasdiff). Regenerate with
//
//	go test ./api -run TestOpenAPIIsCommitted -update
func TestOpenAPIIsCommitted(t *testing.T) {
	a := api.New(chi.NewRouter(), api.Options{Service: &service.Service{}, Version: specVersion})
	got, err := a.OpenAPI().YAML()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("openapi", "openapi.yaml")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (generate it with: go test ./api -run TestOpenAPIIsCommitted -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/openapi/openapi.yaml is out of date; regenerate it with: go test ./api -run TestOpenAPIIsCommitted -update")
	}
}
