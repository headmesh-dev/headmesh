// Package cli implements the headmesh command-line tool.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a small typed client for the Headmesh REST API.
type Client struct {
	BaseURL   string
	APIKey    string
	HTTP      *http.Client
	UserAgent string
}

// APIError is an RFC 9457 problem returned by the server.
type APIError struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%d): %s", e.Title, e.Status, e.Detail)
	}
	return fmt.Sprintf("%s (%d)", e.Title, e.Status)
}

// ErrNoAPIKey is returned for commands that need HEADMESH_API_KEY when it is unset.
var ErrNoAPIKey = errors.New("this command needs an API key: set HEADMESH_API_KEY")

func (c *Client) do(ctx context.Context, method, path string, auth bool, in, out any) error {
	if auth && c.APIKey == "" {
		return ErrNoAPIKey
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Title: http.StatusText(resp.StatusCode)}
		_ = json.Unmarshal(raw, apiErr)
		return apiErr
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// EnrollResponse mirrors the API's enroll response.
type EnrollResponse struct {
	EnrollmentID     string    `json:"enrollment_id"`
	DeviceID         string    `json:"device_id"`
	LoginServer      string    `json:"login_server"`
	AuthKey          string    `json:"auth_key"`
	Hostname         string    `json:"hostname"`
	Tags             []string  `json:"tags"`
	ExpiresAt        time.Time `json:"expires_at"`
	CompletionSecret string    `json:"completion_secret"`
}

// Device mirrors the API's device.
type Device struct {
	ID        string   `json:"id"`
	ShortID   string   `json:"short_id"`
	Name      string   `json:"name"`
	GivenName string   `json:"given_name"`
	Kind      string   `json:"kind"`
	Roles     []string `json:"roles"`
	IPv4      string   `json:"ipv4,omitempty"`
	IPv6      string   `json:"ipv6,omitempty"`
	Online    bool     `json:"online"`
	State     string   `json:"state"`
}

// Grant mirrors the API's grant.
type Grant struct {
	ID          string    `json:"id"`
	GranteeOrg  string    `json:"grantee_org,omitempty"`
	DstDeviceID string    `json:"dst_device_id"`
	Ports       []string  `json:"ports"`
	StartsAt    time.Time `json:"starts_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Revoked     bool      `json:"revoked"`
	ExternalRef string    `json:"external_ref,omitempty"`
	Enforced    bool      `json:"enforced"`
}

// Enroll exchanges an enrollment token for a pre-auth key.
func (c *Client) Enroll(ctx context.Context, token, hostname, goos, arch string) (*EnrollResponse, error) {
	var out EnrollResponse
	err := c.do(ctx, http.MethodPost, "/v1/enroll", false,
		map[string]string{"token": token, "hostname": hostname, "os": goos, "arch": arch}, &out)
	return &out, err
}

// CompleteEnrollment binds the registered node to the device.
func (c *Client) CompleteEnrollment(ctx context.Context, enrollmentID, secret string, nodeID uint64) (*Device, error) {
	var out Device
	err := c.do(ctx, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/complete", false,
		map[string]any{"completion_secret": secret, "node_id": nodeID}, &out)
	return &out, err
}

// Devices lists the org's devices.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/devices", true, nil, &out)
	return out.Devices, err
}

// InviteResponse mirrors the API's enrollment token response.
type InviteResponse struct {
	ID        string            `json:"id"`
	Token     string            `json:"token"`
	JoinURL   string            `json:"join_url"`
	Commands  map[string]string `json:"commands"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// CreateInvite creates an enrollment token.
func (c *Client) CreateInvite(ctx context.Context, roles []string, kind string, uses int, ttl time.Duration, ephemeral bool) (*InviteResponse, error) {
	var out InviteResponse
	err := c.do(ctx, http.MethodPost, "/v1/enrollment-tokens", true, map[string]any{
		"roles": roles, "device_kind": kind, "max_uses": uses, "ttl_seconds": int64(ttl.Seconds()), "ephemeral": ephemeral,
	}, &out)
	return &out, err
}

// CreateGrant gives another org time-limited access to one device.
func (c *Client) CreateGrant(ctx context.Context, req map[string]any) (*Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodPost, "/v1/grants", true, req, &out)
	return &out, err
}

// Grants lists active grants.
func (c *Client) Grants(ctx context.Context) ([]Grant, error) {
	var out struct {
		Grants []Grant `json:"grants"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/grants", true, nil, &out)
	return out.Grants, err
}

// RevokeGrant revokes a grant.
func (c *Client) RevokeGrant(ctx context.Context, id string) (*Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodPost, "/v1/grants/"+id+"/revoke", true, nil, &out)
	return &out, err
}

// AuditEvent mirrors one entry of the API's audit log.
type AuditEvent struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Data       json.RawMessage `json:"data"`
	Hash       string          `json:"hash"`
}

// AuditEvents returns up to limit audit events after the given id, and the id to pass for the next page.
func (c *Client) AuditEvents(ctx context.Context, after int64, limit int) ([]AuditEvent, int64, error) {
	var out struct {
		Events    []AuditEvent `json:"events"`
		NextAfter int64        `json:"next_after"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/audit-events?after=%d&limit=%d", after, limit), true, nil, &out)
	return out.Events, out.NextAfter, err
}

// AuditVerification mirrors the API's chain verification result.
type AuditVerification struct {
	Verified bool   `json:"verified"`
	Checked  int    `json:"checked"`
	Problem  string `json:"problem,omitempty"`
}

// VerifyAudit asks the server to recompute the org's audit hash chain.
func (c *Client) VerifyAudit(ctx context.Context) (*AuditVerification, error) {
	var out AuditVerification
	err := c.do(ctx, http.MethodGet, "/v1/audit-events/verify", true, nil, &out)
	return &out, err
}
