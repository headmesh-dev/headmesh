package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ExportAudit streams the org's audit log as NDJSON (GET /v1/audit-events/export) into w, events after the given id.
func (c *Client) ExportAudit(ctx context.Context, after int64, w io.Writer) error {
	if c.APIKey == "" {
		return ErrNoAPIKey
	}
	q := url.Values{"after": {strconv.FormatInt(after, 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/audit-events/export?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	// An export can be long, so only the context bounds it, not the client's usual timeout.
	hc := &http.Client{}
	if c.HTTP != nil {
		hc = &http.Client{Transport: c.HTTP.Transport}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := &APIError{Status: resp.StatusCode, Title: http.StatusText(resp.StatusCode)}
		_ = json.Unmarshal(raw, apiErr)
		return apiErr
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// auditLine is one line of the export: the event's hashed fields plus its links.
type auditLine struct {
	ID         int64           `json:"id"`
	OrgID      string          `json:"org_id"`
	OccurredAt string          `json:"occurred_at"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id"`
	IP         string          `json:"ip"`
	UserAgent  string          `json:"user_agent"`
	Data       json.RawMessage `json:"data"`
	PrevHash   string          `json:"prev_hash"`
	Hash       string          `json:"hash"`
}

// canonicalAuditEvent is the exact structure the server hashes. Its field order is part of the chain format, so it
// must stay identical to the server's (the API's integration test verifies a real export with this code).
type canonicalAuditEvent struct {
	OrgID      string          `json:"org_id"`
	OccurredAt string          `json:"occurred_at"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	RequestID  string          `json:"request_id"`
	IP         string          `json:"ip"`
	UserAgent  string          `json:"user_agent"`
	Data       json.RawMessage `json:"data"`
}

// AuditFileVerification is the result of checking an exported audit log offline.
type AuditFileVerification struct {
	Checked int `json:"checked"`
	// FromGenesis is true when the first event links to the org's genesis hash, so the file is the whole log.
	FromGenesis bool   `json:"from_genesis"`
	FirstID     int64  `json:"first_id"`
	LastID      int64  `json:"last_id"`
	LastHash    string `json:"last_hash"`
}

// errAuditFileBroken is wrapped by every verification failure.
var errAuditFileBroken = errors.New("audit export does not verify")

// VerifyAuditExport recomputes the hash chain of an NDJSON audit export without the server: every event must hash to
// its stored hash and link to the line before it. A file that starts mid-log can't check its first link; one that
// starts at the beginning reports FromGenesis.
func VerifyAuditExport(r io.Reader) (AuditFileVerification, error) {
	var res AuditFileVerification
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var prevHash, org string
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e auditLine
		if err := json.Unmarshal(line, &e); err != nil {
			return res, fmt.Errorf("%w: line %d is not an audit event: %w", errAuditFileBroken, n, err)
		}
		switch {
		case res.Checked == 0:
			org = e.OrgID
			res.FirstID = e.ID
			res.FromGenesis = e.PrevHash == auditGenesis(e.OrgID)
		case e.OrgID != org:
			return res, fmt.Errorf("%w: line %d (event %d) belongs to org %s, not %s", errAuditFileBroken, n, e.ID, e.OrgID, org)
		case e.ID <= res.LastID:
			return res, fmt.Errorf("%w: line %d: event %d comes after event %d", errAuditFileBroken, n, e.ID, res.LastID)
		case e.PrevHash != prevHash:
			return res, fmt.Errorf("%w: event %d doesn't link to event %d (a missing, edited or reordered line)", errAuditFileBroken, e.ID, res.LastID)
		}
		sum, err := auditEventHash(e)
		if err != nil {
			return res, fmt.Errorf("%w: event %d: %w", errAuditFileBroken, e.ID, err)
		}
		if sum != e.Hash {
			return res, fmt.Errorf("%w: event %d content doesn't match its hash", errAuditFileBroken, e.ID)
		}
		prevHash = e.Hash
		res.Checked++
		res.LastID, res.LastHash = e.ID, e.Hash
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("read export: %w", err)
	}
	if res.Checked == 0 {
		return res, fmt.Errorf("%w: no events", errAuditFileBroken)
	}
	return res, nil
}

func auditGenesis(org string) string {
	sum := sha256.Sum256([]byte("headmesh-audit-genesis:" + org))
	return hex.EncodeToString(sum[:])
}

func auditEventHash(e auditLine) (string, error) {
	prev, err := hex.DecodeString(e.PrevHash)
	if err != nil || len(prev) != sha256.Size {
		return "", errors.New("prev_hash is not a hex SHA-256")
	}
	// Data is hashed with sorted keys and its numbers exactly as written, the way the server canonicalises it.
	raw := []byte(e.Data)
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("data: %w", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("data: %w", err)
	}
	body, err := json.Marshal(canonicalAuditEvent{
		OrgID: e.OrgID, OccurredAt: e.OccurredAt, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, RequestID: e.RequestID, IP: e.IP, UserAgent: e.UserAgent, Data: data,
	})
	if err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}
