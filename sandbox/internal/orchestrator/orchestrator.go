// Package orchestrator drives a Codex session through the OpenAI Agents API
// with the sandbox as its self-hosted environment (plan §7.6). The
// application key stays on the host; only the restricted environment key
// reaches the sandbox, through the executor's stdin.
//
// Flow (per developers.openai.com/api/docs/guides/agents-api, Oct 2026):
//  1. POST /v1/agents/sessions with environment {type: self_hosted,
//     workspace_directory}.
//  2. Open GET /v1/agents/sessions/{id}/events?stream=true (SSE).
//  3. Start `codex exec-server --remote <remote_url> --environment-id <id>`
//     in the sandbox; wait for agent.session.environment.connected.
//  4. POST the prompt as an agent.session.input.message event.
//  5. Print output deltas until the root turn completes, fails or is
//     cancelled; then close the executor's stdin so it exits.
package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the OpenAI API.
const DefaultBaseURL = "https://api.openai.com"

// Client is a minimal Agents API client.
type Client struct {
	BaseURL string
	AppKey  string
	HTTP    *http.Client
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimSuffix(c.BaseURL, "/")
}

func (c *Client) req(ctx context.Context, method, path string, body any, extra map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("OpenAI-Beta", "agents=v1")
	req.Header.Set("Authorization", "Bearer "+c.AppKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Session is the part of the session object the orchestrator needs.
type Session struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Environment struct {
		ID        string `json:"id"`
		RemoteURL string `json:"remote_url"`
		Type      string `json:"type"`
	} `json:"environment"`
}

// CreateSession starts a session whose environment is the sandbox.
func (c *Client) CreateSession(ctx context.Context, model, instructions, workspace string) (Session, error) {
	body := map[string]any{
		"agent":       map[string]any{"model": model, "instructions": instructions},
		"environment": map[string]any{"type": "self_hosted", "workspace_directory": workspace},
	}
	resp, err := c.req(ctx, http.MethodPost, "/v1/agents/sessions", body, nil)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var s Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Session{}, fmt.Errorf("decode session: %w", err)
	}
	if s.ID == "" || s.Environment.ID == "" || s.Environment.RemoteURL == "" {
		return s, errors.New("session response lacks id, environment.id or environment.remote_url")
	}
	return s, nil
}

func idempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SendMessage submits user input.
func (c *Client) SendMessage(ctx context.Context, sessionID, text string) error {
	body := map[string]any{"events": []any{map[string]any{
		"type": "agent.session.input.message",
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}},
	}}}
	resp, err := c.req(ctx, http.MethodPost, "/v1/agents/sessions/"+sessionID+"/events", body, map[string]string{"Idempotency-Key": idempotencyKey()})
	if err != nil {
		return err
	}
	return resp.Body.Close() //nolint:wrapcheck // close of a drained body
}

// Cancel asks the agent to stop its current turn.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	body := map[string]any{"events": []any{map[string]any{"type": "agent.session.input.cancel"}}}
	resp, err := c.req(ctx, http.MethodPost, "/v1/agents/sessions/"+sessionID+"/events", body, map[string]string{"Idempotency-Key": idempotencyKey()})
	if err != nil {
		return err
	}
	return resp.Body.Close() //nolint:wrapcheck // close of a drained body
}

// Event is one stream event; Raw keeps the full JSON.
type Event struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Text  string `json:"text"`
	Turn  struct {
		ID         string `json:"id"`
		SubagentID string `json:"subagent_id"`
	} `json:"turn"`
	Raw json.RawMessage `json:"-"`
}

// Root reports whether the event belongs to the root agent (not a subagent).
func (e Event) Root() bool { return e.Turn.SubagentID == "" }

// Stream opens the SSE event stream and sends events to out until the
// stream ends or ctx is done. The returned channel carries the final error.
func (c *Client) Stream(ctx context.Context, sessionID string, out chan<- Event) <-chan error {
	errc := make(chan error, 1)
	resp, err := c.req(ctx, http.MethodGet, "/v1/agents/sessions/"+sessionID+"/events?stream=true", nil, map[string]string{"Accept": "text/event-stream"}) //nolint:bodyclose // closed by the reader goroutine
	if err != nil {
		errc <- err
		close(out)
		return errc
	}
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(out)
		errc <- ParseSSE(resp.Body, func(data []byte) bool {
			var ev Event
			if json.Unmarshal(data, &ev) != nil {
				return true
			}
			ev.Raw = append(json.RawMessage(nil), data...)
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return errc
}

// ParseSSE calls fn with each event's data (multi-line data joined by
// newlines) until fn returns false or the reader ends.
func ParseSSE(r io.Reader, fn func([]byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var data bytes.Buffer
	flush := func() bool {
		if data.Len() == 0 {
			return true
		}
		b := bytes.Clone(data.Bytes())
		data.Reset()
		if bytes.Equal(b, []byte("[DONE]")) {
			return false
		}
		return fn(b)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event stream: %w", err)
	}
	flush()
	return nil
}

// Executor runs `codex exec-server` in the sandbox. Stop closes its stdin
// (the server runs with --exit-on-stdin-close) and waits for it.
type Executor interface {
	Start(ctx context.Context, remoteURL, environmentID string) (stop func() error, err error)
}

// Options for one run.
type Options struct {
	Model            string
	Instructions     string
	Workspace        string
	Prompt           string
	ConnectTimeout   time.Duration
	Output           io.Writer // assistant text
	Log              io.Writer // progress lines
	Transcript       io.Writer // every event as JSONL (optional)
	OnSessionCreated func(Session)
}

// Result summarizes a run.
type Result struct {
	SessionID string `json:"session_id"`
	Outcome   string `json:"outcome"` // completed, failed, cancelled
	Events    int    `json:"events"`
}

// ErrTurnFailed is returned when the root turn fails or is cancelled.
var ErrTurnFailed = errors.New("agent turn did not complete")

// Run executes one task end to end.
func Run(ctx context.Context, c *Client, ex Executor, o Options) (Result, error) {
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 2 * time.Minute
	}
	logf := func(format string, a ...any) {
		if o.Log != nil {
			_, _ = fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	sess, err := c.CreateSession(ctx, o.Model, o.Instructions, o.Workspace)
	if err != nil {
		return Result{}, err
	}
	res := Result{SessionID: sess.ID}
	if o.OnSessionCreated != nil {
		o.OnSessionCreated(sess)
	}
	logf("session %s created; environment %s", sess.ID, sess.Environment.ID)

	sctx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events := make(chan Event, 64)
	streamErr := c.Stream(sctx, sess.ID, events)

	stop, err := ex.Start(ctx, sess.Environment.RemoteURL, sess.Environment.ID)
	if err != nil {
		return res, fmt.Errorf("start executor: %w", err)
	}
	defer func() {
		if serr := stop(); serr != nil {
			logf("executor exit: %v", serr)
		}
	}()

	connected, sent := false, false
	timer := time.NewTimer(o.ConnectTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_ = c.Cancel(cctx, sess.ID)
			cancel()
			res.Outcome = "cancelled"
			return res, fmt.Errorf("interrupted: %w", ctx.Err())
		case <-timer.C:
			if !connected {
				return res, fmt.Errorf("executor did not connect within %s; check `headmesh-sandbox logs` and the environment key", o.ConnectTimeout)
			}
		case ev, ok := <-events:
			if !ok {
				err := <-streamErr
				if err == nil {
					err = errors.New("event stream closed before the turn ended; the session's saved items have the work")
				}
				return res, err
			}
			res.Events++
			if o.Transcript != nil {
				var compact bytes.Buffer
				transcriptEvent := ev.Raw
				if err := json.Compact(&compact, ev.Raw); err == nil {
					transcriptEvent = compact.Bytes()
				}
				_, _ = o.Transcript.Write(append(bytes.Clone(transcriptEvent), '\n'))
			}
			switch ev.Type {
			case "agent.session.environment.pending":
				logf("waiting for the sandbox executor to connect…")
			case "agent.session.environment.connected":
				connected = true
				logf("sandbox executor connected")
				if !sent {
					if err := c.SendMessage(ctx, sess.ID, o.Prompt); err != nil {
						return res, err
					}
					sent = true
				}
			case "agent.session.environment.failed", "agent.session.failed", "error":
				res.Outcome = "failed"
				return res, fmt.Errorf("%s: %s", ev.Type, ev.Raw)
			case "agent.session.turn.output_text.delta":
				if ev.Root() && o.Output != nil {
					_, _ = io.WriteString(o.Output, ev.Delta)
				}
			case "agent.session.turn.output_text.done":
				if ev.Root() && o.Output != nil {
					_, _ = io.WriteString(o.Output, "\n")
				}
			case "agent.session.requires_action":
				logf("the agent requires an action this orchestrator can't provide: %s", ev.Raw)
			case "agent.session.turn.completed":
				if ev.Root() {
					res.Outcome = "completed"
					return res, nil
				}
			case "agent.session.turn.failed", "agent.session.turn.cancelled":
				if ev.Root() {
					res.Outcome = strings.TrimPrefix(ev.Type, "agent.session.turn.")
					return res, fmt.Errorf("%w (%s): %s", ErrTurnFailed, res.Outcome, ev.Raw)
				}
			}
		}
	}
}
