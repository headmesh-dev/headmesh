// Package sockclient talks to headmesh-sandboxd over its unix socket. The
// host CLI uses the bind-mounted copy (${root}/run/sandboxd.sock); helpers
// inside the container use /run/headmesh/sandboxd.sock.
package sockclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultSocket is the in-container socket path.
const DefaultSocket = "/run/headmesh/sandboxd.sock"

// Client calls sandboxd.
type Client struct {
	Socket string
	http   *http.Client
}

// New returns a client for socket (DefaultSocket when empty).
func New(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	return &Client{Socket: socket, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}}
}

// HTTPClient returns an http.Client that dials the socket (for MCP clients).
func (c *Client) HTTPClient() *http.Client { return c.http }

// Error is a non-2xx answer.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("sandboxd %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://sandboxd"+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandboxd unreachable at %s: %w", c.Socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusServiceUnavailable {
		var e struct{ Error string }
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = string(bytes.TrimSpace(raw))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return &Error{Status: resp.StatusCode, Message: "unhealthy"}
	}
	return nil
}

// Health is /v1/health.
type Health struct {
	OK       bool     `json:"ok"`
	Mode     string   `json:"mode"`
	Problems []string `json:"problems"`
}

// Health returns sandboxd's health; err is non-nil when unreachable.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
	var se *Error
	if errors.As(err, &se) && se.Status == http.StatusServiceUnavailable {
		return h, nil
	}
	return h, err
}

// Status returns the raw status report.
func (c *Client) Status(ctx context.Context, probeReach bool) (json.RawMessage, error) {
	path := "/v1/status"
	if probeReach {
		path += "?probe_reach=1"
	}
	var raw json.RawMessage
	err := c.do(ctx, http.MethodGet, path, nil, &raw)
	return raw, err
}

// Call invokes a tool and decodes its result into out (if non-nil).
func (c *Client) Call(ctx context.Context, tool string, args any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/call/"+tool, args, &env); err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", tool, err)
		}
	}
	return nil
}

// WaitHealthy polls until sandboxd answers healthy or the timeout passes.
func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) (Health, error) {
	deadline := time.Now().Add(timeout)
	var last Health
	var lastErr error
	for {
		h, err := c.Health(ctx)
		if err == nil && h.OK {
			return h, nil
		}
		last, lastErr = h, err
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("unhealthy: %v", last.Problems)
			}
			return last, lastErr
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("wait healthy: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
