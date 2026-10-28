package status

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText prints a report for humans (`hm status`, `headmesh-sandbox status`).
func WriteText(w io.Writer, r Report) {
	mode := strings.ToUpper(r.Mode)
	if r.YOLOExpiresAt != nil {
		mode += fmt.Sprintf(" (halts %s, %s left)", r.YOLOExpiresAt.Local().Format("15:04 MST"), r.YOLORemaining)
	}
	health := "healthy"
	if !r.Healthy {
		health = fmt.Sprintf("%d problem(s)", len(r.Problems))
	}
	_, _ = fmt.Fprintf(w, "Sandbox %s · mode %s · %s\n", r.Name, mode, health)
	_, _ = fmt.Fprintf(w, "Container started %s (boot %s); sandboxd since %s\n",
		fmtTime(r.ContainerStartedAt), short(r.BootID), fmtTime(r.SandboxdStartedAt))

	t := r.Tailnet
	switch t.BackendState {
	case "Running":
		_, _ = fmt.Fprintf(w, "Tailnet: %s %s node %s, DERP %s, %d peers\n", t.DNSName, strings.Join(t.IPs, " "), t.NodeID, orDash(t.DERPRegion), t.Peers)
	default:
		_, _ = fmt.Fprintf(w, "Tailnet: %s %s\n", t.BackendState, t.Error)
	}
	if r.DNSCheck != nil {
		_, _ = fmt.Fprintf(w, "DNS check: %s — %s\n", r.DNSCheck.Status, r.DNSCheck.Message)
	}
	for _, rr := range r.Reach {
		_, _ = fmt.Fprintf(w, "  reach %s: %s\n", rr.Target, rr.Summary)
	}

	_, _ = fmt.Fprintln(w, "\nEntry points:")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, ep := range r.EntryPoints {
		state := ep.State
		if ep.Enabled && ep.Healthy {
			state += " ✓"
		}
		line := fmt.Sprintf("  %s\t%s\t%s", ep.Name, state, ep.Detail)
		if ep.Hint != "" && (!ep.Healthy || !ep.Enabled) {
			line += " — " + ep.Hint
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintf(w, "\nSessions: %d running, %d exited, %d lost · processes: %d running, %d exited, %d lost\n",
		r.Sessions.Running, r.Sessions.Exited, r.Sessions.Lost, r.Processes.Running, r.Processes.Exited, r.Processes.Lost)
	for i, s := range r.Tasks {
		if i == 10 {
			_, _ = fmt.Fprintf(w, "  … %d more\n", len(r.Tasks)-10)
			break
		}
		state := s.State
		if s.ExitCode != nil {
			state = fmt.Sprintf("exited(%d)", *s.ExitCode)
		}
		if s.LostReason != "" && s.State == "lost" {
			state = "lost (" + s.LostReason + ")"
		}
		_, _ = fmt.Fprintf(w, "  %-7s %s %-20s %s\n", s.Kind, s.ID, s.Name, state)
	}
	if len(r.Agents) > 0 {
		_, _ = fmt.Fprintf(w, "\nActive agents: %d\n", len(r.Agents))
		for _, a := range r.Agents {
			_, _ = fmt.Fprintf(w, "  %s in %s (task %q)\n", a.Agent, strings.Join(a.Repos, ", "), a.Task)
		}
	}
	if len(r.Repos) > 0 {
		_, _ = fmt.Fprintln(w, "\nRepos:")
		for _, g := range r.Repos {
			if g.Error != "" {
				_, _ = fmt.Fprintf(w, "  %s: %s\n", g.Repo, g.Error)
				continue
			}
			extra := ""
			if g.Progress != "" {
				extra = " · " + g.Progress + " in progress"
			}
			_, _ = fmt.Fprintf(w, "  %s: %s +%d -%d, %d dirty%s\n", g.Repo, g.Branch, g.Ahead, g.Behind, g.Dirty, extra)
		}
	}
	_, _ = fmt.Fprintf(w, "\nGPU: %s", r.GPU.Mode)
	for _, d := range r.GPU.Devices {
		_, _ = fmt.Fprintf(w, " · %s", d)
	}
	if r.GPU.Error != "" {
		_, _ = fmt.Fprintf(w, " (%s)", r.GPU.Error)
	}
	_, _ = fmt.Fprintln(w)
	for _, c := range r.Caches {
		_, _ = fmt.Fprintf(w, "Cache %s: %s used, %s free\n", c.Path, humanBytes(c.UsedBytes), humanBytes(int64(min(c.FreeBytes, 1<<62))))
	}
	if len(r.Problems) > 0 {
		_, _ = fmt.Fprintln(w, "\nProblems:")
		for _, p := range r.Problems {
			_, _ = fmt.Fprintln(w, "  - "+p)
		}
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return orDash(s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
