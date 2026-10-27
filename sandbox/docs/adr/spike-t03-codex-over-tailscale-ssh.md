# Spike T-3: Codex "Remote connections" over Tailscale SSH into the sandbox

- Status: server side verified locally (2026-10-27); the Codex desktop client on
  Aaryn's Mac is **untested** (steps below). Fallback not needed so far.
- Plan: §7.3, §6.6, §17 T-3
- Environment: ASUS, Docker 29.1.3, Tailscale 1.104.2, Headscale v0.29.6 (throwaway)

## Question

Does an SSH session over Tailscale SSH terminate inside the sandbox as user
`agent`, with the §6.3 capability set and `no-new-privileges`, and does a login
shell find `codex` and `claude` through `/etc/profile.d`? What does Tailscale
SSH need?

## Test

Same throwaway tailnet as T-8 (private Docker network, Headscale v0.29.6,
`mesh.test`, nothing touches hs.xcelsior.ca). Policy:

```json
{ "tagOwners": { "tag:sandbox": ["spike@"] },
  "acls": [ { "action": "accept", "src": ["*"], "dst": ["*:*"] } ],
  "ssh":  [ { "action": "accept", "src": ["spike@"], "dst": ["tag:sandbox"], "users": ["agent"] } ] }
```

- "laptop": a node owned by user `spike` (stands in for Aaryn's Mac), using
  the plain OpenSSH client (`ssh agent@<node>`), as Codex desktop would.
- "sandbox": first a minimal mirror (`ubuntu:26.04` + tailscale binaries, user
  `agent` 1000, `/etc/profile.d/headmesh.sh` adding `/opt/hm/bin` to `PATH`), then
  Fork A's real image `headmesh-sandbox:dev-stub` under s6, both with
  `cap_drop: ALL`, `cap_add` NET_ADMIN NET_RAW SETUID SETGID CHOWN DAC_OVERRIDE
  KILL, `no-new-privileges:true`, `/dev/net/tun`, tailscaled as root with
  `tailscale up … --ssh`.

## Evidence

| Check | Result |
|---|---|
| Non-PTY command `ssh agent@sandbox-rw 'id; …'` | `uid=1000(agent) gid=1000(agent)`. tailscaled log: `access granted to spike as ssh-user "agent"`, `audit: SSH login: user=agent uid=1000 from=100.64.0.1 ts_user=spike`. Parent process: `/usr/bin/su -w SSH_AUTH_SOCK -l agent -c <cmd>`, i.e. a **login shell**, so `/etc/profile.d` runs: `PATH=/opt/hm/bin:…`, `HEADMESH_PROFILE_LOADED=1`, `hm-hello` found. |
| `bash -lc` and `ssh -tt` with a command | Same `PATH`; PTY `/dev/pts/1`. |
| Real image, non-PTY | `codex` → `/usr/local/share/mise/bin/codex`, `codex-cli 0.162.1`; `claude` → `/usr/local/bin/claude`, `2.1.293 (Claude Code)`; `HEADMESH_MODE=regular`; PATH includes `/cache/mise/shims` and `~/.local/bin`. |
| Real image, PTY | `codex --version` → `codex-cli 0.162.1` on `/dev/pts/0`. |
| `ssh root@sandbox-rw` | Refused: `tailnet policy does not permit you to SSH as user "root"`. |
| stdin streaming | `echo … \| ssh agent@… 'cat > ~/upload.txt'` → 21 bytes written. |
| SFTP | `sftp agent@sandbox-rw` lists `/home/agent` (tailscaled serves SFTP itself; no `sftp-server` package needed). |
| Port forwarding | `-L 19999:127.0.0.1:9999` reached a listener inside the container; `-R … ExitOnForwardFailure=yes` accepted (exit 0). |
| First connection right after enrollment | One `Connection timed out`; the retry a few seconds later succeeded (peer path not yet established). |

What Tailscale SSH needs, from the logs and the setup that worked:
- tailscaled running as **root** in the container with `--ssh` (pref `RunSSH: true`).
- `CAP_SETUID`/`CAP_SETGID` to become `agent` (it runs `tailscaled be-child ssh
  --uid=1000 --gid=1000 …`, then `su -l`). `no-new-privileges` doesn't get in
  the way: the process is already root, and `su` needs no setuid elevation.
- `/usr/bin/su` (util-linux/login, present in the image) and a valid login
  shell for `agent` (`/bin/bash`). No OpenSSH server, no `/etc/ssh` host keys:
  the host key is tailscaled's and is published through the coordination
  server.
- A tailnet `ssh` rule naming `agent` in `users`.

Also see the T-8 note: tailscaled uploads logs to `log.tailscale.com` unless
started with `--no-logs-no-support` (seen in this image; Fork A is adding the flag).

## Outcome

Tailscale SSH terminates inside the container as `agent`, through a login shell,
with the planned capabilities and `no-new-privileges`. `codex` and `claude` are on
the login-shell `PATH`. No OpenSSH fallback (port 2222) is needed for the server
side.

## Untested: Codex desktop on the Mac (Aaryn)

Needs the M1 enrollment (§6.6) and the §5.2 policy (providers tagged, so
`xcelsior@` = Aaryn's devices only) with an `ssh` rule
`{action: accept, src: ["xcelsior@"], dst: ["tag:sandbox", "tag:sandbox-yolo"], users: ["agent"]}`.

1. On the Mac (on the tailnet): `ssh agent@asus-sandbox.mesh.xcelsior.ca 'codex --version'`
   should print `codex-cli 0.162.1`. If it hangs once right after enrollment, retry.
2. Codex desktop → Settings → Remote connections → add host
   `agent@asus-sandbox.mesh.xcelsior.ca` (it uses the Mac's ssh config and
   agent; Tailscale SSH ignores client keys, the tailnet identity authorizes).
3. Open `/mnt/storage/projects/xcelsior` remotely and run the acceptance prompt
   (`sandbox/tests/acceptance/prompt.md`).
4. Record in this ADR: connected yes/no, whether Codex needed SFTP or port
   forwarding (both work), and any error. If it fails in a way that is specific
   to Tailscale SSH, use the plan fallback: OpenSSH as UID 1000 on `:2222`, bound to
   the tailnet IP, keys only, `AllowUsers agent`, `PermitRootLogin no`.
