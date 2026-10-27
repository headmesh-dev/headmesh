# Spike T-2: Claude Remote Control under a supervisor, without a TTY

- Status: partly answered (2026-10-27); the restart check needs Aaryn's claude.ai login
- Plan: §7.2, §17 T-2 (fallback: tmux window re-armed by the s6 wrapper)
- Environment: sandbox image, Claude Code 2.1.293 native binary, user `agent`, no credentials

## Question

Once the login, workspace trust and "Enable Remote Control?" answer are persisted in
the agent home, does `claude remote-control` restart cleanly without a TTY, so s6 can
supervise it directly?

## What was tested (no credentials; the host's `~/.claude` was never used)

| Test | Result |
|---|---|
| `claude remote-control --help`, no TTY, not logged in | Exit 1: `You must be logged in to use Remote Control … Run claude auth login`. There is no help screen: the subcommand starts Remote Control straight away. |
| `claude remote-control --name t2 --spawn same-dir --capacity 1 </dev/null` | Same message, exit 1, immediately (no hang, no prompt). |
| Same command in a tmux pane (TTY) | Same message, exit 1. |

From the 2.1.293 binary (strings around the Remote Control entry point):

- The first-run question is a readline prompt, `Enable Remote Control? (y/n)`, on stdin.
  The answer is persisted as `remoteDialogSeen` in `~/.claude.json`, so later starts
  skip it.
- Remote Control needs a **full-scope** claude.ai login. Long-lived tokens
  (`claude setup-token`, `CLAUDE_CODE_OAUTH_TOKEN`) are rejected as inference-only.
- It refuses to start when feature-flag evaluation is off: if
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_TELEMETRY`, `DO_NOT_TRACK` or
  `DISABLE_GROWTHBOOK` is set. The image sets none of them (only
  `DISABLE_AUTOUPDATER=1`); the contract now forbids them.
- `claude daemon … remote-control add` exists, but in this version "Service install is
  disabled — the daemon runs on demand and exits when the last client disconnects",
  so it is no replacement for a supervisor.

Side note: running `claude remote-control --help` on the host while logged in starts
a real Remote Control server (that is why the discovery command hung). Don't use it
to read flags.

## Outcome

Keep the plan's fallback as the design; it covers both answers to the open question:

- The s6 service `claude-rc` runs `/etc/s6-overlay/scripts/claude-rc-loop` as `agent`.
  The loop waits (state `waiting`) until `~/.claude/.credentials.json` exists, then
  keeps `claude remote-control --name $TS_HOSTNAME --spawn same-dir --capacity 4`
  alive in the tmux window `headmesh:rc`, respawning the pane when it exits (for
  example after ~10 minutes offline).
- The TTY comes from tmux, so it doesn't matter whether Remote Control strictly needs
  one after the first run.

## First run (Aaryn)

```
headmesh sandbox shell        # as agent, in /home/agent
claude auth login             # full claude.ai login, not setup-token
claude remote-control --name asus-sandbox --spawn same-dir --capacity 4
#   trust /home/agent, answer "y" to "Enable Remote Control?", then Ctrl-C
```

The `claude-rc` service picks it up within 60 s. To close the spike, after that run:
`docker exec -u agent headmesh-sandbox tmux -L headmesh kill-window -t headmesh:rc`
and confirm the window comes back and connects with no prompt
(`tmux -L headmesh capture-pane -p -t headmesh:rc`); also `docker restart
headmesh-sandbox` and check the same. If it connects without the tmux TTY too
(`claude remote-control … </dev/null >log`), the loop can be simplified later.
