# Spike T-16: `codex exec-server` as non-root inside the sandbox

- Status: answered (2026-10-27): the executor runs as `agent`; Codex's own command sandbox does not, so sessions must use `danger-full-access` and rely on the container
- Plan: §7.6, §17 T-16 (fallback: a dedicated child container with the same mounts)
- Environment: sandbox image, `@openai/codex@0.162.1`, user `agent`, plan §6.3 options
  (`cap_drop: ALL` + the listed adds, `no-new-privileges`, Docker's default seccomp and
  AppArmor `docker-default` profiles); host has `kernel.apparmor_restrict_unprivileged_userns=1`

## Question

Can `codex exec-server` run as `agent` in the hardened container, and does Codex's
Linux sandbox work there (is `--linux-sandbox-pid-namespace inherit` enough)?

## Evidence

| Test (as `agent`) | Result |
|---|---|
| `codex exec-server --listen ws://127.0.0.1:47011 --linux-sandbox-pid-namespace inherit` | Starts, prints the URL, listens (`ss` shows `codex` on 47011). Runs fine as non-root. |
| `codex sandbox -- id` (default policy) | `bwrap: No permissions to create a new namespace` |
| `unshare -U -r id` | `Operation not permitted` (Docker's default seccomp profile blocks `CLONE_NEWUSER` without `CAP_SYS_ADMIN`) |
| `codex sandbox --enable use_legacy_landlock -- id` | Panics: `filesystem-restricted execution requires bubblewrap to isolate app-server sockets` |
| `codex features list` | `use_linux_sandbox_bwrap` removed, `use_legacy_landlock` deprecated: bubblewrap (bundled at `@openai/codex-linux-x64/vendor/…/codex-resources/bwrap`) is the only Linux sandbox |
| Throwaway container with `seccomp=unconfined` (still `no-new-privileges`, caps dropped) | `unshare -U` works, but bwrap fails: `Failed to make / slave: Permission denied` (AppArmor `docker-default` denies the mount) |

`--linux-sandbox-pid-namespace inherit` only skips the PID namespace; bubblewrap still
needs a user namespace and mount operations, which the container's seccomp and
AppArmor profiles forbid.

## Outcome

- The executor itself works as `agent` under the default container hardening; no
  privileges are needed to start it. The orchestrator (`headmesh agent run --provider
  openai`) runs `docker exec -u agent headmesh-sandbox codex exec-server --remote … --environment-id …`
  as planned, adding `--linux-sandbox-pid-namespace inherit` so a later bubblewrap-capable
  setup doesn't fail on the PID namespace.
- Commands the session runs must not ask Codex for a filesystem-restricted sandbox:
  configure the session's sandbox policy as `danger-full-access` (and approvals
  `never` for unattended runs). The trust boundary is then the container, the same as
  for Codex over SSH and sandboxd `exec`: uid 1000, no capabilities, no
  `docker.sock`, egress firewall, own tailnet identity.
- Rejected: relaxing seccomp and AppArmor for the sandbox (or for a per-session child
  container, the plan's fallback). It would weaken the boundary that matters most to
  gain a second, inner one, and a custom AppArmor profile needs root on the host.
  Revisit if Codex ships a Landlock-only mode again.

## Contradiction with plan §1

Plan §1 says `codex exec-server` is documented only as the Agents API per-session
sandbox and is "not a front door for Codex Cloud tasks". On this host the ChatGPT
desktop app (Codex) runs
`codex -c forced_login_method="chatgpt" exec-server --remote https://codex-cloud-environments.chatgpt.com/api --environment-id ccarenv_<redacted>`
(observed 2026-10-26). So exec-server also registers this machine as a **Codex cloud
environment** with a ChatGPT login, and `codex exec-server forward --remote … --environment-id … --connect ws://…`
exists to register an already-running exec-server. That looks like a way to run
chatgpt.com Codex tasks inside the sandbox, which §7.6 says OpenAI doesn't offer.
It is not documented and wasn't tested here (it needs Aaryn's ChatGPT login and an
environment). Recommended follow-up: Aaryn creates a Codex cloud environment of type
"remote/self-hosted" in ChatGPT, then try `docker exec -u agent headmesh-sandbox codex
exec-server --remote https://codex-cloud-environments.chatgpt.com/api --environment-id <id>`
after `codex login` inside the sandbox; if it works, add it as a front door in §7.6.
