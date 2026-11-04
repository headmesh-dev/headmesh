package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUnknownMethodKeepsRPCIDAndAuthentication(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"", f.readToken} {
		request, err := http.NewRequest(http.MethodPost, f.http.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":"probe-42","method":"does/not/exist","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if token == "" {
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("anonymous request bypassed authentication: %d %s", response.StatusCode, body)
			}
			continue
		}
		var result struct {
			Version string             `json:"jsonrpc"`
			ID      string             `json:"id"`
			Error   struct{ Code int } `json:"error"`
		}
		if err := json.Unmarshal(body, &result); err != nil || result.Version != "2.0" || result.ID != "probe-42" || result.Error.Code != -32601 {
			t.Fatalf("invalid unknown-method response: %d %s", response.StatusCode, body)
		}
	}
}
