package api_test

import (
	"encoding/json"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/headmesh-dev/headmesh/api"
	"github.com/headmesh-dev/headmesh/internal/service"
)

// TestEveryOperationIsAuthenticatedOrAllowlisted fails if a route is added without the bearer requirement and
// without a deliberate entry in api.PublicOperations.
func TestEveryOperationIsAuthenticatedOrAllowlisted(t *testing.T) {
	a := api.New(chi.NewRouter(), api.Options{Service: &service.Service{}, Version: "test"})
	seen := map[string]bool{}
	for path, item := range a.OpenAPI().Paths {
		for method, op := range map[string]any{"GET": item.Get, "POST": item.Post, "PUT": item.Put, "PATCH": item.Patch, "DELETE": item.Delete} {
			b, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			var o struct {
				OperationID string                `json:"operationId"`
				Security    []map[string][]string `json:"security"`
			}
			if string(b) == "null" {
				continue
			}
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatal(err)
			}
			seen[o.OperationID] = true
			if len(o.Security) == 0 && !api.PublicOperations[o.OperationID] {
				t.Errorf("%s %s (%s) has no security requirement and is not allowlisted", method, path, o.OperationID)
			}
			if len(o.Security) > 0 && api.PublicOperations[o.OperationID] {
				t.Errorf("%s is allowlisted but also requires auth; drop it from PublicOperations", o.OperationID)
			}
		}
	}
	for id := range api.PublicOperations {
		if !seen[id] {
			t.Errorf("allowlisted operation %s does not exist", id)
		}
	}
}
