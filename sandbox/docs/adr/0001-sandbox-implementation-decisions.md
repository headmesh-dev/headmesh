# ADR 0001: sandbox implementation decisions

- Status: accepted (2026-10-28)
- Plan: §6, §7, §17; spike outcomes are in the `spike-t*` notes next to this file.

This note records where the implementation makes a choice the plan left open,
or departs from the plan, and why.

## Secrets: environment-sourced Compose secrets

Compose 2.40.3 ignores `uid/gid/mode` on file-sourced secrets. They become bind
mounts owned by the host user, UID 1000, which is also `agent` inside the
container. Environment-sourced secrets are copied into the container as
`root:root 0400`. So the CLI reads `${root}/secrets/<name>` and passes the values
only to the `docker compose` process.

- The secrets directory is never mounted, and YOLO's `/host` leaves it out.
- `github_ssh_key` (YOLO's deploy key) is a secret like the others. It replaces
  the plan's bind-mounted key file, and Regular gets an empty value.
- This needs a writable rootfs, because Compose copies the secrets in at create
  time (spike T-8).

## Token hashes live in `config/tokens.json`, not a Compose secret

The plan put token hashes in `secrets/sandboxd_tokens`. Compose secrets are
copied when the container is created, so a revocation would need a restart. The
file holds only argon2id hashes of 256-bit random tokens, which aren't secret in
practice. It lives in the read-only `config/` bind (a directory bind, so atomic
renames show through). sandboxd re-reads it within 2 seconds, so
`headmesh-sandbox token revoke` takes effect immediately.

## Dependencies

The pinned libraries are used as planned: go-sdk v1.8.0, cobra 1.10.2, koanf v2
(TOML) and go-cmp. Additions:

- `golang.org/x/crypto/argon2`: §6.8 requires argon2id, and Go has no stdlib
  implementation. Aaryn approved it on 2026-10-27.
- `google/jsonschema-go`: validates `sandbox.toml` against its JSON Schema. It was
  already in the build through go-sdk.

No YAML library: the Compose override is written by a 150-line deterministic
writer (`internal/yamlw`) that double-quotes every string.

## YOLO host view is per-subtree

Spike T-7 found that a single `/` → `/host` bind is unusable:

- `rprivate` is refused, because the Docker data-root is under `/`;
- `rshared` leaks the masks onto the host;
- `rslave` shows mounts the host makes later, read-write.

`render` therefore binds each top-level directory of `/` with `read_only`,
`bind.recursive: readonly` and `propagation: rprivate`. It skips `proc`, `sys`,
`dev`, `run`, `tmp` and every denylisted subtree. Around each data-root it
descends and binds the siblings, so no bind contains a data-root. Denylisted
paths under a bound subtree are masked: `tmpfs` for directories, `/dev/null`
for files.

The built-in denylist adds these to the plan's list:

- the Docker and tailscaled sockets;
- `/var/lib/docker`, `/var/lib/containerd`;
- the sandbox's own `state/` and `secrets/`;
- the restic repository.

## Health while logged out

"Not enrolled" (`NeedsLogin`/`NoState`) is a setup step, not a fault. So are
front doors that need Aaryn's first-run setup (runner, Remote Control, tunnel).
None of them makes the container unhealthy. A failed `dns-check` once enrolled
does, and so does a missing tmux server. That gives "fails startup health"
without the crash loop `S6_BEHAVIOUR_IF_STAGE2_FAILS=2` would cause.

## Stop time

`S6_KILL_GRACETIME` is 3000, not 80000: s6 sleeps the full grace on every stop,
which made `docker stop` take 80 s. The runner keeps its 80 s drain through its
own `timeout-kill`, and Compose's `stop_grace_period` stays at 90 s.

## Separate Python environments

The project folders are shared with the host, and container Python lives at a
different path. If `uv` ran against a project's `.venv`, it would rebuild the
host environment. The image therefore sets `UV_PROJECT_ENVIRONMENT=.venv-sandbox`.
Node's `node_modules` is shared deliberately: same OS and libc (pitfall 5).

## codex exec-server runs with full access inside the container

Codex's bubblewrap sandbox needs user namespaces, which the container's seccomp
and AppArmor profiles forbid (spike T-16). The orchestrator therefore starts the
executor with `-c sandbox_mode="danger-full-access" -c approval_policy="never"`
and `--linux-sandbox-pid-namespace inherit`. The container is the boundary, as
for SSH and `exec`.

Relaxing the container profile was rejected: it would weaken the outer boundary
to gain an inner one.

## Acceptance check 4 requires a tailnet address

`prod-hetzner.mesh.xcelsior.ca` also has a public DNS record (<prod-hetzner IPv4>), so
`getent` succeeding proves nothing about MagicDNS. Check 4 passes only for an
address in 100.64.0.0/10 or fd7a:115c:a1e0::/48.

## Agents API orchestrator

It follows the October 2026 docs:

- `POST /v1/agents/sessions` with `OpenAI-Beta: agents=v1`;
- an SSE stream on `/events?stream=true`;
- `agent.session.input.message` sent with an `Idempotency-Key`;
- completion is the root turn's `agent.session.turn.completed`.

The application key is read from `${root}/secrets/openai_app_key` (or
`/etc/headmesh/openai-app.key` if readable), because the CLI runs as Aaryn, not
root.

The default model, `gpt-6-astra`, is the docs' example; use `--model` to change
it. This is unverified against the live API until the keys exist.
