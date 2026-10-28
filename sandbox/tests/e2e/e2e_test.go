// Package e2e drives a running sandbox container. It is skipped unless
// HEADMESH_E2E_ROOT points at a sandbox root whose container is up:
//
//	HEADMESH_E2E_ROOT=/mnt/storage/headmesh/sandbox \
//	HEADMESH_E2E_URL=http://172.31.250.10:8780/mcp \
//	HEADMESH_E2E_TOKEN_FILE=… go test ./tests/e2e -v
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/headmesh-dev/headmesh/sandbox/internal/sockclient"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func transports(t *testing.T) map[string]*mcp.StreamableClientTransport {
	t.Helper()
	root := os.Getenv("HEADMESH_E2E_ROOT")
	if root == "" {
		t.Skip("set HEADMESH_E2E_ROOT to run against a live sandbox")
	}
	out := map[string]*mcp.StreamableClientTransport{
		"unix": {Endpoint: "http://sandboxd/mcp", HTTPClient: sockclient.New(filepath.Join(root, "run", "sandboxd.sock")).HTTPClient()},
	}
	if url := os.Getenv("HEADMESH_E2E_URL"); url != "" {
		tok, err := os.ReadFile(os.Getenv("HEADMESH_E2E_TOKEN_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		out["http"] = &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{strings.TrimSpace(string(tok)), http.DefaultTransport}}}
	}
	return out
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		t.Fatalf("%s returned an error: %s", name, b.String())
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestLiveSandbox(t *testing.T) {
	for name, tr := range transports(t) {
		t.Run(name, func(t *testing.T) {
			for _, version := range []string{"2026-07-28", "2025-11-25"} {
				client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
				cs, err := client.Connect(context.Background(), tr, &mcp.ClientSessionOptions{ProtocolVersion: version})
				if err != nil {
					t.Fatalf("connect %s: %v", version, err)
				}
				tools, err := cs.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatalf("tools/list %s: %v", version, err)
				}
				var names []string
				for _, tl := range tools.Tools {
					names = append(names, tl.Name)
				}
				for _, want := range []string{"exec", "session_create", "session_read", "fs_write", "fs_edit", "fs_search", "git_status", "sandbox_status", "net_check"} {
					if !slices.Contains(names, want) {
						t.Fatalf("%s: tools/list lacks %s (%v)", version, want, names)
					}
				}
				t.Logf("%s over %s: %d tools", version, name, len(names))
				_ = cs.Close()
			}

			client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
			cs, err := client.Connect(context.Background(), tr, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()

			ex := call(t, cs, "exec", map[string]any{"command": "id -un; id -u; . /etc/os-release; echo $VERSION_ID; echo $HEADMESH_MODE"})
			if ex["exit_code"].(float64) != 0 || !strings.HasPrefix(ex["stdout"].(string), "agent\n1000\n26.04\n") {
				t.Fatalf("exec %v", ex)
			}

			dir := "/home/agent/e2e-" + name
			call(t, cs, "fs_write", map[string]any{"path": dir + "/hello.txt", "content": "alpha\nbeta\n", "create_dirs": true})
			call(t, cs, "fs_edit", map[string]any{"path": dir + "/hello.txt", "old_string": "beta", "new_string": "gamma"})
			rd := call(t, cs, "fs_read", map[string]any{"path": dir + "/hello.txt"})
			if rd["content"] != "     1\talpha\n     2\tgamma\n" {
				t.Fatalf("fs_read %v", rd)
			}
			if sr := call(t, cs, "fs_search", map[string]any{"pattern": "gamma", "path": dir}); sr["count"].(float64) != 1 {
				t.Fatalf("fs_search %v", sr)
			}
			if st := call(t, cs, "fs_stat", map[string]any{"path": dir + "/hello.txt"}); st["uid"].(float64) != 1000 {
				t.Fatalf("fs_stat %v", st)
			}
			call(t, cs, "fs_move", map[string]any{"src": dir + "/hello.txt", "dst": dir + "/moved.txt"})
			if ls := call(t, cs, "fs_list", map[string]any{"path": dir}); len(ls["entries"].([]any)) != 1 {
				t.Fatalf("fs_list %v", ls)
			}
			del := call(t, cs, "fs_delete", map[string]any{"path": dir, "recursive": true})
			if !strings.Contains(del["trashed_to"].(string), ".headmesh-trash") {
				t.Fatalf("fs_delete %v", del)
			}

			s := call(t, cs, "session_create", map[string]any{"name": "e2e-" + name})
			id := s["id"].(string)
			call(t, cs, "session_send", map[string]any{"id": id, "input": "echo e2e-$((6*7))-$(id -u)", "enter": true})
			var got string
			var off float64
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !strings.Contains(got, "e2e-42-1000"); {
				r := call(t, cs, "session_read", map[string]any{"id": id, "offset": off, "wait_ms": 2000})
				got += r["data"].(string)
				off = r["next_offset"].(float64)
			}
			if !strings.Contains(got, "e2e-42-1000") {
				t.Fatalf("session output %q", got)
			}
			list := call(t, cs, "session_list", nil)
			found := false
			for _, x := range list["sessions"].([]any) {
				m := x.(map[string]any)
				if m["id"] == id && m["state"] == "running" {
					found = true
				}
			}
			if !found {
				t.Fatalf("session %s not running in %v", id, list)
			}
			k := call(t, cs, "session_kill", map[string]any{"id": id, "remove": true})
			if k["state"] == "running" {
				t.Fatalf("session_kill %v", k)
			}
			st := call(t, cs, "sandbox_status", nil)
			if st["mode"] == nil || st["boot_id"] == "" {
				t.Fatalf("sandbox_status %v", st)
			}
		})
	}
}
