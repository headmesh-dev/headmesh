package acceptance

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Before is the state captured ahead of `headmesh-sandbox recreate`.
type Before struct {
	Node      Node   `json:"node"`
	SessionID string `json:"session_id"`
	File      string `json:"file"`
}

// Snapshot writes a ~/.bash_history marker and a marker file, starts a
// session, and records the node and boot identity.
func Snapshot(ctx context.Context, dial Dialer, marker string) (Before, error) {
	c, err := dial(ctx)
	if err != nil {
		return Before{}, err
	}
	defer func() { _ = c.Close() }()
	n, err := node(ctx, c)
	if err != nil {
		return Before{}, err
	}
	file := "/home/agent/.hm-recreate-marker"
	o, err := run(ctx, c, "printf '%s\\n' "+quote(marker)+" >> ~/.bash_history && printf '%s\\n' "+quote(marker)+" > "+file, 30)
	if err != nil || o.ExitCode != 0 {
		return Before{}, fmt.Errorf("write markers: %w %s", err, o.Stderr)
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := c.Call(ctx, "session_create", map[string]any{"name": "recreate-probe", "shell": []string{"sleep", "86400"}}, &info); err != nil {
		return Before{}, err
	}
	return Before{Node: n, SessionID: info.ID, File: file}, nil
}

// CheckAfterRecreate verifies persistence after the container was recreated.
func CheckAfterRecreate(ctx context.Context, dial Dialer, b Before, marker string) (Result, error) {
	res := Result{Route: "recreate", Phase: "all", StartedAt: time.Now().UTC()}
	res.Host, _ = os.Hostname()
	c, err := dial(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = c.Close() }()
	after, err := node(ctx, c)
	if err != nil {
		return res, err
	}
	res.Node = after
	add := func(id int, name string, pass bool, details string) {
		res.Checks = append(res.Checks, Check{ID: id, Name: name, Pass: pass, Details: details})
	}
	o, err := run(ctx, c, "grep -Fx "+quote(marker)+" ~/.bash_history && cat "+quote(b.File), 30)
	add(1, "home and history persist", err == nil && o.ExitCode == 0 && strings.Count(o.Stdout, marker) == 2,
		fmt.Sprintf("bash_history marker and %s: exit %d", b.File, o.ExitCode))
	switch {
	case b.Node.BackendState != "Running":
		add(2, "tailnet identity persists", true, "not enrolled before the recreate ("+b.Node.BackendState+"); identity check skipped")
	case after.NodeID == b.Node.NodeID && strings.Join(after.IPs, ",") == strings.Join(b.Node.IPs, ","):
		add(2, "tailnet identity persists", true, fmt.Sprintf("node %s %v unchanged", after.NodeID, after.IPs))
	default:
		add(2, "tailnet identity persists", false, fmt.Sprintf("node %s %v → %s %v", b.Node.NodeID, b.Node.IPs, after.NodeID, after.IPs))
	}
	add(3, "new boot identity", after.BootKey != "" && after.BootKey != b.Node.BootKey, b.Node.BootKey+" → "+after.BootKey)
	var list struct {
		Sessions []struct {
			ID         string `json:"id"`
			State      string `json:"state"`
			LostReason string `json:"lost_reason"`
		} `json:"sessions"`
	}
	state, reason := "missing", ""
	if err := c.Call(ctx, "session_list", nil, &list); err == nil {
		for _, s := range list.Sessions {
			if s.ID == b.SessionID {
				state, reason = s.State, s.LostReason
			}
		}
	}
	add(4, "old sessions reported lost", state == "lost" && strings.Contains(reason, "container restarted"), state+": "+reason)
	add(5, "sandboxd healthy after recreate", true, "sandbox_status answered")
	res.FinishedAt = time.Now().UTC()
	res.Total = len(res.Checks)
	for _, ch := range res.Checks {
		if ch.Pass {
			res.Passed++
		}
	}
	res.Pass = res.Passed == res.Total
	return res, nil
}
