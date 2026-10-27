# Sandbox container contract

This page fixes the interface between the image (`sandbox/image/`), the Compose
files (`sandbox/compose/`), the host CLI (`headmesh-sandbox`) and the daemon
(`headmesh-sandboxd`). Change it here first, then in the code.

## Users

| User | UID/GID | Purpose |
|---|---|---|
| `agent` | build args `AGENT_UID`/`AGENT_GID` (default 1000/1000) | Every agent-facing process. Matches the host user that owns the project folders. No sudo. |
| `hmtunnel` | 990/990 (system) | Runs `tunnel-client` only, so the OpenAI runtime key is unreadable by `agent`. A fixed system UID can't collide with a custom `AGENT_UID`. |
| `root` | 0 | `tailscaled`, `tailscale-up`, `secrets-init`, `claude-runner` (sessions drop to `agent` through `claude-session`), `yolo-ttl`. |

The `ubuntu` user that ships with `ubuntu:26.04` (UID 1000) is removed.

## Paths inside the container

| Container path | Host source (Regular) | Mode | Notes |
|---|---|---|---|
| `/etc/headmesh/config/` | `${root}/config/` | ro | `sandbox.toml`, `tokens.json` (argon2id hashes only) |
| `/home/agent` | `${root}/home` | rw | Persistent agent home (`~/.claude`, `~/.codex`, history) |
| `/var/lib/tailscale` | `${root}/state/tailscale-<mode>` | rw (root) | Node identity, one per mode |
| `/var/lib/headmesh/sessions` | `${root}/sessions` | rw | Session/process metadata, logs, exec output, agent registry |
| `/var/lib/headmesh/artifacts` | `${root}/artifacts` | rw | `artifact_*` store, acceptance results |
| `/var/log/headmesh` | `${root}/logs` | rw | `s6-log` dirs per service, `audit.jsonl` |
| `/run/headmesh` | `${root}/run` | rw | `sandboxd.sock` (host CLI), `dns-check.json`, `banner` |
| `/srv/runner` | `${root}/runner` | rw | `claude self-hosted-runner --base-dir` |
| `/srv/runner-work` | `${root}/runner-work` | rw | Fresh clones for repos not mapped in `[repos]` |
| `/cache/{mise,uv,npm,pnpm,go,cargo}` | named volumes `hm-cache-*` | rw | Package caches |
| project folders | same path as on the host | rw/ro | From `sandbox.toml [mounts]` |
| `/host/<X>` | each `/<X>` (YOLO only), one bind per subtree, never one `/` bind | ro, `bind.recursive: readonly`, `bind.propagation: rprivate` | Skips `proc sys dev run tmp` and the Docker data-root (`/mnt/storage/docker`): directories containing it are bound child by child. Denylisted secrets masked on top. Never `rshared`. See spike T-7 |

Image-owned paths:

| Path | Content |
|---|---|
| `/usr/local/bin/headmesh-sandboxd` | Daemon and in-container helper (see below) |
| `/usr/local/bin/hm` | Symlink to `headmesh-sandboxd` |
| `/usr/local/bin/headmesh-sandbox` | Host CLI, shipped so `headmesh-sandbox acceptance run` works inside |
| `/usr/local/bin/claude-session` | Runner `--exec-path` wrapper: `setpriv` to `agent`, scrubs runner secrets from the environment |
| `/etc/headmesh/runner-hooks/{checkout,post-session}` | `exec headmesh-sandboxd runner-hook <name>` |
| `/etc/headmesh/git-hooks/{pre-commit,commit-msg,pre-push}` | Global hooks (`core.hooksPath`), chain to the repo's own hook |
| `/etc/headmesh/agent-guidelines.md.tmpl` | Guidelines template rendered by the `guidelines` oneshot |
| `/etc/headmesh/tmux.conf` | `exit-empty off`, `remain-on-exit on`, big history |
| `/etc/profile.d/headmesh.sh` | mise shims on `PATH`, `[YOLO]` prompt, banner |
| `/run/hm-secrets/` | Per-user copies of secrets made by `secrets-init` (root 0711) |

## Secrets

Compose secrets are **environment-sourced** (`secrets.<name>.environment`). The
host CLI reads `${root}/secrets/<name>` and passes the value only to the
`docker compose` process. Compose then copies each secret into the container as
`/run/secrets/<name>`, `root:root 0400`. File-sourced secrets are bind mounts that
keep host ownership (UID 1000 = `agent`) and ignore `uid/gid/mode`, so `agent` could
read them (spike T-1).

| Secret | Consumer | Copy made by `secrets-init` |
|---|---|---|
| `ts_authkey` | `tailscale-up` (root) | none |
| `claude_env_secret` | `claude-runner` (root) | none; `agent` can't read it |
| `github_token` | git credential helper, `GH_TOKEN` (agent) | `/run/hm-secrets/github_token` `agent 0400` |
| `openai_tunnel_key` | `tunnel-client` (hmtunnel) | `/run/hm-secrets/openai_tunnel_key` `hmtunnel 0400` |
| `github_ssh_key` | git over SSH, YOLO only (`core.sshCommand`) | `/run/hm-secrets/github_ssh_key` `agent 0400` |

An empty secret means "front door not configured"; the service then idles and
reports `disabled` in status.

Ubuntu 26.04 ships uutils coreutils, whose `install -o` chowns before it chmods; without
`CAP_FOWNER` that fails, so the scripts copy, chmod, then chown (`hm_put_file`).

## Environment (set by the generated Compose override)

| Variable | Example | Used by |
|---|---|---|
| `HEADMESH_MODE` | `regular` / `yolo` | everything |
| `HEADMESH_YOLO_EXPIRES_AT` | RFC 3339 | `yolo-ttl`, status, banner |
| `HEADMESH_ENTRYPOINTS` | `claude-runner,claude-rc,openai-tunnel` | s6 run scripts (absent = idle) |
| `HEADMESH_GPU` | `off` / `shared` | status |
| `HEADMESH_REPO_LOCK` | `off` / `advisory` / `enforce` | `pre-commit` hook |
| `HEADMESH_SANDBOX_CONFIG` | `/etc/headmesh/config/sandbox.toml` | `headmesh-sandboxd` |
| `TS_LOGIN_SERVER` | `https://hs.xcelsior.ca` | `tailscale-up` |
| `TS_HOSTNAME` | `asus-sandbox` | `tailscale-up`, runner `--client-label`, RC `--name` |
| `TS_TAGS` | `tag:sandbox` | `tailscale-up` only **checks** the enrolled tags against it (state `degraded` on mismatch); the tagged pre-auth key sets them, `--advertise-tags` is never passed (spike T-8) |
| `TZ` | `America/Toronto` | |
| `HEADMESH_GIT_USER_NAME`, `HEADMESH_GIT_USER_EMAIL` | from `[git]` | `secrets-init` → `/run/hm-git/identity.gitconfig` |
| `HEADMESH_OPENAI_TUNNEL_ID` | `tunnel_…` | `openai-tunnel` (empty = idle) |
| `HEADMESH_RUNNER_CAPACITY` | `1` (default) | `claude-runner --capacity` |
| `HEADMESH_RC_CAPACITY`, `HEADMESH_RC_DIR` | `4`, `/home/agent` (defaults) | `claude-rc` |

Never set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `DISABLE_TELEMETRY`, `DO_NOT_TRACK`
or `DISABLE_GROWTHBOOK` in the sandbox: Claude Code 2.1.293 refuses to start Remote
Control when any of them is set (spike T-2). The image sets only `DISABLE_AUTOUPDATER=1`.

## `headmesh-sandboxd` subcommands

```
headmesh-sandboxd serve      [--config PATH] [--listen :8780] [--unix /run/headmesh/sandboxd.sock]
headmesh-sandboxd health     [--unix PATH] [--timeout 5s]          # exit 0 healthy, 1 not
headmesh-sandboxd status     [--json] [--unix PATH]                # also `hm status`
headmesh-sandboxd dns-check  [--config PATH] [--out /run/headmesh/dns-check.json]
                             # exit 0 unless --strict; result feeds health
headmesh-sandboxd guidelines render [--config PATH] [--template PATH] [--home /home/agent]
                             [--banner-out /run/headmesh/banner]
headmesh-sandboxd yolo-ttl   [--expires-at RFC3339] [--halt /run/s6/basedir/bin/halt]
headmesh-sandboxd runner-hook checkout|post-session               # reads CLAUDE_RUNNER_* env
headmesh-sandboxd agents     register|heartbeat|unregister|list ...
headmesh-sandboxd version
```

Defaults: `--config` falls back to `$HEADMESH_SANDBOX_CONFIG`, then
`/etc/headmesh/config/sandbox.toml`; `--unix` to `/run/headmesh/sandboxd.sock`.

## s6-rc services

| Service | Type | User | Depends on |
|---|---|---|---|
| `secrets-init` | oneshot | root | — |
| `tailscaled` | longrun | root | `secrets-init` |
| `tailscale-up` | oneshot | root | `tailscaled`, `secrets-init` |
| `dns-check` | oneshot | agent | `tailscale-up` |
| `guidelines` | oneshot | agent | `secrets-init` |
| `tmux` | longrun | agent | `secrets-init` |
| `sandboxd` | longrun | agent | `tmux`, `dns-check`, `guidelines` |
| `claude-runner` | longrun | root → `agent` sessions | `sandboxd`, `secrets-init` |
| `claude-rc` | longrun | agent | `tmux`, `sandboxd` |
| `openai-tunnel` | longrun | hmtunnel | `sandboxd`, `secrets-init` |
| `yolo-ttl` | longrun | root | `secrets-init` |

Every service also depends on s6-overlay's `base`. Service definitions live in
`/etc/s6-overlay/s6-rc.d/`; the `user` bundle is in
`/etc/s6-overlay/user-bundles.d/user/contents.d/` (s6-overlay 3.2.3.2 deprecates
defining it under `s6-rc.d`). `tailscaled` and `sandboxd` signal readiness through
`s6-notifyoncheck` (`tailscale status --json` answers; `headmesh-sandboxd health`
exits 0), so dependants wait for them. `claude-runner` has `timeout-kill` 80000
(drain), and its `finish` + `runner-backoff` back off 5 s → 300 s only after fast
failures. A oneshot that exits non-zero stops the container
(`S6_BEHAVIOUR_IF_STAGE2_FAILS=2`), so `tailscale-up`, `dns-check` and `guidelines`
always exit 0 and report problems through state files instead.

Each longrun has an `s6-log` consumer writing to `/var/log/headmesh/<svc>/`
(`n10 s10000000`). Settings: `S6_BEHAVIOUR_IF_STAGE2_FAILS=2`, `S6_KILL_GRACETIME=3000`,
`S6_CMD_WAIT_FOR_SERVICES_MAXTIME=120000`, `S6_KEEP_ENV=1`. The runner's 80 s drain
comes from its own `timeout-kill` (s6-overlay's shutdown runs `s6-rc -bda change` with no
overall timeout, so it waits for it; Compose's `stop_grace_period: 90s` covers it); `S6_KILL_GRACETIME` is the final wait before
SIGKILL for leftovers, and `s6-linux-init-shutdownd` sleeps it in full, so 80000 there
made every stop take 80 s (measured). `tailscaled` runs with `--no-logs-no-support`
and the image sets `TS_NO_LOGS_NO_SUPPORT=true`: nothing is uploaded to
`log.tailscale.com`.

## Runtime state written by the image

| Path | Writer | Content |
|---|---|---|
| `/run/hm-state/<service>.json` | s6 run scripts | `{"service","state","reason","ts"}`; `state` is one of `running`, `starting`, `restarting`, `disabled`, `waiting`, `backoff`, `not-enrolled`, `needs-reauth`, `degraded`, `error`. sandboxd's status aggregator reads these for the entry points. |
| `/run/hm-state/runtime.env` | `secrets-init` | `export VAR=…` for `HEADMESH_MODE`, `HEADMESH_YOLO_EXPIRES_AT`, `HEADMESH_ENTRYPOINTS`, `HEADMESH_GPU`, `HEADMESH_REPO_LOCK`, `TS_HOSTNAME`, `TZ`; sourced by `/etc/profile.d/headmesh.sh` because Tailscale SSH logins don't get the container environment |
| `/run/hm-state/mode-label` | `secrets-init` | `[YOLO] ` or empty (tmux status line) |
| `/run/hm-git/{identity,mode}.gitconfig` | `secrets-init` | included from `/etc/gitconfig` |
| `/var/lib/tailscale/.headmesh-enrolled` | `tailscale-up` | auth-once marker: once present, a logged-out node is reported `needs-reauth` and never re-enrolled automatically |
| `/run/hm-tunnel/health.url` | `tunnel-client` | base URL of the tunnel's health server (`127.0.0.1:8082`) |

## Guidelines template data

`headmesh-sandboxd guidelines render` executes `/etc/headmesh/agent-guidelines.md.tmpl`
with Go `text/template` and this data:

| Field | Type | Meaning |
|---|---|---|
| `.Mode` | string | `regular` or `yolo` |
| `.YOLO` | bool | `.Mode == "yolo"` |
| `.ExpiresAt` | string | RFC 3339 TTL end; empty in Regular |
| `.Hostname` | string | `TS_HOSTNAME` |
| `.RepoLock` | string | `off`, `advisory` or `enforce` |
| `.Repos` | `[]struct{Name, Path string}` | `[repos]` mapping |
| `.Writable`, `.ReadOnly` | `[]string` | container paths of rw / ro mounts |

## Repository lock file (`repo_lock = "enforce"`)

`agents_register` writes `<git common dir>/headmesh-agent.lock` and heartbeats refresh
it. The `pre-commit` hook reads it with `jq`:

```json
{"agent_id": "agt_…", "agent": "claude-runner", "task": "…", "heartbeat_unix": 1793080000}
```

A commit is refused when the lock's `agent_id` differs from the committer's
`$HEADMESH_AGENT_ID` and `heartbeat_unix` is less than 120 s old.

## Build

The image's Go stage runs `go build -trimpath -ldflags="-s -w -buildid= -X main.version=$VERSION"`
for `./cmd/headmesh-sandboxd` and `./cmd/headmesh-sandbox` with `golang:1.26.8`,
`CGO_ENABLED=0`, `GOWORK=off`, `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`. Declare
`var version = "dev"` in both `main` packages to get the version stamped.

## Network

- Compose network `hm-sbx`: Linux bridge **`br-hm-sbx0`**, subnet `172.31.250.0/24`,
  gateway `.1`, `enable_icc: "false"`. Fixed addresses: Regular `172.31.250.10`,
  YOLO `172.31.250.11` (`ipv4_address`), so the host firewall can apply
  `yolo.allow_lan` to the YOLO address only, without a reload on mode switch.
  The bridge name must start with `br`: on the ASUS the persisted `DOCKER-USER`
  chain returns only for `lo`, `tailscale0`, `docker0` and `br+` and drops
  every other bridge (no egress at all for `hm-sbx0`).
- Host egress firewall: nftables table `inet headmesh_sbx` (`sandbox/host/firewall/`),
  config `/etc/headmesh/sandbox-firewall.conf` mirrors the values above.
  Status for the CLI without root: `/run/headmesh-sandbox-firewall.json`.
- Backup status for the CLI without root: `${root}/state/backup-status.json`
  (`last_success_at`, written by `headmesh-home-backup`).
- sandboxd listens on `:8780` (tailnet and the `hm-sbx` bridge) and on the unix socket.
- The runner's health endpoint is `127.0.0.1:8081/healthz`; the tunnel client's is
  `127.0.0.1:8082` (`/healthz`, `/readyz`).
- tmux socket: `tmux -L headmesh` (UID 1000). The tmux session is `headmesh`; sandboxd
  sessions are windows named `s-<id>`; Remote Control is the window `rc`.
- No published ports.
