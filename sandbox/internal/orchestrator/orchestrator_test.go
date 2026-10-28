package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI emulates the Agents API endpoints the orchestrator uses.
type fakeAPI struct {
	mu        sync.Mutex
	created   map[string]any
	messages  []string
	cancelled bool
	connected chan struct{}
	failTurn  bool
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OpenAI-Beta") != "agents=v1" || r.Header.Get("Authorization") != "Bearer app-key" {
			http.Error(w, "bad headers", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"asess_1","status":"active","environment":{"id":"env_1","type":"self_hosted","remote_url":"https://remote.example/env"}}`)
	})
	mux.HandleFunc("GET /v1/agents/sessions/asess_1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		send := func(v string) {
			// SSE encodes multiline event data as one `data:` field per line.
			for _, line := range strings.Split(v, "\n") {
				_, _ = fmt.Fprintf(w, "data: %s\n", line)
			}
			_, _ = io.WriteString(w, "\n")
			fl.Flush()
		}
		_, _ = io.WriteString(w, ": keepalive\n\n")
		send(`{"type":"agent.session.environment.pending"}`)
		select {
		case <-f.connected:
		case <-r.Context().Done():
			return
		}
		send(`{"type":"agent.session.environment.connected"}`)
		// Wait for the prompt.
		for i := 0; i < 200; i++ {
			f.mu.Lock()
			n := len(f.messages)
			f.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		send(`{"type":"agent.session.turn.output_text.delta","delta":"sub ","turn":{"id":"t2","subagent_id":"sa_1"}}`)
		send(`{"type":"agent.session.turn.completed","turn":{"id":"t2","subagent_id":"sa_1"}}`)
		send(`{"type":"agent.session.turn.output_text.delta","delta":"5/5 checks ","turn":{"id":"t1"}}`)
		// One event split over two data lines.
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"agent.session.turn.output_text.delta\",\ndata: \"delta\":\"passed\",\"turn\":{\"id\":\"t1\"}}\n\n")
		fl.Flush()
		send(`{"type":"agent.session.turn.output_text.done","text":"5/5 checks passed","turn":{"id":"t1"}}`)
		if f.failTurn {
			send(`{"type":"agent.session.turn.failed","turn":{"id":"t1"}}`)
			return
		}
		send(`{"type":"agent.session.turn.completed","turn":{"id":"t1"}}`)
	})
	mux.HandleFunc("POST /v1/agents/sessions/asess_1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		var body struct {
			Events []struct {
				Type  string `json:"type"`
				Input []struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"input"`
			} `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, e := range body.Events {
			switch e.Type {
			case "agent.session.input.message":
				f.messages = append(f.messages, e.Input[0].Content[0].Text)
			case "agent.session.input.cancel":
				f.cancelled = true
			}
		}
		_, _ = io.WriteString(w, `{}`)
	})
	return mux
}

type fakeExec struct {
	api     *fakeAPI
	remote  string
	envID   string
	stopped bool
	noShow  bool
}

func (e *fakeExec) Start(_ context.Context, remote, env string) (func() error, error) {
	e.remote, e.envID = remote, env
	if !e.noShow {
		close(e.api.connected)
	}
	return func() error { e.stopped = true; return nil }, nil
}

func TestRunCompletes(t *testing.T) {
	api := &fakeAPI{connected: make(chan struct{})}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	ex := &fakeExec{api: api}
	var out, log, tr bytes.Buffer
	res, err := Run(context.Background(), &Client{BaseURL: srv.URL, AppKey: "app-key"}, ex, Options{
		Model: "gpt-x", Instructions: "be careful", Workspace: "/mnt/storage/projects/xcelsior", Prompt: "run the acceptance checks",
		Output: &out, Log: &log, Transcript: &tr,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if res.Outcome != "completed" || res.SessionID != "asess_1" {
		t.Fatalf("result %+v", res)
	}
	if out.String() != "5/5 checks passed\n" {
		t.Fatalf("output %q (subagent text must be skipped)", out.String())
	}
	env := api.created["environment"].(map[string]any)
	if env["type"] != "self_hosted" || env["workspace_directory"] != "/mnt/storage/projects/xcelsior" {
		t.Fatalf("created %v", api.created)
	}
	if ex.remote != "https://remote.example/env" || ex.envID != "env_1" || !ex.stopped {
		t.Fatalf("executor %+v", ex)
	}
	if len(api.messages) != 1 || api.messages[0] != "run the acceptance checks" {
		t.Fatalf("messages %v", api.messages)
	}
	if lines := strings.Count(tr.String(), "\n"); lines != 8 {
		t.Fatalf("transcript has %d lines:\n%s", lines, tr.String())
	}
}

func TestRunTurnFailed(t *testing.T) {
	api := &fakeAPI{connected: make(chan struct{}), failTurn: true}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	res, err := Run(context.Background(), &Client{BaseURL: srv.URL, AppKey: "app-key"}, &fakeExec{api: api}, Options{Prompt: "x"})
	if !errors.Is(err, ErrTurnFailed) || res.Outcome != "failed" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRunConnectTimeoutAndCancel(t *testing.T) {
	api := &fakeAPI{connected: make(chan struct{})}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	_, err := Run(context.Background(), &Client{BaseURL: srv.URL, AppKey: "app-key"}, &fakeExec{api: api, noShow: true},
		Options{Prompt: "x", ConnectTimeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "did not connect") {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, &Client{BaseURL: srv.URL, AppKey: "app-key"}, &fakeExec{api: api, noShow: true}, Options{Prompt: "x"})
	if err == nil || res.Outcome != "cancelled" || !api.cancelled {
		t.Fatalf("cancel: %+v %v cancelled=%v", res, err, api.cancelled)
	}
}

func TestBadKey(t *testing.T) {
	api := &fakeAPI{connected: make(chan struct{})}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	_, err := Run(context.Background(), &Client{BaseURL: srv.URL, AppKey: "wrong"}, &fakeExec{api: api}, Options{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("got %v", err)
	}
}

func TestParseSSE(t *testing.T) {
	var got []string
	in := "event: x\ndata: a\ndata: b\n\n: c\n\ndata: c\n\ndata: [DONE]\n\ndata: never\n\n"
	if err := ParseSSE(strings.NewReader(in), func(b []byte) bool { got = append(got, string(b)); return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "a\nb|c" {
		t.Fatalf("got %q", got)
	}
}
