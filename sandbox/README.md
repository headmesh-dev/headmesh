# Headmesh agent sandbox

A persistent Docker container on the ASUS that joins Aaryn's Headscale tailnet
as its own tagged node, so cloud agents can work with real files, real
dependencies and private services. Agents reach it through several front doors:

- an MCP server, `headmesh-sandboxd`;
- the Claude self-hosted runner and Claude Remote Control;
- Codex over Tailscale SSH;
- the OpenAI Secure MCP Tunnel;
- `codex exec-server`, driven from the host through the Agents API.

It implements plan §6 and §7 (`docs/plan.md`).

| Path | What |
|---|---|
| `image/` | Multi-stage Dockerfile on `ubuntu:26.04` (pinned by digest), s6-overlay services, git hooks, guidelines template, `docker-bake.hcl`, container-structure-test, hadolint config |
| `compose/` | `compose.yaml` with the `regular` and `yolo` profiles (embedded in the CLI) |
| `config/` | `sandbox.example.toml` and its JSON Schema (`sandbox.schema.json`) |
| `sandboxd/` | The MCP daemon: tools, sessions (tmux), auth, audit, status |
| `cmd/headmesh-sandboxd/` | Daemon binary, plus the in-container helpers (`hm`) |
| `cmd/headmesh-sandbox/` | Host CLI |
| `host/` | Egress firewall and restic backup units. Aaryn installs them with sudo; see `host/README.md` |
| `tests/acceptance/` | The five-check harness, the recreate bonus and `prompt.md` |
| `tests/e2e/` | Live test against a running container |
| `docs/` | `container-contract.md` (the interface between image, Compose, CLI and daemon) and the ADRs |

This is its own Go module, `github.com/headmesh-dev/headmesh/sandbox`; the
root `go.work` includes it. Run `go test -race ./...` here.

## What's verified (2026-10-27)

All of this was run locally, without tailnet enrollment:

- `go test -race ./...` passes. That includes integration tests on real tmux and
  real git, and the harness against an in-process sandboxd. `golangci-lint run`
  with the repo config reports 0 issues.
- `docker buildx bake -f sandbox/image/docker-bake.hcl sandbox-local` builds
  `headmesh-sandbox:dev` (2.39 GB). hadolint is clean, and container-structure-test
  passes 43/43.
- `headmesh-sandbox up` starts the container through Compose with the §6.3
  hardening. It reports healthy with tailscaled logged out, and the unconfigured
  front doors idle and say why. `status` and `doctor` work.
- Against the running container, `tools/list` (31 tools), `exec`, `session_*` and
  `fs_*` pass over the unix socket, and over HTTP with a bearer token. Both
  protocol versions work: 2026-07-28 stateless and the 2025-11-25 handshake
  (`tests/e2e`).
- Acceptance, route `mcp` and route `ssh` (run inside the container, phase 1, then
  phase 2 from a new process):
  - Checks 1–3 pass: uid 1000, Ubuntu 26.04, the file hash matches the host's
    copy, and the dependency runs.
  - Check 5's mechanics pass: the session is still running after the reconnect,
    and the marker exists.
  - Checks 4 and 5 still fail because the node isn't enrolled yet.
- `acceptance recreate` passes 5/5: home and `~/.bash_history` persist, the boot
  identity changes, and old sessions report `lost (container restarted at …)`.
  The tailnet identity part was skipped (not enrolled).
- YOLO:
  - It is refused without a fresh backup record.
  - With one, it starts with the per-subtree read-only `/host`. Writes fail even
    as root, propagation is `private`, `/etc/shadow` reads as 0 bytes, `~/.ssh`
    is masked, and the host tailscaled state and Docker data-root are left out.
  - It halts itself at the TTL, with an audit entry, and the next `up` is Regular.

Spike outcomes are in `docs/adr/spike-t0{1,2,3,7,8}-*.md` and
`docs/adr/spike-t16-*.md`. Other implementation decisions are in
`docs/adr/0001-sandbox-implementation-decisions.md`.

## Security findings to act on

1. **Install the egress firewall before enrolling** (`host/firewall`, step 1
   below). A Docker bridge container can otherwise reach whatever the host can:
   its LAN, its own services and, on a host that is itself a tailnet node, that
   tailnet. Host-specific findings are in the internal ops notes.
2. YOLO shows the host read-only. That includes data directories and
   certificates, wherever file permissions allow. Agents run as uid
   1000; root-only files stay unreadable to them, but tailscaled and the runner
   run as root in the container. Add anything sensitive to
   `[mounts] extra_denylist`: denylisted subtrees are left out of `/host`.
3. Inside the sandbox, `UV_PROJECT_ENVIRONMENT=.venv-sandbox`, so `uv` never
   rebuilds a project's host `.venv` against container paths. Make sure each
   project's `.gitignore` covers `.venv-sandbox` (or `.venv*`).

## Quick start (host)

```bash
cd /mnt/storage/projects/headmesh/sandbox
go build -o ~/.local/bin/headmesh-sandbox ./cmd/headmesh-sandbox
docker buildx bake -f image/docker-bake.hcl sandbox-local     # headmesh-sandbox:dev
headmesh-sandbox init                                          # /mnt/storage/headmesh/sandbox
$EDITOR /mnt/storage/headmesh/sandbox/config/sandbox.toml      # image = "headmesh-sandbox:dev" until GHCR has a release
headmesh-sandbox scan-secrets                                  # review, then --apply
headmesh-sandbox token create claude-code-mac > ~/.config/headmesh/sandbox-token
headmesh-sandbox render --diff
headmesh-sandbox up
headmesh-sandbox status
headmesh-sandbox doctor
```

Other commands: `down [--yolo|--all]`, `recreate`, `logs [service|audit] [-f]`,
`shell [--root]`, `attach` (watch agents in tmux), `backup`,
`token list|revoke`, `acceptance run|verify|report|recreate`, and
`agent run --provider openai`.

Secrets live in `/mnt/storage/headmesh/sandbox/secrets/<name>`, mode 0600; an
empty file means that front door is off. They are never mounted. The CLI passes
them to `docker compose` through the environment, and Compose copies each one
into the container as `root:root 0400` (spike T-1).

## Steps that need Aaryn

Each step needs sudo, a production change or an account. None of them has been
done.

### 1. Host (sudo)

- Install the egress firewall and the home backup units, then run a first backup.
  The exact commands are in `host/README.md`. Check the result with
  `headmesh-sandbox doctor`; it should show `egress firewall ✓` and `home backup ✓`.
- Install restic (plan §5.3). Sleep targets are already masked (`doctor` checks
  this).

### 2. Headscale policy (production ACL change; maintenance window)

This builds on plan §5.2 and §6.6. Add the tags and grants to
`api/infra/headscale/policy.hujson` in the Xcelsior repo. Run
`headscale policy check` first; the policy `tests` must pass.

```hujson
"tagOwners": { "tag:sandbox": ["group:admin"], "tag:sandbox-yolo": ["group:admin"] },
"grants": [
  // Regular: only the private services you list (keep sandbox.toml [mode.regular] reach in sync).
  { "src": ["tag:sandbox"], "dst": ["tower-server"], "ip": ["tcp:11434"] },
  { "src": ["tag:sandbox-yolo"], "dst": ["*"], "ip": ["*"] },
],
"ssh": [
  { "action": "accept", "src": ["xcelsior@"], "dst": ["tag:sandbox", "tag:sandbox-yolo"], "users": ["agent"] },
],
```

`xcelsior@` must mean Aaryn's own devices only. That's true once the provider
hosts are tagged (§5.2).

### 3. Enroll the node

```bash
# On prod-hetzner: a single-use pre-auth key that carries the tag (Headscale 0.29.6).
sudo headscale preauthkeys create --user <owner> --tags tag:sandbox --expiration 1h
# On the ASUS: store it and recreate.
install -m 600 /dev/stdin /mnt/storage/headmesh/sandbox/secrets/ts_authkey   # paste, then Ctrl-D
headmesh-sandbox recreate
headmesh-sandbox status        # tailnet Running, node asus-sandbox, dns-check ok
```

- The key alone sets the tags. `--advertise-tags` is never passed, because
  Headscale rejects it with a tagged key (spike T-8). `tailscale-up` checks the
  enrolled tags against `TS_TAGS` instead.
- `tailscale up` runs with the key only while no state exists, so later restarts
  never re-authenticate. Tagged nodes don't expire. Clear the key file after
  enrollment.
- YOLO is a separate node (`asus-sandbox-yolo`, `tag:sandbox-yolo`) with its own
  state directory. Enroll it the same way, with a tag:sandbox-yolo key, during
  the first `up --yolo`.

### 4. Front doors

**MCP over the tailnet** (Claude Code, any MCP client):

```bash
headmesh-sandbox token create claude-code-mac          # prints hms_… once
claude mcp add --transport http sandbox http://asus-sandbox.mesh.xcelsior.ca:8780/mcp \
  --header "Authorization: Bearer hms_…"
```

Tokens are scoped (`sandbox:read|exec|write|admin`) and can be revoked:
`headmesh-sandbox token revoke claude-code-mac` takes effect within seconds.
OAuth (RFC 9728 metadata is already served at
`/.well-known/oauth-protected-resource/mcp`) arrives with the Headmesh
authorization server in M3.

**Claude self-hosted runner** (plan §7.1):

1. Turn on "Allow self-hosted environments" in claude.ai admin settings.
2. Put the environment secret in `secrets/claude_env_secret`, and fill
   `secrets/github_token` with a fine-grained PAT.
3. Set `[entrypoints.claude_runner] enabled = true`, then run
   `headmesh-sandbox up`.

The runner runs as root. Every session goes through `claude-session`, which
drops to `agent` with `setpriv`, so sessions can't read the secret (spike T-1).
The checkout hook links repos mapped in `[repos]` to the shared working copy, on
`main`. One live check is still pending: what the runner puts in the sessions'
environment.

**Claude Remote Control** (plan §7.2):

1. Set `[entrypoints.claude_rc] enabled = true`, then run `headmesh-sandbox up`.
2. Run `headmesh-sandbox shell`, then `claude auth login` (a full claude.ai
   login), trust `/home/agent`, and run `claude remote-control` once to answer
   "Enable Remote Control? y".
3. Run `headmesh-sandbox recreate`.

Never set `DISABLE_TELEMETRY`, `DO_NOT_TRACK`,
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` or `DISABLE_GROWTHBOOK`: Remote
Control refuses to start with any of them (spike T-2). Whether it restarts on its
own without a TTY still needs checking after this login.

**Codex over Tailscale SSH** (plan §7.3):

- This needs the `ssh` rule from step 2.
- In Codex desktop, add the remote host `agent@asus-sandbox.mesh.xcelsior.ca`.
- Smoke test: `ssh agent@asus-sandbox.mesh.xcelsior.ca codex --version`.
- Tailscale SSH works inside the container (spike T-3). Codex desktop itself is
  untested.

**OpenAI Secure MCP Tunnel** (plan §7.4):

1. In the Platform, create a Tunnel and a runtime key with Read+Use only. Put the
   key in `secrets/openai_tunnel_key` and the id in
   `[entrypoints.openai_tunnel] tunnel_id`. Set `enabled = true`, then run `up`.
2. Create a token for the tunnel route:
   `headmesh-sandbox token create chatgpt-tunnel --route tunnel` (exec ≤ 110 s,
   responses ≤ 8 MiB).
3. In ChatGPT: Plugins → Add custom MCP server → Connection: Tunnel, with bearer
   auth using that token.

**Codex `exec-server` via the Agents API** (plan §7.6):

1. Store the application key outside the sandbox. The CLI runs as you, so use
   `secrets/openai_app_key` (0600; never mounted, and denylisted from YOLO's
   `/host`). `/etc/headmesh/openai-app.key` is also accepted if you can read it.
2. Put the environment key (Agents tab, every other permission None) in
   `secrets/openai_env_key`.
3. Run:

   ```bash
   headmesh-sandbox agent run --provider openai --repo xcelsior \
     --prompt-file sandbox/tests/acceptance/prompt.md
   ```

The executor runs as `agent` in the repo with `--linux-sandbox-pid-namespace
inherit` and full access inside the container. Codex's bubblewrap sandbox can't
run under the container's seccomp and AppArmor profiles, so the container is the
boundary (spike T-16). The environment key reaches the executor through stdin,
and the transcript goes to `logs/agent-runs/<session>.jsonl`.

### 5. Acceptance (plan §6.15)

```bash
headmesh-sandbox acceptance run --route mcp \
  --url http://asus-sandbox.mesh.xcelsior.ca:8780/mcp --token-file ~/.config/headmesh/sandbox-token
ssh agent@asus-sandbox.mesh.xcelsior.ca headmesh-sandbox acceptance run --route ssh --phase 1
ssh agent@asus-sandbox.mesh.xcelsior.ca headmesh-sandbox acceptance run --route ssh --phase 2
# runner, exec-server, tunnel: the agent follows tests/acceptance/prompt.md
headmesh-sandbox acceptance recreate
headmesh-sandbox acceptance verify
headmesh-sandbox acceptance report --out ../docs/acceptance/$(date +%F).md
```

Set the targets in `sandbox.toml [acceptance]`:

- `allowed_target` must be granted to `tag:sandbox`.
- `denied_target` must be blocked. Run that probe only after the firewall is
  installed.

Check 4 accepts only a tailnet address for the MagicDNS name; a public DNS
record for the same name doesn't count.

### 6. YOLO

1. Make a dedicated deploy key, add it to GitHub as "headmesh-sandbox-yolo (asus)",
   and pin `known_hosts`:

   ```bash
   ssh-keygen -t ed25519 -f /mnt/storage/headmesh/sandbox/secrets/github_ssh_key \
     -C "headmesh-sandbox-yolo (asus)"
   ```

   It is passed only to the YOLO container.
2. Run `headmesh-sandbox backup`, then `headmesh-sandbox up --yolo --ttl 4h`.

The CLI refuses without a backup younger than `backup_max_age` and asks you to
confirm you're watching.

### 7. Publish the image

Push a `sandbox-v0.1.0` tag. `.github/workflows/sandbox-image.yml` builds it,
runs hadolint, container-structure-test and trivy, pushes to
`ghcr.io/headmesh-dev/sandbox`, and signs it with cosign. Then set
`image = "ghcr.io/headmesh-dev/sandbox:0.1.0@sha256:…"`.

## Troubleshooting

- Run `headmesh-sandbox doctor` first; every failure comes with a fix.
- `headmesh-sandbox logs sandboxd|tailscaled|tailscale-up|claude-runner|claude-rc|openai-tunnel|audit`.
- To watch agents live: `headmesh-sandbox attach`.
- "IP works, names fail": `dns-check` explains the fix (pitfall 3).
