# Spike T-1: keep the runner's environment secret away from sessions

- Status: mechanism verified locally (2026-10-27); end-to-end check with a real runner pending Aaryn's Team enablement
- Plan: §7.1 (runner as root, `--exec-path` → `setpriv` wrapper), §14 "Runner secret", §17 T-1
- Environment: ASUS, Ubuntu 26.04.1, kernel 7.0.0-38, Docker 29.1.3, Compose 2.40.3, sandbox image built from `sandbox/image/Dockerfile`

## Question

`claude self-hosted-runner` documents that session code can read the environment
secret. If the runner runs as root and spawns every session through a wrapper that
drops to `agent`, can a session still get at the secret: the file, the runner's
environment or memory, an inherited descriptor, or by regaining root?

## Finding 1: how the secret gets into the container decides everything

The host user and `agent` are both UID 1000, so any host file Aaryn can read is
readable by `agent` once mounted.

| Compose secret source | What the container sees |
|---|---|
| `file:` with `uid: "0"`, `gid: "0"`, `mode: 0400` | A bind mount of the host file with **host ownership** `1000:1000 0600`; `uid/gid/mode` are ignored. `agent` reads it. |
| `environment:` with the same `uid/gid/mode` | Compose copies the value into the container as `/run/secrets/<name>` **`root:root 0400`**, and the variable does not appear in the container environment. `agent` gets `Permission denied`. |

Decision: every Compose secret is environment-sourced. The host CLI reads
`${root}/secrets/<name>` and passes the values only in the environment of the
`docker compose` process (contract, "Secrets"). Side effect: the copy lives in the
container's writable layer until the container is recreated, and a read-only root
filesystem can't receive it (relevant to T-8).

## Finding 2: the wrapper holds

Test: `sandbox/image` with the plan §6.3 options (`cap_drop: ALL` plus NET_ADMIN,
NET_RAW, SETUID, SETGID, CHOWN, DAC_OVERRIDE, KILL; `no-new-privileges`), started by
Compose with env-sourced fake secrets. A root "runner" opened the secret on fd 7,
exported `SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET` with its value, then ran
`/usr/local/bin/claude-session --print hello` with `HEADMESH_CLAUDE_BIN` pointing at a
probe script.

| Probe (as the session) | Result |
|---|---|
| identity | `uid=1000 gid=1000 groups=agent`, cwd kept (`/srv/runner`), argv forwarded |
| capabilities | `CapInh/CapPrm/CapEff/CapAmb = 0`, `NoNewPrivs: 1` |
| `cat /run/secrets/claude_env_secret` | `Permission denied` |
| `/proc/<runner>/environ` | `Permission denied` |
| `/proc/<runner>/mem` | `Permission denied` |
| own environment | no `SELF_HOSTED_RUNNER_*`, no secret value |
| inherited fd 7 (the secret) | closed by the wrapper |
| `setpriv --reuid=0` | `setresuid failed: Operation not permitted` |
| `su root` | `Authentication failure` (no setuid binaries in the image) |
| `/run/hm-secrets/github_token` | readable (intended: git needs it) |
| `/run/hm-secrets/openai_tunnel_key` | not readable (owned by `hmtunnel`) |
| `HOME`, attribution | `/home/agent`, `HEADMESH_ENTRYPOINT=claude-runner`, `HEADMESH_SESSION=<runner session id>` |

Visible, by design: the runner's command line (`/proc/<pid>/cmdline`) shows the secret
**path**, never the value. `CapBnd` keeps the container's set (`0x30e3`) because
dropping the bounding set needs `CAP_SETPCAP`; with `no-new-privileges`, a non-zero UID
and no setuid or file-capability binaries, nothing can raise it.

## Not testable yet

These need the Team "self-hosted environments" toggle and a real environment secret:

- which environment variables the runner passes to the child (the wrapper strips
  `SELF_HOSTED_RUNNER_*` whatever they are);
- whether the child needs root-created files besides `CLAUDE_RUNNER_WORK_ORDER_FILE`
  (the wrapper chowns that one), e.g. the host-config snapshot under `--base-dir`
  or workspace-trust seeding, which the runner would write to root's `~/.claude.json`
  rather than `agent`'s;
- the checkout hook path end to end (it runs as root; `headmesh-sandboxd runner-hook
  checkout` must create or link the checkout as `agent`).

Run when enabled: `docker exec headmesh-sandbox tail -f /var/log/headmesh/claude-runner/current`,
dispatch one task, then inside the session run `cat /run/secrets/claude_env_secret`,
`tr '\0' '\n' </proc/$(pgrep -f self-hosted-runner)/environ` and `env | grep -i runner`;
all three must fail or show nothing.

## Outcome

Preferred design adopted: runner as root, secret at `root 0400` (env-sourced Compose
secret), sessions through `/usr/local/bin/claude-session`
(`setpriv --reuid --regid --init-groups --inh-caps=-all`, environment scrub, secret-fd
close). Fallback (accept the exposure, rotate every 30 days, alert on unknown runner
registrations) stays documented for the case the live check above fails.
