# ADR 0002: self-hosted third-party MCP gateway, pre-flight verification and runtime isolation

- Status: proposed (2026-11-16)
- Context: post-v0.1 roadmap (after tailnet enrollment acceptance checks 4 & 5); builds on `ADR 0001`, `spike-t01`, `spike-t07`, `spike-t08` and `spike-t16`

This note records the architecture for 1-click installation and execution of
community or tenant-supplied MCP servers inside the Headmesh sandbox without
relying on paid external reputation APIs or weakening the Regular / YOLO
isolation boundaries.

---

## Problem and product rationale

Cloud agent entry points (Claude Remote Control, Claude self-hosted runner,
OpenAI Secure MCP Tunnel, Codex) cannot run third-party MCP servers such as
`postgres`, `redis`, `qdrant` or internal service wrappers against private mesh
targets (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), because vendor clouds are not
enrolled on the tenant's Headscale tailnet.

Running those MCP servers inside the tenant's Headmesh sandbox—and multiplexing
them behind `headmesh-sandboxd`'s single authenticated `:8780/mcp` front
door—turns every sandbox node into a mesh-aware MCP gateway with 1-click tool
attachment (`headmesh-sandbox mcp add <pkg>`).

However, spawning untrusted community `npx` / `uvx` packages directly inside the
main sandbox container as `agent` (`uid=1000`) breaks least privilege,
especially in `[mode.yolo]`, where `uid=1000` holds:

- `/home/aaryn` mounted read-write and per-subtree `/host/<X>` mounted read-only;
- `/run/hm-secrets/github_token` and `/run/hm-secrets/github_ssh_key`;
- reachability to tailnet databases and (when `allow_lan = true`) LAN hosts;
- public WAN egress through `br-hm-sbx0`.

A compromised or malicious database MCP running as `uid=1000` in YOLO mode could
read mesh databases and host files and immediately `POST` them to an external
host, or inject hidden instructions via `tools/list` descriptions (tool
poisoning / rug-pull updates).

## Decision: capability enforcement over paid reputation feeds

External reputation APIs (for example, M8ven's paid Tool Trust API) were
evaluated and **rejected as a runtime trust boundary**:

1. **Point-in-time lab telemetry cannot prevent live exfiltration:** a static
   verdict reflects how a package behaved during a third party's earlier scan.
   It misses zero-day maintainer compromises, unindexed or private tenant MCPs,
   and environment-triggered payloads that activate only when `DATABASE_URL` or
   mesh routes exist.
2. **Capabilities enforce invariants regardless of intent:** a database MCP that
   has no host filesystem mounts and zero WAN/DNS egress at the kernel socket
   layer physically cannot exfiltrate query results to the internet even if the
   package is compromised.
3. **Zero recurring SaaS dependency:** Headmesh stays 100 % self-hostable using
   open registries, local static/supply-chain scanners, optional mesh-local LLM
   inspection (`tower-server:11434`), and kernel primitives already present in
   the sandbox stack.

---

## Architecture (four layers)

### 1. Catalog discovery (`registry.modelcontextprotocol.io`)

1-click discovery queries the upstream open Model Context Protocol Registry API,
which consumes the same `server.json` manifest format already shipped in
`sandbox/server.json`, supplemented by git/local manifest URLs for private
tenant tools.

### 2. Host pre-flight verification and lockfile (`headmesh-sandbox mcp add`)

No third-party MCP server may execute floating tags (`npx -y pkg@latest` or
unpinned `uvx`) inside a running sandbox. `headmesh-sandbox mcp add <pkg>` runs
a four-step quarantine pipeline before writing `${root}/state/mcp-lock.json`:

1. **Artifact & digest resolution:** resolve exact dependency trees (`npm`
   SHA-512 tarball integrity, `uv.lock` SHA-256 wheel hashes, or OCI image
   `@sha256:…` digests) into the shared package caches (`hm-cache-npm`,
   `hm-cache-uv`).
2. **Supply-chain & advisory gate:** scan the resolved lockfile with
   `github.com/google/osv-scanner` (native Go module in `go.work`) against OSV
   malware/CVE advisories, and reject newly published package versions younger
   than 48 hours unless `--allow-unverified` is passed explicitly.
3. **Offline dry-run & `tools/list` capture:** start the server once inside an
   ephemeral `--network=none` container with synthetic placeholder environment
   variables, perform the MCP `initialize` + `tools/list` handshake, and fail if
   startup attempts unauthorized path probes or crashes.
4. **Tool-poisoning scan & SHA-256 schema pin:** inspect all tool names,
   descriptions and JSON Schema fields using embedded YARA/heuristic rules
   (Apache-2.0 patterns from `mcp-scanner` / `mcp-scan` detecting hidden prompt
   injections, cross-origin shadowing and credential-harvesting instructions),
   plus an optional semantic pass against a configured mesh model (such as
   `tower-server:11434`). Record the canonical SHA-256 digest of `tools/list`
   (`tools_sha256`) in `mcp-lock.json`.

### 3. Sidecar mount isolation + shared tailnet netns + per-UID `nftables`

#### Why inner `bwrap` and same-container `setpriv` are insufficient

- **Bubblewrap (`CLONE_NEWUSER`):** `spike-t16` established that Docker's
  default seccomp and AppArmor profiles block user namespaces, and `ADR 0001`
  rejected relaxing the outer container security profile.
- **Same-container `setpriv` alone:** running a child MCP under a dedicated UID
  inside `headmesh-sandbox-yolo` blocks `0400`/`0700` files (`spike-t01`), but
  any world-readable (`0644`/`0755`) file under `/host/<X>` or shared project
  mounts remains readable in the shared mount namespace without `CAP_SYS_ADMIN`
  or `CAP_SYS_CHROOT`.

#### Chosen boundary: `sandbox-mcp` companion container

When one or more `[mcps.<name>]` entries are enabled in `sandbox.toml`,
`headmesh-sandbox render` emits a companion `sandbox-mcp` (or
`sandbox-mcp-yolo`) service in `compose.override.generated.yaml`:

- **Separate mount namespace (`read_only: true`):**
  - `cap_drop: [ALL]` (zero Linux capabilities), `security_opt: ["no-new-privileges:true"]`.
  - **No** `/host/<X>`, **no** `/home/aaryn`, **no** `/home/agent`, **no**
    `ts_authkey` / `claude_env_secret` / `github_token` / `github_ssh_key`, and
    **no** `[mounts]` project directories unless explicitly granted in
    `[mcps.<name>].mounts`.
  - Mounts `/cache/npm` and `/cache/uv` **read-only** at runtime (populated
    only during `mcp add`), `/run/hm-mcp` (shared Unix socket directory with
    `sandboxd`), `/tmp` (`tmpfs`, `nosuid,nodev,noexec` where supported, or
    per-UID scratch), and env-sourced per-MCP secrets at
    `/run/hm-secrets/mcp-<name>` (`<mcp_uid>:root 0400`, written via
    copy → chmod → chown per `container-contract.md`).
- **Shared network namespace (`network_mode: "service:sandbox"` / `service:sandbox-yolo`):**
  - Shares `tailscale0` (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) and `br-hm-sbx0`
    (`172.31.250.10` / `.11`, still governed on the host by `inet headmesh_sbx`)
    without consuming a second Headscale node identity.
- **Per-MCP UID + in-netns `nftables` (`inet hm_mcp_egress`):**
  - Without user namespaces, sockets opened by UID `N` inside `sandbox-mcp`
    carry kernel `sk_uid = N` in the shared network namespace's `output` hook.
  - Each configured MCP is assigned a deterministic system UID in `940–989`
    (below `hmtunnel` `990` and `AGENT_UID` `1000`).
  - An s6 oneshot (`mcp-firewall`) running as `root` inside `sandbox` (which
    already holds `CAP_NET_ADMIN` for `tailscaled`) loads `nftables` table
    `inet hm_mcp_egress` enforcing three strict egress profiles:

| Profile | UID range | Permitted egress | Dropped egress | Intended tools |
|---|---|---|---|---|
| `offline` | `940–949` | Unix domain socket in `/run/hm-mcp` only | All `AF_INET` and `AF_INET6` sockets | SQLite (explicit single-file mount), local AST/document tools |
| `mesh-only` | `950–969` | Explicit `ip:port` targets from `[mcps.<name>].reach` in `100.64.0.0/10`, `fd7a:115c:a1e0::/48` (plus LAN targets only when `yolo.allow_lan = true`) | All public WAN (`0.0.0.0/0`, `::/0`), `127.0.0.0/8` (`:8780`, `:8081`, `:8082`), and UDP/TCP port `53` | `postgres`, `redis`, `qdrant`, internal mesh HTTP/gRPC services |
| `wan-only` | `970–989` | Public WAN TCP `443` (or explicit port list) and `127.0.0.11:53` DNS | `100.64.0.0/10`, `fd7a:115c:a1e0::/48`, RFC 1918 LAN, link-local, and `127.0.0.0/8` loopback services (`:8780`, `:8081`, `:8082`) | `github`, `stripe`, `linear`, public search/docs APIs |

- **Preventing DNS-tunneling exfiltration in `mesh-only`:**
  - If a `mesh-only` UID could send recursive DNS queries (`<hex-secret>.attacker.tld`)
    to `127.0.0.11:53` or `100.100.100.100:53`, data could escape despite WAN
    TCP/UDP drops. Port `53` is therefore dropped for `950–969`; MagicDNS names
    declared in `[mcps.<name>].reach` are resolved ahead of time by the
    supervisor and injected via `/etc/hosts` or resolved IP endpoints.

### 4. `headmesh-sandboxd` multiplexing, redaction and audit

`headmesh-sandboxd` remains unprivileged (`agent`, `uid=1000`) and dials each
MCP worker over `/run/hm-mcp/<name>.sock`:

1. **Handshake & lock verification:** on connect (and reconnect), `sandboxd`
   fetches `tools/list`, computes its canonical SHA-256, and compares it against
   `${root}/state/mcp-lock.json`. Any mismatch marks the MCP `degraded` in
   `hm status` and withholds its tools until re-verified.
2. **Deterministic namespacing:** tools are exported on `:8780/mcp` as
   `<name>__<tool>` (for example, `postgres__query`). Built-in `sandboxd` tool
   names (`exec`, `fs_*`, `session_*`, `artifact_*`) are reserved and cannot be
   shadowed.
3. **Mandatory secret scrubbing:** responses from third-party MCPs always pass
   through `sandboxd/redact` (`gitleaks` + built-in patterns), even when global
   `redact_output = false` for `agent`'s own `exec` calls.
4. **Structured audit:** every proxied `tools/call` writes an entry to
   `/var/log/headmesh/audit.jsonl` tagged with `mcp_name`, `mcp_uid`, `profile`,
   `package_digest` and `duration_ms`.

---

## Configuration sketch (`sandbox.toml`)

```toml
[mcps.postgres]
enabled = true
package = "npm:@modelcontextprotocol/server-postgres@0.6.2"
profile = "mesh-only"
reach = ["prod-hetzner.mesh.xcelsior.ca:5432"]
secret_env = { DATABASE_URL = "mcp_postgres_url" } # sourced from ${root}/secrets/mcp_postgres_url

[mcps.github]
enabled = true
package = "oci:ghcr.io/github/github-mcp-server@sha256:..."
profile = "wan-only"
secret_env = { GITHUB_PERSONAL_ACCESS_TOKEN = "mcp_github_pat" }

[mcps.sqlite_analytics]
enabled = true
package = "uv:mcp-server-sqlite==0.7.1"
profile = "offline"
mounts = [{ path = "/mnt/storage/data/analytics.db", mode = "ro" }]
```

---

## Pre-implementation spike checklist (`spike-t18`)

Run and record these four checks on ASUS before implementing `sandbox-mcp`
(`T-17` is already assigned to R2 object lock in `docs/plan.md` §17):

1. **Cross-container `meta skuid` match (`T-18a`):** start a minimal sidecar with
   `network_mode: "service:sandbox"` and `cap_drop: [ALL]`, load a test rule in
   `sandbox` matching `meta skuid 951`, and confirm outbound packets from UID
   `951` in the sidecar hit the counter and drop cleanly.
2. **Loopback & DNS exfil closure (`T-18b`):** from UID `951` (`mesh-only`),
   verify TCP to `<tailnet-target>:5432` succeeds while `1.1.1.1:443`,
   `127.0.0.1:8780` (`sandboxd`), `127.0.0.1:8082` (`tunnel-client`) and UDP/TCP
   `:53` (`getent hosts leak.example.com`) all fail immediately.
3. **YOLO filesystem isolation (`T-18c`):** start `sandbox-yolo` +
   `sandbox-mcp-yolo` and confirm UID `951` inside `sandbox-mcp-yolo` gets
   `ENOENT` on `/host`, `/home/aaryn` and `/run/hm-secrets/github_ssh_key`.
4. **Schema rug-pull tripwire (`T-18d`):** mutate one character in a locked mock
   MCP server's `tools/list` description and confirm `sandboxd` withholds the
   server's tools and reports `state = "degraded", reason = "tools_sha256 mismatch"`.
