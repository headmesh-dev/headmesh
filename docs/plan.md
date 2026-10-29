# Headmesh — Technical Implementation Plan

Source blueprint: `/mnt/storage/projects/Headscale Sandbox Plugin Handoff.md`.
Plan date: 2026-10-26. Owner: Aaryn Biro (Xcelsior Compute Inc.).
Everything below is the recommended approach. Items marked **SPIKE** are unverified facts with a time-boxed check and a named fallback (§17).

---

## 0. Context

**Why this work.** Cloud agents (Claude, Codex) can't join Aaryn's self-hosted Headscale tailnet through their managed VPN settings. The blueprint's answer is a persistent Docker sandbox on the ASUS that joins Headscale itself, with several "front doors" for cloud agents. It is packaged as a plugin and grows into **Headmesh**: an open, self-hostable, multi-tenant Tailscale-style product built on Headscale. Xcelsior is its first customer. Around it: a plugin marketplace (Headmesh + Xcelsior), a CLI, a sign-up page and a marketing site.

**What this plan delivers.**
- A production-grade Go monorepo `github.com/headmesh-dev/headmesh`.
- A live agent sandbox on the ASUS that passes the five-step acceptance test through each front door.
- Plugins published for Claude Code and Codex.
- The Headmesh control plane running on one Hetzner VM in Nuremberg, managing two Headscale "cells".
- An integration folder in the Xcelsior repo.
- headmesh.dev with a waitlist.

**Decisions locked with Aaryn (2026-10-26).**

| Topic | Decision |
|---|---|
| Names | GitHub org **`headmesh-dev`**. The plain `headmesh` org is taken by an unrelated, empty 2022 account. Domain **headmesh.dev**: registered today at Porkbun, DNS already on Cloudflare. Contact **aaryn.biro@headmesh.dev** plus **hello@headmesh.dev**. |
| Core language | **Go** for core, api, cli, sandbox daemon and cell agent. Xcelsior stays Python and calls the API. |
| Headscale | **Two cells.** Adopt `hs.xcelsior.ca` in place as cell `xcelsior`, keeping the 100.64.0.x IPs the Xcelsior code hardcodes. Add a fresh shared cell `hs1` (`hs1.headmesh.dev`) for public sign-ups. |
| Headscale API exposure | **No gRPC exposure on prod-hetzner.** A cell agent beside each Headscale talks to it over its local unix socket and only connects **outbound** to Headmesh. Any design that opens Headscale gRPC needs Aaryn's explicit approval first (§9.4). |
| Codex front doors | **All three in M1.** SSH remote connections are the primary route: they land **inside the sandbox container**, allowed only from Aaryn's own devices. Plus **OpenAI Secure MCP Tunnel**, plus the **`codex exec-server` orchestrator** (Agents API, §7.6). The acceptance test runs through SSH **and** exec-server. Exec-server adds no Hetzner cost: it runs in the sandbox on the ASUS, and OpenAI bills Agents API usage. |
| Hosting | **One VM in Hetzner `nbg1` + on-demand standby.** Continuous replication to Cloudflare R2. Scripted restore to `fsn1`/`hel1` through the Hetzner API with fencing. No idle second server. No Canadian data-residency requirement. |
| Cost | Create hm-prod-1 **now in M0** so the expiring Hetzner credits pay for it. Create it as a `cx23` and rescale **CPU/RAM-only** to `cx33` while credits last; that keeps the 40 GB disk, so it can drop back to `cx23` (€5.99/month) afterwards unless metrics justify `cx33` (€8.99/month) (§13.1). |
| prod-hetzner disk | **Reclaim approved by Aaryn (2026-10-26).** Make it **set-and-forget** so nobody cleans the server by hand again (§5.4). |
| npm | **Org `headmesh` secured by Aaryn (2026-10-27).** The `@headmesh/*` scope is reserved. CI publishes with npm trusted publishing (§5.5, §10). |
| hm-prod-1 | **Created by Aaryn (2026-10-27)** in Hetzner project **"Headmesh"**: `cx23`, x86, 40 GB, `nbg1` (eu-central), IPv4 **<hm-prod-1 IPv4>**, **Ubuntu 26.04**. Root SSH key on the ASUS: `~/.ssh/hm-prod-1`. Next: Cloud Firewall, then bootstrap (§5.8). |
| OS baseline | **Ubuntu 26.04 LTS ("resolute") on every machine**: ASUS (26.04.1), tower (upgraded 2026-10-26), hm-prod-1 (26.04), prod-hetzner (24.04 → 26.04.1, §5.7). Images, docs and scripts target 26.04. |
| prod-hetzner OS upgrade | **Approved (2026-10-27). Aaryn runs it** from paste-ready commands; agents have no SSH access to prod-hetzner. Done 2026-10-27; the runbook is in the internal ops notes. |
| Execution | **Aaryn approves everything in this plan (2026-10-27).** Steps marked "approval" are pre-approved; prod-touching steps still announce themselves before they run. Every session follows §3.3 "Package discipline". |

---

## 1. Ground truth discovered (these change the blueprint's assumptions)

**Hosts.** The reference sandbox host is a laptop on Ubuntu 26.04.1 with Docker 29, an RTX 2060 (NVIDIA Container Toolkit with CDI) and an existing Tailscale instance on another tailnet, so the sandbox must never share the host network. Production runs on Hetzner Cloud in `nbg1`. Inventory, addresses and the state of the existing Headscale failover are in the internal ops notes.

**Third-party apt repos on 26.04 (from the ASUS's upgrade, 2026-10-27)**
- Tailscale publishes a `resolute` suite (in use on the ASUS).
- deadsnakes has a `resolute` PPA sources file on the ASUS.
- nodesource uses the codename-free `nodistro` suite and Grafana uses `stable main`, so neither needs re-pointing.
- Docker's suite is codename-specific (`download.docker.com/linux/ubuntu resolute stable`): confirm it exists during §5.7; if not, leave it out and record it.
- The upgrader leaves `*.distUpgrade` copies of the old files in `/etc/apt/sources.list.d/`.


**Xcelsior today.** Xcelsior runs its own Headscale (`hs.xcelsior.ca`) and an OAuth-protected MCP server (`https://mcp.xcelsior.ca/mcp`). Findings about its current network policy, enrollment code and MCP scopes, which shape §5.2, §8.3 and §11, are in the internal ops notes.

**External research (October 2026)**
- **Headscale v0.29.6 is the latest.** It adds:
  - **grants** (recommended over legacy `acls`), `via`, `nodeAttrs`, and policy `tests`/`sshTests` that run on `policy set`;
  - tagged nodes that **never expire**;
  - `ExpireNode{disable_expiry}`;
  - pre-auth keys with `acl_tags` and an optional user;
  - gRPC/REST: `SetPolicy`, `CheckPolicy`, `SetTags`, `RenameNode`, `DeleteNode`, `AuthApprove`.
- Headscale notes: Postgres is "highly discouraged"; `base_domain` must differ from the `server_url` domain; `extra_records` are global.
- **MCP spec is 2026-07-28.** It is stateless: no `initialize`, no `Mcp-Session-Id`, and `server/discover` is required. **DCR is deprecated in favour of CIMD.** RFC 9728, PKCE and RFC 8707 are required.
- **MCP SDKs:** the Go SDK v1.8.0 serves 2026-07-28 only with `StreamableHTTPOptions.Stateless=true`, and otherwise negotiates down to 2025-11-25. The TS SDK is v2.4.0.
- **Claude self-hosted runners** (public beta, Team and Enterprise):
  - `claude self-hosted-runner`, outbound only.
  - It **clones the repo into `<base-dir>/<owner>/<repo>` and creates a working branch**.
  - Hooks: `checkout`, `command`, `post-session`. Health on `--health-port`.
  - Exits after its sessions end, so it needs a supervisor.
  - **Session code can read the environment secret.**
- **Claude Remote Control** needs a full claude.ai login (not `setup-token`), a TTY on first run and workspace trust. It exits after about 10 minutes offline.
- **`codex exec-server`** is documented only as the **Agents API per-session self-hosted sandbox**. It is not a front door for Codex Cloud tasks.
- **OpenAI Secure MCP Tunnel:** `ghcr.io/openai/tunnel-client`, outbound only. Limits: 10 MiB per message, 120 s, 600 requests/minute. The OAuth authorization server is **not** tunneled.
- **Plugin formats:**
  - Codex reads `.claude-plugin/marketplace.json` **and** `.agents/plugins/marketplace.json` (where `category` is required).
  - Codex plugin manifest: the portable `plugin.json` (Agent Plugins 1.0.0, with `extensions["com.openai"]`), falling back to `.codex-plugin/plugin.json`. Portable `mcp.json`.
  - Claude: `.claude-plugin/plugin.json` plus `.mcp.json`, validated with `claude plugin validate --strict`.
- **Docker Compose:** `bind.recursive: readonly` is in the spec. `depends_on.restart: true` covers **only Compose-initiated restarts, not crash restarts**.
- **Hetzner API pricing** (net €/month, VAT 0%):
  - EU: `cx23` €5.49 (€0.0088/h), `cx33` €8.49, `cx43` €15.99, with 20 TB traffic.
  - US (Ashburn/Hillsboro) is 3–4× the price with 1–3 TB traffic.
  - Add-ons: IPv4 €0.50, floating IPv4 €3, backups +20%, volumes €0.0572/GB.
  - `cx` (x86) is now cheaper than `cax` (ARM).
- **Name availability:** npm org `headmesh` **secured 2026-10-27**; PyPI `headmesh` is free.

---

## 2. Architecture overview

```
 Cloudflare: DNS · Pages (headmesh.dev, get.headmesh.dev) · D1 (waitlist) · Turnstile · Workers cron (external monitor)
             · R2 (Litestream + pgBackRest + encrypted keys + tofu state) · Email Routing (hello@, aaryn.biro@)

 Hetzner nbg1 — hm-prod-1 <hm-prod-1 IPv4> (cx23 → cx33 when needed, Ubuntu 26.04), project "Headmesh" (separate token from prod-hetzner)
 ┌─────────────────────────────────────────────────────────────────────────────────────────┐
 │ Caddy 2 :443 ─► headmesh-server (REST API /v1, OAuth AS, web/app SPA, River workers)    │
 │                     │ pgx                                                               │
 │                 Postgres 18 ── pgBackRest ─► R2                                         │
 │ Headscale v0.29.6 (cell hs1, policy.mode=database, embedded DERP region 900)            │
 │     ▲ unix socket          Litestream (10 s) ─► R2                                      │
 │ headmesh-cellagent[hs1] ── outbound bidi stream (mTLS) ─► headmesh-server               │
 │ tailscaled (VM is a node in hs1 and in the xcelsior cell as tag:hm-ingress/ops)         │
 └─────────────────────────────────────────────────────────────────────────────────────────┘
          ▲ outbound only (HTTPS/mTLS) — no new inbound port, Headscale gRPC never exposed
 prod-hetzner (nbg1, Xcelsior prod): Headscale hs.xcelsior.ca (cell "xcelsior") + headmesh-cellagent[xcelsior]
          Xcelsior API → control_plane/headmesh (grants on rental events, onboarding via Headmesh API)

 ASUS (home): headmesh-sandbox container (bridge netns, own node tag:sandbox / tag:sandbox-yolo)
   s6-overlay: tailscaled(root, NET_ADMIN, Tailscale SSH) · tmux server · headmesh-sandboxd (MCP :8780)
               · claude self-hosted-runner · claude remote-control · openai tunnel-client · [codex exec-server add-on]
   Front doors: Claude cloud task → runner polls api.anthropic.com (outbound)
                Codex desktop "Remote connections" → Tailscale SSH (from Aaryn's devices only) → inside container
                ChatGPT/Codex → OpenAI tunnel ← tunnel-client (outbound) → sandboxd MCP
                Claude Code / any MCP client → MagicDNS URL (M1) or https://asus.sbx.headmesh.dev/mcp via Headmesh ingress (M3)
                Aaryn interactive → Claude Remote Control (host instance stays as fallback; sandbox instance added)
```

---

## 3. Repository, toolchain and engineering standards

### 3.1 Layout (single Go module `github.com/headmesh-dev/headmesh`, Go 1.26)

```
headmesh/
  cmd/headmesh/              # CLI entry
  cmd/headmesh-server/       # API + OAuth AS + web app + River workers (subcommands: serve, worker, migrate, recover)
  cmd/headmesh-cellagent/    # runs beside each Headscale
  cmd/headmesh-sandboxd/     # MCP + session daemon inside the sandbox
  core/                      # domain: tenancy/ devices/ enrollment/ grants/ rules/ services/ audit/ policy/ (compiler,
                             #   evaluator, timeline) cells/ reconcile/ webhooks/ authz/ events/
  api/                       # Huma v2 handlers, middleware, oauth/ (zitadel/oidc OP), openapi/openapi.yaml (generated, committed)
  cli/                       # cobra commands, join/instance mgmt, sandbox subcommands
  sandbox/
    image/                   # Dockerfile, rootfs/ (s6-rc.d services, git hooks, guidelines, tmux.conf), docker-bake.hcl
    compose/                 # compose.yaml (profiles regular|yolo), .env.example
    sandboxd/                # Go packages: mcp/ sessions/ (tmux) fs/ gitx/ status/ authn/ audit/
    entrypoints/             # claude-runner hooks, remote-control wrapper, tunnel profile, exec-server orchestrator (add-on)
    config/                  # sandbox.example.toml + JSON Schema
    tests/                   # acceptance harness, container-structure-test.yaml
  internal/                  # config (koanf), log (slog), otel, db (sqlc output), headscalepb wrapper, version, crypto
  db/                        # migrations/ (goose), queries/ (sqlc), sqlc.yaml
  proto/cell/v1/             # cell agent protocol (buf + connect-go)
  web/marketing/             # Astro + Starlight (headmesh.dev)
  web/app/                   # Vite + React SPA (embedded into headmesh-server via go:embed)
  plugins/headmesh/  plugins/xcelsior/
  .claude-plugin/marketplace.json   .agents/plugins/marketplace.json
  docs/                      # handoff.md (the blueprint), adr/, threat-model.md, runbooks/, acceptance/
  deploy/
    compose/                 # hm-prod-1 stack (caddy, headscale, server, cellagent, postgres, litestream, alloy)
    tofu/                    # OpenTofu: hcloud + cloudflare
    ha/                      # recovery/fencing tool + tests, restore-drill
    breakglass/              # WireGuard break-glass
    install/                 # install.sh served at get.headmesh.dev
    secrets/                 # sops+age encrypted
  test/e2e/                  # tenant isolation suite, revocation latency
  mise.toml  .golangci.yml  pnpm-workspace.yaml  .github/ (workflows/, renovate.json, SECURITY.md, CONTRIBUTING.md)
  LICENSE  LICENSING.md  AGENTS.md  CODEOWNERS
```

### 3.2 Pinned toolchain and libraries (latest stable as of 2026-10-26)

| Area | Choice (version) | Why |
|---|---|---|
| Tool/version manager | mise 2026.10.x (`mise.toml`) | Pins go/node/pnpm/linters per repo; also runs tasks |
| HTTP router | chi v5.5.0 | stdlib-compatible, stable |
| API framework | Huma v2.39.1 | Code-first OpenAPI 3.1, validation, RFC 9457 problem details |
| DB | pgx v5.11.0 + sqlc 1.31.1 | Type-safe SQL, no ORM magic |
| Migrations | goose v3.29.0 (embedded) | Go + SQL migrations, simple |
| Jobs/scheduling | River v0.49.0 | Postgres-native, transactional enqueue, unique/periodic jobs, leader election |
| OAuth/OIDC | zitadel/oidc v3.51.16 | OpenID-certified OP + RP; device flow, PKCE |
| Passkeys | go-webauthn v0.18.2 | Standard WebAuthn library |
| Headscale client | Generated gRPC stubs from Headscale **v0.29.6** (`gen/go/headscale/v1`) | Exact API parity |
| Cell protocol | connect-go v2.0.2 + buf | Bidi streaming over HTTP/2; no extra port |
| MCP | modelcontextprotocol/go-sdk v1.8.0 | Official; 2026-07-28 stateless mode plus 2025-11-25 fallback |
| CLI | cobra 1.10.2 + charmbracelet/fang v2 + huh v2 + lipgloss | Styled help/completions; prompts |
| Misc Go | tailscale/hujson, zalando/go-keyring, koanf v2, OpenTelemetry Go, prometheus/client_golang, standard-webhooks, creack/pty v1.1.24 | |
| Testing | stdlib testing + go-cmp, pgregory.net/rapid (property), Go fuzzing, testcontainers-go v0.44.0, rogpeppe testscript (CLI) | |
| Lint | golangci-lint v2.15 (gofumpt, staticcheck, gosec, errcheck, revive, …) | |
| Web marketing | Astro 7.4 + Starlight 0.42 + Tailwind 4.3 | Static, fast, docs built in |
| Web app | Vite 8.3 + React 19.3 + TanStack Router 1.170 + TanStack Query 5.105 + shadcn/ui + openapi-typescript 7.13 + openapi-fetch 0.17 | Type-safe client generated from OpenAPI |
| JS lint/test | Biome 2, Vitest, Playwright + axe | |
| Xcelsior client | openapi-python-client 0.29.1 + httpx 0.28 | Generated, typed |
| Release | GoReleaser 2.19, release-please, cosign (keyless), syft SBOM, `actions/attest-build-provenance` | Signed, provenance-attested artifacts |
| Infra | OpenTofu + hcloud + cloudflare providers, sops + age, Caddy 2, Litestream 0.5.17, pgBackRest, Grafana Alloy | |

### 3.3 Standards
- **Commits and versioning:** conventional commits with release-please changelogs. Semver for the CLI, server and plugins.
- **ADRs:** one in `docs/adr/` per decision in §0 and per spike outcome.
- **CI security:**
  - Every action pinned by SHA; `permissions: {}` by default.
  - zizmor (Actions security lint), actionlint, gitleaks, govulncheck, osv-scanner, trivy on images, CodeQL, OpenSSF Scorecard.
  - Renovate with Docker digest pinning; new stable releases are adopted without a waiting period (ADR 0010, 2026-10-29).
- **Go code:** contexts with deadlines on all I/O; no panics in request paths; errors wrapped with `%w`; every handler declares a required permission. A test fails if any route lacks one, mirroring Xcelsior's guard tests.
- **Branch protection:** rulesets block force-push and deletion on `main`. Note: private repos on a Free org can't enforce rulesets (decision gate §19).
- **Package discipline (every session, every track):**
  - Start from the libraries, tools and versions pinned in §3.2, §6.2 and §13.2.
  - **Upgrades only (Aaryn, 2026-10-27):** a session may switch to a **better** package than the plan names, meaning a newer stable release or a more battle-tested, better-maintained library for the same job. Record why in the commit message, and in an ADR under `docs/adr/` when it replaces a library. Never switch to a weaker, less-maintained, pre-release or merely equivalent option, and never add dependencies the plan doesn't need.
  - If a pinned version doesn't exist or doesn't work and nothing clearly better exists, stop and report it with evidence.
  - Commit lockfiles (`go.sum`, `pnpm-lock.yaml`, `uv.lock`). Upgrades come through Renovate, not by hand.
  - System packages target Ubuntu 26.04 (`resolute`); container base images are `ubuntu:26.04` pinned by digest.
- **Shared checkout:** all sessions work in `/mnt/storage/projects/headmesh` on `main` (no worktrees). Each session edits and commits only its own track's paths, makes small commits, and runs `git pull --rebase` before every push. Never reformat or rewrite another track's files.

---

## 4. Milestones (parallel tracks: Core, Xcelsior, Sandbox, Consumer)

| Milestone | Scope | Exit criteria |
|---|---|---|
| **M0 Prep (days 1–2)** | Fix failover standby; Xcelsior ACL hardening (approval); host prep; accounts; repo bootstrap; prod-hetzner disk automation; **prod-hetzner Ubuntu 26.04.1 upgrade (Aaryn runs, §5.7)**; **hm-prod-1 (created 2026-10-27): firewall and bootstrap (§5.8)**, then Caddy, break-glass WireGuard and an hs1 dev Headscale to build the core against | `headscale-failover status --json` shows `promotable:true`; ACL policy tests pass; repo with CI green; prod-hetzner disk under 70% with timers active; prod-hetzner on 26.04.1 with all post-checks green; hm-prod-1 reachable only through its firewall rules |
| **M1 Sandbox + plugins + marketing (weeks 1–2)** | Track A + B (runner, Remote Control, Tunnel, SSH, **exec-server orchestrator**); Track C; marketing + waitlist live | 5/5 acceptance checks via MCP, Claude runner, Codex-SSH, **Codex exec-server** and Tunnel routes, plus the recreate bonus; plugins validated and installable; headmesh.dev live |
| **M2 Core v0 + hs1 + CLI (weeks 2–5)** | Domain, compiler, cell agent, enrollment, grants, audit, API; hs1 on Hetzner; CLI join/invite/status | E2E isolation matrix green; revocation p99 < 5 s; `headmesh join` works on a fresh VM; first restore drill green |
| **M3 Auth + web app + ingress (weeks 4–6)** | Users, OAuth AS, sign-up page, sandbox OAuth, Headmesh ingress for MCP | Sign-up → org → first device in < 5 minutes; Claude.ai connector to sandbox via OAuth |
| **M4 Xcelsior cell + integration (weeks 5–8)** | Cell agent on prod-hetzner (observe → enforce); `control_plane/headmesh`; rental grants | Provider hosts provably isolated; rentals create and revoke grants; Xcelsior live gates green |
| **M5 Hardening + launch (weeks 8–10)** | Load tests, game day, pen-test checklist, license decision, public repo | Recovery game day under 15 minutes; launch checklist complete |

---

## 5. M0 — Preparation and safety

### 5.1 Fix the Headscale standby on the ASUS (sudo, needs approval)

Make the existing Headscale standby promotable again and add promote preflights in the Xcelsior failover tool. Status 2026-10-27: done. Details are in the internal ops notes.

### 5.2 Xcelsior tailnet hardening (Xcelsior repo; prod ACL change, so approval and a maintenance window)

Harden the Xcelsior cell's policy before Headmesh adopts it: tags for control, provider hosts and sandboxes, one `grants`-based policy file with policy `tests`, and container ports bound to the mesh IP. Needs a maintenance window. Details are in the internal ops notes.

### 5.3 Host prep on the ASUS (sudo, approval)

**Status 2026-10-27: done** with `deploy/host/asus-prep.sh` (idempotent). Sleep, suspend, hibernate and hybrid-sleep are masked; the logind lid settings apply at the next boot. Installed: restic 0.18.1 and age 1.2.1 from Ubuntu (ESM updates), and gitleaks 8.30.1 and sops 3.13.3 from pinned upstream releases with sha256 checks. Ubuntu's gitleaks 8.16 is too old, and sops isn't packaged. The GPU smoke test passed (RTX 2060 visible through CDI; `ubuntu:26.04` digest `sha256:19fcd1bf268f…`). Left by hand: BIOS "power on after AC loss" (step 2).
1. Mask `sleep`, `suspend`, `hibernate` and `hybrid-sleep`. Set `HandleLidSwitch=ignore` and `HandleLidSwitchExternalPower=ignore` in `/etc/systemd/logind.conf.d/headmesh.conf`.
2. In BIOS, enable "power on after AC loss" where available.
3. Install pinned **restic, gitleaks, sops, age**.
4. GPU smoke test: `docker run --rm --device nvidia.com/gpu=all ubuntu:26.04 nvidia-smi` (CDI is present).

### 5.4 prod-hetzner disk: reclaim now, then set-and-forget (reclaim approved 2026-10-26; config changes go in the Xcelsior repo `api/infra/`)

Reclaim prod-hetzner disk once, then make it self-maintaining (ADR 0008). Details and status are in the internal ops notes.

### 5.5 Accounts and settings (Aaryn does these by hand; I can't create accounts or enter credentials)

| Item | Steps |
|---|---|
| GitHub org `headmesh-dev` | Require 2FA; base permissions none; Actions allow-list; secret scanning and push protection; private vulnerability reporting |
| npm | **Done 2026-10-27:** org `headmesh` secured, reserving `@headmesh/cli`, `@headmesh/sdk` and the per-platform packages. Remaining: claim `headmesh` with a 0.0.0 placeholder from CI using trusted publishing |
| PyPI | Pending trusted publisher for `headmesh` (for the future Python SDK) |
| Cloudflare (zone already active) | Enable DNSSEC (DS record at Porkbun). Email Routing: `hello@` and `aaryn.biro@` → aaryn.biro@xcelsior.ca. **Sending** as aaryn.biro@headmesh.dev needs a mailbox provider (Zoho Mail free or Fastmail); decide later. Turnstile widget. Check Porkbun's .dev renewal price |
| Resend | Account; sending domain `mail.headmesh.dev` (SPF, DKIM, DMARC `p=quarantine` with `rua` reports) |
| Hetzner | **Done 2026-10-27:** project **"Headmesh"** and server hm-prod-1. Remaining: the Cloud Firewall (§5.8); a Read & Write API token for that project, stored only as the GitHub `recovery` environment secret and in sops (never in chat or the repo); note the project's server limit from Console → Limits |
| Claude Team | Owner turns on "Allow self-hosted environments" (claude.ai/admin-settings/cloud-environments), Remote Control and cloud sessions (claude.ai/admin-settings/claude-code), and the GitHub connector |
| OpenAI Platform | Create a Tunnel (Tunnels Read+Manage). Create a runtime key with Read+Use only for `tunnel-client` |
| Grafana Cloud (free) | Stack for Headmesh telemetry, kept separate from Xcelsior's LGTM |

### 5.6 Repo bootstrap

**Status 2026-10-28:** done. The repo is `headmesh-dev/headmesh` with CI for Go, the sandbox image, plugins, marketing and deploy. ADRs 0001–0015 are in `docs/adr/` (decisions from §0, the nginx move, spike outcomes and licensing).
- Create `headmesh-dev/headmesh`. Copy the blueprint into `docs/handoff.md`. Write ADR-0001…0012 from §0.
- Add `mise.toml`, linters, the CI skeleton, CODEOWNERS (`@aabiro`), SECURITY.md, Renovate and issue templates.
- Commit email: aaryn.biro@headmesh.dev (add it to GitHub verified emails).

### 5.7 prod-hetzner: Ubuntu 24.04 → 26.04.1 (approved 2026-10-27; Aaryn runs every command)

Upgrade prod-hetzner to Ubuntu 26.04.1 (done 2026-10-27; ADR 0009). The runbook and outcome are in the internal ops notes.

### 5.8 hm-prod-1: firewall and bootstrap (server created 2026-10-27; approved)

hm-prod-1 was bootstrapped with `deploy/compose/bootstrap.sh` (admin user, key-only SSH, host nftables firewall with a dead-man rollback, Docker, Tailscale, Ubuntu Pro), joined the tailnet as a tagged node that can reach nothing, got break-glass WireGuard (`deploy/breakglass/`), and had public SSH closed (2026-10-28). The Hetzner Cloud Firewall `hm-prod-1-fw` is applied as the outer layer (2026-10-29): inbound TCP 80/443, UDP 3478/41641/51820 and ICMP only; tailnet and break-glass SSH verified afterwards. Open: bringing the server and firewall under OpenTofu, which waits for the R2 state bucket (§13.2). Addresses and status details are in the internal ops notes.

---

## 6. Track A — Agent sandbox (blueprint step 1)

### 6.1 Topology decision
- **One container per mode.** `tailscaled` runs **inside** the container as root with `NET_ADMIN`/`NET_RAW`. Every agent-facing process runs as uid/gid **1000** (user `agent`) under **s6-overlay v3.2.3.2**.
- **Why not a Tailscale sidecar** with `network_mode: service:`? Compose's `depends_on.restart` covers only restarts Compose triggers. A sidecar crash restarted by Docker leaves the sandbox on a dead network namespace, silently.
- **Why not host networking?** The container would inherit the host's tailnet identity and ACLs, plus any other VPN the host runs. **Forbidden.**
- **Why s6 rather than one Compose service per entry point?** Entry points must share processes and tmux sessions, so `status` can see running tasks across all of them. s6 still restarts each one independently, with backoff.

### 6.2 Image (`sandbox/image/Dockerfile`, multi-stage, published as `ghcr.io/headmesh-dev/sandbox:<ver>`, signed)
- **Base:** `ubuntu:26.04@sha256:<pinned>`, matching the host OS and libc (pitfall 5).
- **apt packages:** ca-certificates, curl, git, git-lfs, openssh-client, tmux, ripgrep, fd-find, jq, build-essential, pkg-config, libssl-dev, python3, python3-venv, python3-dev, sqlite3, postgresql-client, redis-tools, iproute2, nftables, iptables, dnsutils, iputils-ping, netcat-openbsd, procps, lsof, less, nano, unzip, xz-utils, zstd, locales, tzdata, gnupg, util-linux (`setpriv`).
- **Tailscale:** `COPY --from=tailscale/tailscale:v1.104.2@sha256:<pinned> /usr/local/bin/{tailscale,tailscaled}`. These are static Go binaries.
- **s6-overlay:** noarch and x86_64 tarballs, checked against sha256.
- **Toolchains:** **mise** at a pinned release, verified by checksum. `/etc/mise/config.toml` pins node 24 LTS, python 3.14, uv 0.12, go 1.26, pnpm, gh and rust (optional). Data lives in the `/cache/mise` volume.
- **Agent CLIs:**
  - Claude Code: native binary pinned to **2.1.293** from `downloads.claude.ai/claude-code-releases/<ver>/linux-x64/claude`, verified by checksum.
  - Codex: `@openai/codex@0.164.0`.
  - Gitleaks: **8.30.1**, built from the pinned Go module with Go 1.26.8 so the embedded TLS standard library includes the critical fix.
  - OpenAI tunnel: `tunnel-client` from `ghcr.io/openai/tunnel-client@sha256:<pinned>`.
  - Renovate bumps all three.
- **Headmesh binaries:** `headmesh-sandboxd` and `headmesh`, built in a Go stage with `CGO_ENABLED=0`, `-trimpath` and reproducible flags.
- **User:** `agent`, UID/GID taken from build args (default 1000/1000), `HOME=/home/agent`. **No sudo.** `PATH` includes mise shims in `/etc/profile.d/headmesh.sh`, so login shells (SSH) find `codex` and `claude`.
- **Locale and timezone:** `en_US.UTF-8`, `TZ=America/Toronto`.
- **GPU:** injected through CDI at runtime; no CUDA in the base image. An optional `-cuda` tag built `FROM nvidia/cuda:<13.x>-runtime-ubuntu26.04` if that image exists (**SPIKE T-15**).
- **Packaging:** OCI labels; `HEALTHCHECK CMD headmesh-sandboxd health --timeout 5s`.
- **Build checks:** `docker buildx bake` with registry cache, then hadolint, container-structure-test (users, permissions, no setuid binaries except allowed ones, no sudo), and trivy (fails on critical).

### 6.3 Compose (`sandbox/compose/compose.yaml`) — skeleton

```yaml
name: headmesh-sandbox
x-base: &base
  image: ghcr.io/headmesh-dev/sandbox:${HEADMESH_SANDBOX_VERSION:?}
  hostname: ${HEADMESH_SANDBOX_NAME:-asus-sandbox}
  init: false                      # s6 is PID 1
  cap_drop: [ALL]
  cap_add: [NET_ADMIN, NET_RAW, SETUID, SETGID, CHOWN, DAC_OVERRIDE, KILL]   # tailscaled + s6-setuidgid only
  security_opt: ["no-new-privileges:true"]
  devices: ["/dev/net/tun:/dev/net/tun", "nvidia.com/gpu=all"]               # CDI; dropped when gpu = "off"
  networks: [hm-sbx]
  stop_grace_period: 90s           # runner needs >= 80 s to drain
  ulimits: { nofile: { soft: 65536, hard: 65536 } }
  mem_limit: ${HEADMESH_SANDBOX_MEM:-10g}
  cpus: ${HEADMESH_SANDBOX_CPUS:-6}
  logging: { driver: local, options: { max-size: "20m", max-file: "5" } }
  environment: [HEADMESH_MODE, TS_LOGIN_SERVER, TS_HOSTNAME, TS_TAGS, TZ]
  secrets: [ts_authkey, github_token, claude_env_secret, openai_tunnel_key, sandboxd_tokens]
services:
  sandbox:                         # Regular (default)
    <<: *base
    profiles: [regular]
    restart: unless-stopped
    volumes: !override [...generated from sandbox.toml regular set...]
  sandbox-yolo:                    # YOLO (opt-in, never restarts by itself)
    <<: *base
    profiles: [yolo]
    restart: "no"
    volumes: !override [...generated yolo set incl. host root ro (bind.recursive: readonly) + tmpfs/dev-null masks...]
networks:
  hm-sbx: { driver: bridge, ipam: { config: [{ subnet: 172.31.250.0/24 }] },
            driver_opts: { com.docker.network.bridge.enable_icc: "false", com.docker.network.bridge.name: "hm-sbx0" } }
secrets: { ts_authkey: { file: ${HEADMESH_SANDBOX_ROOT}/secrets/ts_authkey }, ... }
volumes: { hm-cache-mise: {}, hm-cache-uv: {}, hm-cache-npm: {}, hm-cache-pnpm: {}, hm-cache-go: {}, hm-cache-cargo: {} }
```
- **No published ports.** sandboxd is reachable only over the tailnet (and from the host via its unix socket).
- `headmesh sandbox up` refuses to start one profile while the other is running.

### 6.4 Mounts, config and secret masking
- **Config** lives in `${HEADMESH_SANDBOX_ROOT:-/mnt/storage/headmesh/sandbox}/config/sandbox.toml`, validated against a JSON Schema.
- `headmesh sandbox render` produces `compose.override.generated.yaml`. That makes mounts and tags easy to widen or narrow with a diff preview.
- **Host paths are mirrored** inside the container (`/mnt/storage/projects/...` at the same path). Paths then match Remote Control on the host, logs and tool configs.
- **Regular mode:**
  - Read-write: project folders, for example `/mnt/storage/projects/{xcelsior,headmesh,…}` (a list).
  - Read-only: data folders.
  - Agent home: `${root}/home` → `/home/agent`.
  - State: `${root}/state/tailscale-regular` → `/var/lib/tailscale`.
  - Also bound: sessions, artifacts, runner and logs directories.
- **YOLO mode:**
  - `/` → `/host` read-only with `bind.recursive: readonly`. This fails closed if the kernel can't do recursive read-only mounts (kernel 7.0 can).
  - `/home/aaryn` and project folders read-write at their host paths.
  - System folders stay read-only.
  - Separate state: `${root}/state/tailscale-yolo`.
- **Secret denylist (enforced in both modes, refused by `render` unless listed under `allow_secrets`):**
  - Home dotfiles: `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.kube`, `~/.docker`, `~/.password-store`, `~/.local/share/keyrings`, `~/.config/gh`, `~/.netrc`, `~/.pgpass`, `~/.claude/.credentials.json`, `~/.codex/auth.json`, `~/.config/restic`.
  - System and tailnet: `/etc/headscale`, `/etc/wireguard`, `/var/lib/tailscale*`, `/var/lib/headscale`, `/var/backups/headscale` (contains the **Noise private key**), `/root`, `/etc/shadow`, `/etc/ssh/ssh_host_*`.
  - In YOLO mode these are masked with an empty tmpfs (directories) or `/dev/null` binds (files) layered over the read-only `/host` bind.
- **Project secret files:** `headmesh sandbox scan-secrets` runs a gitleaks directory scan plus filename heuristics (`.env*`, `*.pem`, `id_*`, `*.key`, `credentials*.json`) over the read-write mounts. It proposes `/dev/null` file masks; Aaryn reviews them and they're stored in `sandbox.toml`. For example, it masks `xcelsior/.env`, `.env.staging`, `.env.staging.secrets` and `.env.worker.staging`, which hold prod secrets.
- **SPIKE T-7:** confirm that mask mounts apply over a recursive read-only parent bind, and that Docker orders mounts by path depth.

**Persistence matrix**

| Item | Container stop/start | Recreate (`up --force-recreate`) | Host reboot |
|---|---|---|---|
| Bind-mounted projects/home/state/sessions logs | ✓ | ✓ | ✓ |
| Tailscale identity (`/var/lib/tailscale` bind) | ✓ | ✓ | ✓ |
| Package caches (named volumes) | ✓ | ✓ | ✓ (lost on `volume prune`/`down -v` — acceptable) |
| Container rootfs edits | ✓ | ✗ | ✓ (if not recreated) |
| Running processes / tmux sessions | ✗ | ✗ | ✗ — reported as `lost` via boot_id change |

### 6.5 Host egress firewall (new; the blueprint missed it)
- A bridge container can reach host services and the LAN through the Docker gateway: Postgres 16 on the ASUS, Xcelsior ports, Redis, router admin.
- A `headmesh-sandbox-firewall.service` (root, oneshot) installs an nftables table `inet headmesh_sbx` on `DOCKER-USER`. For traffic from `172.31.250.0/24`:
  - **Allow:** internet; UDP 41641–41650 to the LAN (WireGuard direct paths); Docker DNS.
  - **Deny:** RFC1918 ranges, 100.64.0.0/10 on the host side, 169.254.0.0/16, the host gateway, and the ASUS's own addresses.
- This coexists with `xcelsior-egress-guard`. Order and chains are documented and tested from inside the container with `nc` probes.
- YOLO can opt into LAN access with `yolo.allow_lan = true`.

### 6.6 Tailnet enrollment (M1, on the existing `xcelsior` cell; prod ACL change needs approval)
- **Node identities:**
  - Regular: `asus-sandbox`, `tag:sandbox`.
  - YOLO: `asus-sandbox-yolo`, `tag:sandbox-yolo`.
  - Each has its own state directory and persistent identity.
- **Key handling:**
  - Use a **single-use tagged pre-auth key**. Tagged nodes never expire, so no expiry setting is needed (pitfall 2 is covered).
  - Keep a 24 h reusable key only for re-enrollment drills.
  - `tailscale up` runs with the auth key **only when no state exists** (the TS_AUTH_ONCE behaviour).
- **Flags:** `tailscale up --login-server=https://hs.xcelsior.ca --auth-key=file:/run/secrets/ts_authkey --hostname=$TS_HOSTNAME --advertise-tags=$TS_TAGS --accept-dns=true --accept-routes=false --ssh --netfilter-mode=on`.
- **Regular-mode reach (`sandbox.toml [mode.regular] reach`):** Aaryn picks an explicit list of `host:port` entries, for example `tower-server:11434` (Ollama), the prod-hetzner Grafana/Loki ports read-only, and the Mac NFS. Default: **no prod database, no provider hosts**.
- **DNS (pitfall 3):** tailscaled rewrites the container's `resolv.conf` to `100.100.100.100` with search domain `mesh.xcelsior.ca`. The oneshot `dns-check` service fails startup health if `getent hosts prod-hetzner.mesh.xcelsior.ca` fails while `tailscale ping 100.64.0.1` succeeds. That's the classic "IP works, names fail" symptom, and the error explains the fix.
- **SPIKE T-8:** `read_only: true` rootfs alongside tailscaled's `resolv.conf` writes. If it fails, keep the rootfs writable.

### 6.7 s6-rc services (`sandbox/image/rootfs/etc/s6-overlay/s6-rc.d/`)

| Service | Type | User | Command / notes | Depends on |
|---|---|---|---|---|
| `tailscaled` | longrun | root | `tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock --tun=hm0 --port=41641`; readiness when LocalAPI answers | — |
| `tailscale-up` | oneshot | root | Auth-once logic above; never `--reset` or `--force-reauth` | tailscaled |
| `dns-check` | oneshot | agent | MagicDNS check with diagnostic | tailscale-up |
| `guidelines` | oneshot | agent | Renders `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` and the shell banner with the **active mode** and the multi-agent protocol | — |
| `tmux` | longrun | agent | `tmux -D -L headmesh -f /etc/headmesh/tmux.conf` (foreground server, tmux 3.6) | — |
| `sandboxd` | longrun | agent | `headmesh-sandboxd serve --listen :8780 --unix /run/headmesh/sandboxd.sock` | tmux, dns-check |
| `claude-runner` | longrun | see §7.1 | `claude self-hosted-runner …` (exits after sessions; s6 restarts with backoff) | sandboxd |
| `claude-rc` | longrun | agent | Remote Control in a tmux window (needs a TTY) | tmux |
| `openai-tunnel` | longrun | agent | `tunnel-client run --profile sandbox` | sandboxd |
| `yolo-ttl` | longrun | root | YOLO only: after TTL, audit and `/run/s6/basedir/bin/halt` | — |
| `codex-exec` | on demand (no long-running service) | agent | Started per session by the host orchestrator through `docker exec`; it exits with the session (§7.6). `status` lists live executors | sandboxd |

- Logs go through `s6-log` into `${root}/logs/<svc>` (rotate at 10 MB, keep 10).
- Settings: `S6_BEHAVIOUR_IF_STAGE2_FAILS=2`, `S6_KILL_GRACETIME=80000`, `S6_CMD_WAIT_FOR_SERVICES_MAXTIME=120000`.

### 6.8 `headmesh-sandboxd` — the MCP server and session manager

**Transport**
- Streamable HTTP at `/mcp`. Serves **2026-07-28 stateless** (`StreamableHTTPOptions.Stateless=true`, `server/discover`) and also negotiates **2025-11-25** for older clients.
- `Origin` validation; request body limit 10 MiB.
- Also exposed on the unix socket `/run/headmesh/sandboxd.sock`, used by the host CLI through a bind-mounted run directory.

**Auth**
- **M1:** bearer tokens. 32-byte random values with prefix `hms_`, stored as argon2id hashes in `secrets/sandboxd_tokens`, one per client (claude-code-mac, chatgpt-tunnel, …), revocable.
- **M3:** OAuth 2.1 resource server. Validates Headmesh authorization-server JWTs (EdDSA) through JWKS, with audience = this sandbox's resource URL (RFC 8707). Serves `/.well-known/oauth-protected-resource` (RFC 9728) and a `WWW-Authenticate` challenge with `resource_metadata` and `scope`.
- Scopes: `sandbox:read`, `sandbox:exec`, `sandbox:write`, `sandbox:admin`.

**Agent-independent state**
- Sessions, processes and artifacts are keyed by sandboxd's own IDs and stored under `${root}/sessions`. Nothing depends on MCP sessions, so they survive client disconnects and sandboxd restarts.

**Tools** (annotations: `readOnlyHint` / `destructiveHint` / `idempotentHint`; every response says whether it was truncated and how to fetch the rest)
- **Status:**
  - `sandbox_status`: mode, `boot_id`, `container_started_at`, `sandboxd_started_at`; tailnet (IP, MagicDNS name, online, DERP/direct path, reachability of configured targets); entry-point health; sessions; active agents per repo; git summary; GPU; cache disk usage.
- **Exec:**
  - `exec{command, cwd, env, timeout_s≤3600 (≤110 on the tunnel route), stdin?}` → exit code, stdout/stderr tail (64 KB each, full log in `output_ref`), duration. Runs `bash -lc` in a new process group; a timeout kills the group.
- **Interactive sessions:**
  - `session_create{name?, cwd, shell, env, cols, rows}`, `session_send{id, input, enter?, keys?}`, `session_read{id, offset, max_bytes, wait_ms≤30000}` (long poll) → `{data, next_offset, alive, exit_code?}`.
  - Also `session_list`, `session_kill{signal}`, `session_resize`.
  - Backed by **tmux** windows with `remain-on-exit on` and `pipe-pane -o` to an append-only log, plus an exit-code file written by a wrapper.
  - Aaryn can watch live with `headmesh sandbox attach` (`tmux -L headmesh attach`).
- **Background processes:**
  - `process_start{command, cwd, name}` (detached, setsid, logged), `process_list`, `process_logs{tail, since}`, `process_stop`.
- **Files:**
  - `fs_read{path, offset, limit}`: line-numbered; binary detection; 2 MB cap.
  - `fs_write{path, content, mode?}`: atomic temp+rename; preserves mode/ownership; refuses read-only mounts with a clear error.
  - `fs_edit{path, old_string, new_string, replace_all?}`: exact match; fails if not unique.
  - `fs_list{path, depth, glob}`, `fs_search{pattern, path, glob, regex}` (ripgrep JSON), `fs_stat`, `fs_move`.
  - `fs_delete`: moves to `<mount>/.headmesh-trash/<ts>/` for recovery.
- **Git:**
  - `git_status{repo}`: porcelain v2, ahead/behind, other active agents.
  - `git_sync{repo}`: `fetch` + `pull --rebase --autostash`. On conflict it reports and stops; it **never** force-pushes.
  - `git_push_safe{repo}`: rebase on `origin/main`, run hooks, push. Refuses non-fast-forward.
- **Artifacts:**
  - `artifact_put{name, content|path}`, `artifact_list`, `artifact_get`. Stored in `${root}/artifacts` and served over the tailnet with HMAC-signed URLs that expire in 1 h.
- **Network:**
  - `net_check{target, port}`: DNS (MagicDNS vs system), `tailscale ping` (direct vs DERP), TCP connect, latency.
- **Agents:**
  - `agents_register{agent, task, repos}`, `agents_heartbeat`, `agents_list`.

**Resources and prompts**
- Resources: `sandbox://status`, `sandbox://mounts`, `sandbox://guidelines`, `sandbox://sessions/{id}/log`.
- Prompt: `multi-agent-protocol`. The MCP `instructions` field states the active mode.

**Audit and limits**
- Every call is appended to `${root}/logs/audit.jsonl`: ts, client, tool, args digest, mode, duration, result code. Forwarded to Headmesh audit from M3.
- Optional gitleaks-based redaction of tool output.
- Per-client concurrency limit of 8 and rate limit of 600/min.

**"Did processes survive?"**
- `boot_id` (`/proc/sys/kernel/random/boot_id` combined with the container start time) is stored with each session.
- On reconnect, `sandbox_status` reports each session as `running`, `exited(code)` or `lost (container restarted at …)`.

### 6.9 Git and GitHub inside the container
- **Global `~/.gitconfig`:**
  - Identity: `user.name`/`user.email` from `sandbox.toml` (Aaryn's GitHub-verified address).
  - Behaviour: `pull.rebase=true`, `rebase.autoStash=true`, `fetch.prune=true`, `push.autoSetupRemote=true`, `rerere.enabled=false`.
  - Hooks: `core.hooksPath=/etc/headmesh/git-hooks`.
  - Remotes: `url."https://github.com/".insteadOf git@github.com:` in Regular mode.
- **Hooks:**
  - `pre-commit`: `gitleaks protect --staged`.
  - `commit-msg`: adds the trailer `Agent: <entrypoint>/<session>` so agent commits are attributable.
  - `pre-push`: refuses non-fast-forward pushes to `main`/`master`, and refuses while a rebase or merge is in progress. This hook is the client-side "never force-push main".
- **Credentials:**
  - **Regular:** GitHub **fine-grained PAT** scoped to chosen repos (Contents RW, Pull requests RW, Metadata R), 90-day expiry. Served from `/run/secrets/github_token` by a credential helper and `GH_TOKEN`.
  - **YOLO:** a dedicated `ed25519` key `id_ed25519_sandbox_yolo`, added to GitHub as "headmesh-sandbox-yolo (asus)". Mounted **only** in YOLO and pinned with `known_hosts`. The plan notes the **trade-off**: Aaryn's personal key would break his own logins if revoked and widen blast radius; a dedicated key is one-click revocable.
- **Server side (needs a GitHub plan that supports it):** a ruleset on `main` that blocks force-pushes and deletion.

### 6.10 Concurrency protocol (no hard lock by default)
- **Guidelines** (`/etc/headmesh/agent-guidelines.md`), rendered into `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` and the MCP instructions. When told another agent is active, an agent:
  - sticks to its own files;
  - makes small, frequent commits;
  - runs `git pull --rebase` before every push;
  - does no reformatting or mass edits of files it isn't working on;
  - asks Aaryn rather than overwrite changes it didn't make;
  - works on main by default; branches or worktrees only when asked.
- **Registry:** a Claude `SessionStart` hook and the guidelines' first step call `agents_register`. `sandbox_status` and the session banner show "1 other agent active in /mnt/storage/projects/xcelsior (claude-runner, task …)".
- **Optional lock:** `repo_lock = "off" | "advisory" (default) | "enforce"`. `enforce` makes the `pre-commit` hook refuse when another live agent holds `.git/headmesh-agent.lock` (heartbeat under 2 minutes).

### 6.11 Modes: Regular (default) and YOLO (opt-in)
- **CLI:**
  - `headmesh sandbox up` always starts **Regular**.
  - `headmesh sandbox up --yolo --ttl 4h` first runs these preflights:
    - a restic snapshot of `/home/aaryn` under 24 h old (offers `headmesh sandbox backup`);
    - an interactive confirmation that Aaryn is watching, with a warning against ingesting untrusted content (web pages, public issues) because of prompt injection.
  - Then it stops Regular, starts `sandbox-yolo` (`restart: "no"`) and records an audit entry.
  - `yolo-ttl` halts the container at expiry. **The next `up` falls back to Regular.**
- **Mode visibility:** shown in `status`, the shell `PS1` (`[YOLO]` in red), MCP `instructions`, the Claude `SessionStart` banner and `AGENTS.md`.

### 6.12 Backups (YOLO prerequisite)
- restic repository `/mnt/storage/backups/restic-home`. Password file `/etc/headmesh/restic.pass` (root 0400, masked from the sandbox).
- Nightly `headmesh-home-backup.timer`. Retention: `--keep-daily 7 --keep-weekly 4 --keep-monthly 6`.
- Monthly `restic check --read-data-subset=5%`.

### 6.13 GPU (pitfall 6)
- CDI device `nvidia.com/gpu=all` (already present).
- `sandbox.toml gpu = "off" | "shared"`. **Default is `off` while `xcelsior-worker` is serving rentals**, to avoid VRAM contention on the 6 GB RTX 2060.
- `status` shows GPU processes from both sides. Switching requires a container restart.

### 6.14 Diagnostics
- `headmesh sandbox status [--json]` (host) and `hm status` (inside) report:
  - mode and TTL;
  - container and s6 service states, plus per-entry-point health (runner registered, Remote Control connected, tunnel `doctor`, exec-server n/a);
  - tailnet: node, IP, MagicDNS, DERP region, direct/relayed per target;
  - DNS check; running sessions and processes; active agents; repo states; GPU; cache disk usage; last backup age.
- `headmesh sandbox doctor` checks, each with a remediation hint:
  - TUN device, NET_ADMIN, MagicDNS, clock skew (runner auth fails beyond 5 minutes), MTU, UDP reachability;
  - firewall rules present, masks in place, mounts ro/rw as declared;
  - secrets file permissions, restic age, sleep masked.

### 6.15 Acceptance harness (`sandbox/tests/acceptance`)

**The five checks** (each route writes a JSON result into `${root}/artifacts/acceptance/<route>-<ts>.json`; a host verifier cross-checks it)
1. **Run a command:** `id -u` = 1000, `uname -a`, `cat /etc/os-release` shows 26.04.
2. **Read a real project file:** sha256 of `/mnt/storage/projects/xcelsior/README.md` equals the host's hash.
3. **Use a project dependency:** `cd /mnt/storage/projects/xcelsior && uv run --frozen python -c "import fastapi, psycopg; print(fastapi.__version__)"` (cache warm).
4. **Reach a private service by MagicDNS name:** `getent hosts prod-hetzner.mesh.xcelsior.ca` plus a TCP connect to a configured allowed target (for example `tower-server.mesh.xcelsior.ca:11434`), and a negative probe to a denied target.
5. **Disconnect and reconnect:** create session S with a marker process and file; drop the client; reconnect. S is `running`, the marker exists, and the node ID and IP are unchanged.

**Routes (M1)**
- **(a) MCP:** automated Go client over the MagicDNS URL with a bearer token. Runs in CI on the ASUS self-hosted runner, `workflow_dispatch` only.
- **(b) Claude self-hosted runner:** `claude --environment ccpool_… "Run the Headmesh acceptance prompt"`. The prompt file is in the repo; the session writes the JSON.
- **(c) Codex over SSH remote connection:** Codex desktop on the Mac connects to `agent@asus-sandbox.mesh.xcelsior.ca` and runs the acceptance prompt. Plus a scripted `ssh agent@asus-sandbox … headmesh sandbox acceptance --route ssh`.
- **(d) OpenAI Secure MCP Tunnel:** from ChatGPT, a custom MCP connection over the tunnel performs the tool calls.
- **(e) Codex `exec-server` (Agents API):** `headmesh agent run --provider openai --repo xcelsior --prompt-file sandbox/tests/acceptance/prompt.md`. The session runs inside the sandbox through the executor and writes the JSON.

**Bonus:** `headmesh sandbox recreate` (`down` + `up --force-recreate`). Files, Tailscale node ID and IP, and the `~/.bash_history` marker all persist, and sessions are reported as `lost` with the new `boot_id`.

**Report:** `docs/acceptance/2026-xx-xx.md` with a pass/fail table per route.

---

## 7. Track B — Cloud entry points

### 7.1 Claude self-hosted runner (Claude cloud tasks)
- **Process:** `claude self-hosted-runner --environment-secret-file /run/secrets/claude_env_secret --base-dir /srv/runner --capacity 1 --health-port 8081 --client-label asus-sandbox --hooks-dir /etc/headmesh/runner-hooks --push-outcome-on-release --kill-session-after-min 240 --configure-git`.
  - **Not** `--use-anthropic-git-proxy`: it deletes and replaces `~/.gitconfig`.
- **Checkout hook:** maps `owner/repo` to the shared working copy declared in `sandbox.toml [repos]`. For example `aabiro/xcelsior` → `/mnt/storage/projects/xcelsior`; it symlinks `/srv/runner/aabiro/xcelsior` to it.
  - It ensures the tree is on `main` and fetched, so agents work **directly on main** as the blueprint requires.
  - Unmapped repos fall back to a fresh clone under `/mnt/storage/headmesh/sandbox/runner-work`.
  - The runner's default per-session branch is overridden by the guidelines (commit on main and push `main` unless Aaryn asks for a branch).
  - The claude.ai "Changes" card needs a pushed branch to diff against, so the plan documents that **diffs appear after a push**. When Aaryn wants a reviewable diff, a branch or PR can be requested per task.
- **Session MCP servers:** baked into `~/.claude.json` at user scope: local sandboxd (unix-socket HTTP) and Xcelsior (remote OAuth).
- **Hooks:** a `SessionStart` hook prints the banner and registers the agent; `post-session` unregisters.
- **Secret exposure (SPIKE T-1):** the docs say any session can read the environment secret.
  - **Preferred:** the runner runs as root inside the container with the secret at 0400, and `--exec-path /usr/local/bin/claude-session` runs `setpriv --reuid=1000 --regid=1000 --init-groups --inh-caps=-all claude "$@"`, so sessions run as `agent` and can't read the secret.
  - **Fallback:** accept the exposure, rotate the secret every 30 days, and alert on unknown runner registrations.
- **Graceful drain:** `stop_grace_period: 90s`, and s6 `down-signal` SIGTERM.
- **Enablement:** confirm the Team toggle (manual, §5.5) and run `claude self-hosted-runner setup` once interactively.

### 7.2 Claude Remote Control (Aaryn's interactive sessions)
- **Keep the host's existing Remote Control** as the immediate fallback and baseline.
- **Add a sandbox instance:** `tmux new-window -t headmesh -n rc 'claude remote-control --name asus-sandbox --spawn same-dir --capacity 4'`, supervised by the s6 `claude-rc` service, which recreates the window if it dies and re-runs after the 10-minute offline exit.
- **First run is interactive:** `headmesh sandbox shell` → `claude auth login` (full claude.ai login, not `setup-token`) → trust the workspace → answer `Enable Remote Control? y`.
- `~/.claude` and `~/.claude.json` persist in the bind-mounted agent home.
- **SPIKE T-2:** confirm restart behaviour without a TTY once trust and enablement are persisted.

### 7.3 Codex: SSH remote connections (primary Codex front door, M1)
- **Server:** the sandbox's own `tailscaled --ssh`, so SSH **terminates inside the container** as user `agent`. The host's own node gets **no** SSH rule.
- **Policy:** `ssh: [{action: "accept", src: ["xcelsior@"], dst: ["tag:sandbox", "tag:sandbox-yolo"], users: ["agent"]}]`.
  - This depends on §5.2, after which `xcelsior@` = **Aaryn's personal devices only**, because providers are tagged.
  - Optionally narrow `src` to specific device IPs, or use `action: "check"` for re-auth.
- **Codex desktop:** add the remote host `agent@asus-sandbox.mesh.xcelsior.ca`. `codex` is on the login-shell `PATH` (`/etc/profile.d/headmesh.sh`).
- **SPIKE T-3:** Codex desktop works with Tailscale SSH. Fallback: OpenSSH running as uid 1000 on `:2222`, bound to the tailnet IP, keys only, `AllowUsers agent`, `PermitRootLogin no`.

### 7.4 OpenAI Secure MCP Tunnel (ChatGPT and Codex via Platform)
- **Service:** s6 `openai-tunnel` runs `tunnel-client run --profile sandbox`. The profile in `/etc/headmesh/tunnel/sandbox.yaml` sets `mcp_server_url: http://127.0.0.1:8780/mcp` and the tunnel ID.
- **Key:** `CONTROL_PLANE_API_KEY` from a Read+Use-only key in `/run/secrets`. Health comes from `tunnel-client doctor`; admin UI on loopback only.
- **Auth:** the OAuth authorization server can't be tunneled, so sandboxd authenticates tunnel traffic with a dedicated **bearer token** (`api_key`, `headerScheme: bearer` in the plugin's portable `mcp.json`).
- **Limits:** responses are capped at 8 MiB and `exec` timeouts at 110 s on this route (tunnel: 10 MiB / 120 s / 600 rpm).
- **Hookup:** ChatGPT: Plugins → Add custom MCP server → Connection: Tunnel. Codex: `tunnel-client codex plugin install`.

### 7.5 Headmesh ingress for any MCP client (M3)
- **URL:** `https://asus.sbx.headmesh.dev/mcp` → Caddy on hm-prod-1 (wildcard certificate via DNS-01 Cloudflare, rate limits, 10 MiB body cap) → over the tailnet → `asus-sandbox:8780`.
- hm-prod-1 joins the `xcelsior` cell as `tag:hm-ingress` (a second tailscaled instance on the VM, like Xcelsior's dual-instance pattern).
- Grant: `tag:hm-ingress → tag:sandbox:8780` only.
- The sandbox makes **no inbound connections**; WireGuard does the transport.
- This becomes the product feature `headmesh expose` (Headscale has no Funnel).
- OAuth via the Headmesh authorization server (M3) lets Claude.ai custom connectors use it.

### 7.6 `codex exec-server` (M1, alongside SSH and Tunnel; no extra Hetzner cost)
- **What it gives you:** Codex driven through OpenAI's **Agents API**, executing inside the sandbox with the same files, packages, tailnet and git setup. It suits scripted or unattended Codex tasks started from the CLI, cron, CI or Xcelsior.
- **What it doesn't give you:** it does **not** make chatgpt.com/codex cloud tasks run on your machine. OpenAI doesn't document that, which is why SSH stays the primary interactive Codex route. Results show up in the CLI stream and artifacts, and as commits and pushes from the session; OpenAI has no diff UI for this path.
- **Prerequisites (Aaryn, Platform dashboard):**
  - an **application key** with `api.agents.read`, `api.agents.write` and `api.responses.write`, stored on the host at `/etc/headmesh/openai-app.key` (0400), **never** inside the sandbox;
  - an **environment key** (Platform → Agents → Environments → Keys, every other permission None).
- **Host-side orchestrator:** `headmesh agent run --provider openai --repo … --prompt …`.
  - It holds the Agents API **application key outside the sandbox**, as OpenAI requires.
  - It creates a session with `environment: {type: "self_hosted", workspace_directory: "/mnt/storage/projects/<repo>"}` and waits for `environment.pending`.
  - Then: `docker exec -u agent headmesh-sandbox codex exec-server --remote <remote_url> --environment-id <id>` with `CODEX_API_KEY=<restricted environment key>` passed through stdin or a tmpfs file.
  - It streams session events to the terminal and artifacts; the executor exits with the session.
- **SPIKE T-16:** run the executor as non-root in the container (`--linux-sandbox-pid-namespace inherit` if isolation needs privileges), and pin the CLI version that supports it.

### 7.7 Unified status
- Each entry point exposes health (runner `:8081/healthz`, tunnel `doctor`, Remote Control tmux pane state, Tailscale SSH).
- sandboxd aggregates these into `sandbox_status` and the CLI. One command shows connected entry points, mode, Headscale connectivity and running tasks.

---

## 8. Track C — Plugins and marketplace (one repo serving both ecosystems)

### 8.1 Catalogs
- **`.claude-plugin/marketplace.json`:**
  ```json
  { "name": "headmesh", "owner": { "name": "Headmesh", "email": "hello@headmesh.dev" },
    "description": "Headmesh and Xcelsior plugins for Claude Code and Codex",
    "plugins": [
      { "name": "headmesh", "source": "./plugins/headmesh", "description": "Agent sandboxes on your own machines over a private Headscale mesh", "category": "infrastructure" },
      { "name": "xcelsior", "source": "./plugins/xcelsior", "description": "Rent and manage GPUs on Xcelsior Compute", "category": "infrastructure" } ] }
  ```
- **`.agents/plugins/marketplace.json`** (Codex-native):
  ```json
  { "name": "headmesh", "interface": { "displayName": "Headmesh" },
    "plugins": [
      { "name": "headmesh", "source": { "source": "local", "path": "./plugins/headmesh" }, "category": "Developer Tools",
        "policy": { "installation": "AVAILABLE", "authentication": "ON_USE" } },
      { "name": "xcelsior", "source": { "source": "local", "path": "./plugins/xcelsior" }, "category": "Developer Tools",
        "policy": { "installation": "AVAILABLE", "authentication": "ON_USE" } } ] }
  ```

### 8.2 Plugin contents (both plugins)
- **Manifests:**
  - `.claude-plugin/plugin.json`: name, displayName, version, description, author, homepage, repository, license, keywords, `userConfig`.
  - Root portable `plugin.json`: Agent Plugins 1.0.0 `$schema`, with `extensions["com.openai"].interface` (displayName, shortDescription, developerName, category, websiteURL, privacyPolicyURL, termsOfServiceURL, defaultPrompt, brandColor, logo).
  - `.codex-plugin/plugin.json` fallback, scaffolded by `$plugin-creator` (**SPIKE T-12**: confirm which one Codex picks; keep both in sync with a generator script `plugins/gen.go` from one source YAML).
- **MCP definitions:**
  - Claude `.mcp.json`. Portable `mcp.json` with `type: "streamable-http"` and auth under `extensions["com.openai"].auth`.
- **Skills:** `skills/*/SKILL.md`, shared by both tools.
- **Claude-only:** `commands/`.
- **Excluded:** `bin/` (claude.ai refuses it) and hooks (they block Codex directory listing).

**`plugins/headmesh`**
- `userConfig`:
  - `sandbox_url` (string, default `https://asus.sbx.headmesh.dev/mcp`);
  - `sandbox_token` (string, **sensitive**, stored in the keychain).
- `.mcp.json`: `{"mcpServers":{"headmesh-sandbox":{"type":"http","url":"${user_config.sandbox_url}","headers":{"Authorization":"Bearer ${user_config.sandbox_token}"}}}}`. From M3, an OAuth variant (`oauth` block, CIMD) replaces the token.
- Skills:
  - `headmesh-sandbox`: tools, persistence semantics, how to report "processes lost".
  - `multi-agent-git`: the §6.10 protocol.
  - `headmesh-network`: enrolling devices and granting temporary access with the CLI/API (M2+).
- Commands: `/sandbox-status`, `/sandbox-acceptance`.

**`plugins/xcelsior`**
- `.mcp.json`: `{"mcpServers":{"xcelsior":{"type":"http","url":"https://mcp.xcelsior.ca/mcp"}}}`. OAuth is discovered through PRM; CIMD is preferred, with DCR as fallback.
- Portable `mcp.json`: `auth.type: "oauth"`, `client.mode: "cimd"`.
- Skills:
  - `xcelsior-gpu-rental`: guardrail sequence `list_available_gpus` → `should_i_run_this` → `create_instance` (plan) → explicit user confirmation → `watch_instance`. Destructive tools always need confirmation.
  - `xcelsior-provider`: provider onboarding, earnings, payouts.
- Commands: `/gpu-prices`, `/my-instances`.

### 8.3 Xcelsior-side fixes (Xcelsior repo)
1. **Scope gap.** Either drop `billing:write`, `ssh:read`, `providers:write` and `reputation:write` from PRM `scopes_supported` (so clients don't ask for scopes they can't get), **or** extend `CONNECTOR_ALLOWED_SCOPES` with explicit consent copy. **Recommendation:** extend, for `ssh:read` only (read-only). Keep the money-moving writes off connectors.

   Test: `tests/test_advertised_scopes_match_the_connector_ceiling.py`.
2. Make sure the plugin uses the canonical audience `https://mcp.xcelsior.ca/mcp`, since the legacy audience sunsets on 2026-11-30.
3. Add a Codex section to `api/docs/mcp-quickstarts.md`. Fix the stale `.vscode/mcp.json` path (`mcp/` → `api/mcp/`).

### 8.4 Validation and release
- **CI `plugins.yml`:**
  - `claude plugin validate --strict .` and `claude plugin validate --strict plugins/*`.
  - ajv validation against the agent-plugins.org 1.0.0 schemas for portable `plugin.json` and `mcp.json`.
  - A check that the manifest generator output matches what's committed.
  - **Install smoke test in a clean container:**
    - `claude plugin marketplace add ./ && claude plugin install xcelsior@headmesh`, then assert that `mcp__plugin_xcelsior_xcelsior__*` is listed;
    - `codex plugin marketplace add ./ --sparse .agents/plugins --sparse plugins && codex plugin add xcelsior@headmesh`.
- **Releases:** `claude plugin tag plugins/<p> --push` creates `{name}--v{version}` tags.
- **User docs:** `claude plugin marketplace add headmesh-dev/headmesh --sparse .claude-plugin --sparse plugins`, then `claude plugin install headmesh@headmesh`. The equivalent `codex plugin marketplace add headmesh-dev/headmesh …`. A team-wide `extraKnownMarketplaces` snippet.

---

## 9. Track D — Headmesh core (control layer + API)

### 9.1 Domain model (Postgres 18; UUIDv7 primary keys; `org_id` on every tenant table with a leading index)

| Table | Key columns / notes |
|---|---|
| `cells` | id, name (`xcelsior`, `hs1`), server_url, base_domain, headscale_version, mode (`observe`/`enforce`), capacity limits, agent cert fingerprint, last_seen |
| `orgs` | id, slug (DNS-safe, unique, ≤ 24 chars), short_id (10-char base32, used in tags), cell_id, parent_org_id (child orgs of a platform), is_platform, template (`team`/`locked`), settings jsonb (allow_subnet_routes=false, allow_exit_nodes=false, max_grant_ttl, ssh_enabled), status |
| `users` / `memberships` | Global users (email, passkeys); membership role ∈ owner/admin/member/viewer/billing, optional `expires_at` (contractors) |
| `devices` | id, org_id, cell_id, headscale_node_id, name, given_name (`<name>-<orgslug>`), kind (server/gpu/laptop/sandbox/agent/ingress), roles text[], ipv4, ipv6, online, last_seen, enrolled_via, status (active/quarantined/removed), external_ref |
| `services` | id, org_id, device_id, name, proto, ports (`tcp:8888`), description |
| `rules` | id, org_id, src selector (all/role/device/member), dst selector, ports, expires_at? (temporary intra-org access), version (ETag) |
| `grants` | id, grantor_org_id, grantee_org_id, src selector (grantee devices or roles), dst (device or service), ports, starts_at, **expires_at (required)**, status (scheduled/active/expired/revoked), reason, external_ref (unique per grantor, for example an Xcelsior attempt_id), created_by, revoked_at/by |
| `enrollment_tokens` | id, org_id, token_hash (SHA-256 of a 256-bit secret; shown once, prefix `hmj_`), kind (device/member), roles, max_uses, uses, ephemeral, approval_required, expires_at |
| `enrollments` | id, token_id, cell preauth key id, status (minted/registered/completed/expired/rejected), node_id, client info |
| `api_keys` | id, org_id or platform scope, hash, prefix `hmk_`, scopes, expires_at, last_used_at |
| `oauth_*` | clients (incl. CIMD cache), grants, refresh-token families with reuse detection, signing keys (EdDSA, rotated) |
| `audit_events` | bigserial id, org_id, occurred_at, actor (type/id), action, target, request_id, ip, ua, data jsonb, prev_hash, hash — **append-only** |
| `webhook_endpoints` / `webhook_deliveries` | URL, secret, event filter, status; delivery attempts |
| `policy_revisions` | id, cell_id, sha256, text, compiled_at, effective_at, applied_at, status, compiler_version, summary |
| `cell_ops` | id, cell_id, kind, args, signature, status, result, created/acked timestamps |
| `idempotency_keys` | key, principal, request hash, response, expires (24 h) |

**Defence in depth.** Postgres row-level security on tenant tables (`SET LOCAL headmesh.org_ids = …` per request), plus app-level scoping. A test asserts that every tenant table has RLS on and that cross-org reads return nothing.

### 9.2 Mapping tenancy onto Headscale (no fork; Headscale ≥ v0.29.6)
- **Every Headmesh-enrolled node is a tagged node.** Pre-auth keys carry `acl_tags` and no user, so nodes are owned by `tagged-devices`. Tagged nodes **never expire**, so pitfall 2 is gone for every Headmesh device.
- **Tags:**
  - `tag:hm-o-<org10>`: org membership.
  - `tag:hm-r-<org10>-<role>`: role.
  - `tag:hm-n-<dev10>`: per device, so grants can target exactly one machine across IPv4 and IPv6 without IP bookkeeping.
  - **SPIKE T-4:** tag length and charset limits, and whether `tagOwners` accepts `[]` for API-assigned tags. Fallback: owner = a single internal service user, `headmesh@`.
- **Adopted cells:** existing user-owned nodes (Aaryn's personal devices) keep their Headscale user. They're mapped to devices in the `aaryn-personal` org through an import file and are retagged only when Aaryn approves.
- **MagicDNS:**
  - Headmesh renames nodes to `<name>-<orgslug>` (unique per cell), giving `<name>-<orgslug>.<base_domain>`.
  - `hs1` base_domain: **`mesh.headmesh.dev`** (different from `hs1.headmesh.dev`, mirroring Xcelsior's `hs.`/`mesh.` split).
  - **No tenant data in `extra_records`**, because they're global to the cell.
- **Peer visibility:** Headscale sends a node only the peers allowed in either direction (`FilterNodesByACL`), so other tenants are invisible. Proven by the e2e suite.
- **Routes and exits:** off by default.
  - Headmesh never calls `SetApprovedRoutes` unless the org setting allows it **and** the route is within the org's own prefixes.
  - `autoApprovers` is never emitted.
  - The compiler **rejects** `autogroup:internet`, `autogroup:danger-all` and `*` in tenant rules and grants.

### 9.3 Policy compiler (`core/policy`) — pure, deterministic, property-tested
- **Input:** a consistent snapshot (REPEATABLE READ) of one cell's orgs, devices, roles, services, rules and active grants at time `t`.
- **Output:** HuJSON with sorted keys, plus its sha256. Sections:
  - `tagOwners` (every emitted tag);
  - **`grants` only** (no legacy `acls`);
  - `ssh` (orgs that opt in);
  - `nodeAttrs`;
  - **`tests`**: isolation canaries per org. `src` = the org tag; `deny` = up to K=5 sampled other-org device tags on common ports plus all platform-infrastructure tags; `accept` = the org's own template rule.

  Headscale runs `tests` on `SetPolicy` and **rejects** a failing policy, so the server enforces isolation a second time (**SPIKE T-14**: how tests treat tags with no nodes).
- **Templates:**
  - `team`: org devices ↔ org devices, all ports. Default for direct sign-ups.
  - `locked`: nothing until rules are added. Used for Xcelsior child orgs.
- **Invariants** (pgregory.net/rapid generators over random tenants, devices, rules, grants and times; plus Go fuzzing of the HuJSON writer):
  - I1: no emitted rule lets org A's devices reach org B's devices unless an active grant A→B covers **exactly** those device tags and ports.
  - I2: none of `*`, `autogroup:internet` or `autogroup:danger-all` in tenant-derived rules.
  - I3: every emitted tag has a `tagOwners` entry; no dangling tags.
  - I4: grants with `expires_at ≤ t` or `starts_at > t` are never emitted.
  - I5: compiling the same snapshot twice gives identical output.
  - I6: a removed device leaves no reference.
  - I7: per-org size bounds hold.
- **Evaluator:** an "explain" engine for `GET /access/check` and for diffing reachability matrices (needed for adopting the xcelsior cell). It reuses Headscale's own `hscontrol/policy/v2` package as a library, so semantics match exactly (**SPIKE T-5**: whether it's importable; fallback: a sidecar running `headscale policy check` plus a Headmesh-native evaluator with golden tests cross-checked against Headscale).
- **Policy timeline:** with each revision the compiler also emits up to 32 precomputed future revisions, one at each scheduled boundary (grant start or expiry) in the next 24 h. Cell agents apply them on schedule **even if the API is unreachable**. Scheduled expiries are therefore enforced during control-plane outages on cells whose Headscale is still up.
- **Performance budget:** compile a cell of 5k devices / 2k orgs / 10k grants in under 200 ms, with a benchmark in CI and a regression gate.

### 9.4 Cell agent (`headmesh-cellagent`) — how Headmesh reaches Headscale without exposing it
- **Placement:** runs **on the Headscale host**, talking gRPC over Headscale's **unix socket** (`/var/run/headscale/headscale.sock`). No Headscale API key leaves the box, and **gRPC 50443 is never exposed**.
- **Transport:** connects **outbound** to `https://api.headmesh.dev/cell.v1.CellService/Sync` (connect-go bidi stream over HTTP/2, mTLS).
  - The cell certificate is an Ed25519 cert minted at bootstrap from a one-time token and rotated every 30 days.
  - **No inbound port is opened on prod-hetzner.**
  - Fallback, **only with Aaryn's explicit approval**: Headscale gRPC with TLS bound to the tailnet IP only, plus a host/Hetzner firewall allowing just the Headmesh VM.
- **Protocol:**
  - `Hello{cell_id, agent_ver, headscale_ver, noise_pub_fpr}`
  - `Observed{nodes delta, policy_hash, health}`
  - `Desired{policy_timeline[]}`
  - `Op{id, kind ∈ CreatePreAuthKey|SetTags|RenameNode|DeleteNode|ExpireNode|SetApprovedRoutes|BackfillNodeIPs, args, sig}`
  - `OpResult`

  Ops are idempotent by `id`. Pre-auth key values go back only over the stream and are never stored by the server, which keeps only a hash and the Headscale key id.
- **Local guardrails** (they protect the production xcelsior cell even from a buggy or compromised Headmesh):
  - `CheckPolicy` runs before `SetPolicy`.
  - Invariants file `/etc/headmesh/cell-invariants.hujson` lists reachability that must survive (for example `tag:xc-control → tag:xc-host:22`) and node IDs that must never be deleted. Violating policies are refused.
  - Rate limits: at most 5 deletes per minute and at most 20% of nodes retagged per hour.
  - Last-known-good policy is kept, and `headmesh-cellagent rollback` restores it.
  - `freeze` stops applying changes; `revoke-grant <id>` and `revoke-all-grants` work locally when the API is down, from the cached grant→rule map.
  - Modes: `observe` (read-only, reports) and `enforce`.
- **Packaging:** static binary plus a hardened systemd unit (`ProtectSystem=strict`, `NoNewPrivileges`, `SupplementaryGroups=headscale`, `ReadWritePaths=/var/lib/headmesh-cellagent`), or a container next to Headscale in compose.
- **Metrics:** op latency, policy apply latency, drift events, last sync.

### 9.5 Enrollment ("one-click onboarding")
1. An admin creates a token: `POST /v1/orgs/{org}/enrollment-tokens {roles, max_uses, ttl, ephemeral, approval_required}`. The response holds the join URL `https://app.headmesh.dev/join/<token>` (an OS-detecting page) and one-liners:
   - Linux/macOS: `curl -fsSL https://get.headmesh.dev | sh -s -- join hmj_…`
   - Windows (later): `irm https://get.headmesh.dev/install.ps1 | iex`
   - npm: `npx @headmesh/cli join hmj_…`
2. The device runs `headmesh join hmj_…`, which calls `POST /v1/enroll {token, hostname, os, arch, kind}`. Rate limited per token and per IP. If approval is required, the request queues until an admin approves.
3. The server asks the cell agent for a **single-use pre-auth key** with a 5-minute expiry, `acl_tags=[org, role…, device]`, and `ephemeral` when requested. It returns `{login_server, auth_key, hostname, enrollment_id}`.
4. The CLI runs `tailscale up --login-server … --auth-key … --hostname …` in an isolated instance where needed (§10). It then calls `POST /v1/enrollments/{id}/complete {node_id}`.
5. The server verifies `node.pre_auth_key.id` through the cell agent; a background reconciler is the fallback. It renames the node to `<name>-<orgslug>`, records the device, writes an audit entry, sends the `device.enrolled` webhook and pushes SSE to the dashboard.
6. Unknown nodes (registered outside Headmesh) in an enforced cell are **quarantined** (tag `tag:hm-quarantine`, no grants) and raise an alert.

### 9.6 Grants ("fine-grained, temporary access")
- **States:** `scheduled → active → expired | revoked`, plus `extend`.
- **Rules:**
  - `expires_at` is required, with a max TTL per org or platform (default 30 days).
  - The grantor org's admin, or a platform acting for its child orgs, creates the grant.
  - The target is one device or one service (device + ports).
- **Lifecycle:** River jobs fire at boundaries. The policy timeline already covers scheduled flips. **Manual revocation** triggers an immediate compile and apply at priority.
- **SLOs:** revocation enforced on the destination node p99 < 5 s, measured by the e2e suite and the production prober.
- **Established TCP flows** must be cut after revocation (**SPIKE T-13** proves it; if not, the cell agent also expires the grantee device's session or the e2e test documents the gap).
- **Idempotency:** `external_ref` makes creation idempotent (Xcelsior passes `attempt_id`).
- **Events:** `grant.created`, `.activated`, `.extended`, `.expired`, `.revoked`, each with audit and webhook.

### 9.7 Audit log (tamper-evident)

**Status 2026-10-29:** the append-only, per-org hash chain is built and tested on Postgres 18; `GET /v1/audit-events` (paged), `GET /v1/audit-events/verify` and `headmesh audit tail|verify` are live behind the `audit:read` scope. Still to do: the signed daily anchor to R2 and NDJSON export.
- **Append-only:** the app's DB role has INSERT/SELECT only, and a trigger raises on UPDATE or DELETE.
- **Per-org hash chain:** `hash = sha256(prev_hash || canonical_json(event))`. The chain head is locked with `SELECT … FOR UPDATE` on `org_chain_heads`.
- **Daily anchor:** a signed chain-head manifest (Ed25519) goes to R2 with an object lock if the bucket supports it (**SPIKE T-17**; otherwise Backblaze B2 object lock).
- **Verification and export:** `headmesh audit verify` checks the chain. Export is NDJSON.
- **Coverage:** every enrollment, grant and revocation, every policy apply or drift, every mode switch and every API key event is logged.

### 9.8 API v1 (REST/JSON, OpenAPI 3.1 generated by Huma, committed, with an `oasdiff` breaking-change gate in CI)
- **Conventions:**
  - `Authorization: Bearer` (OAuth JWT or `hmk_` API key).
  - `Idempotency-Key` on POST.
  - Cursor pagination (`?cursor=&limit≤200`).
  - RFC 9457 errors.
  - `ETag`/`If-Match` on mutable resources.
  - Per-principal rate limits (token bucket, `RateLimit-*` headers).
  - `/v1` versioning.
  - Generated SDKs for TypeScript, Python and Go.
- **Account:** `GET /v1/me`.
- **Orgs:** `POST|GET /v1/orgs`, `GET|PATCH|DELETE /v1/orgs/{org}`, `POST /v1/orgs/{platform}/children`.
- **Members:** `GET|POST /v1/orgs/{org}/members`, `PATCH|DELETE …/members/{id}`, `POST /v1/orgs/{org}/invites`, `POST /v1/invites/{token}/accept`.
- **Enrollment:** `POST|GET|DELETE /v1/orgs/{org}/enrollment-tokens`, `POST /v1/enroll`, `POST /v1/enrollments/{id}/complete`, `GET|POST /v1/orgs/{org}/enrollment-requests/{id}:approve|:reject`.
- **Devices:** `GET /v1/orgs/{org}/devices`, `GET|PATCH|DELETE …/devices/{id}`, `POST …/devices/{id}:expire`.
- **Services:** CRUD `/v1/orgs/{org}/services`.
- **Rules:** CRUD `/v1/orgs/{org}/rules`, plus `POST /v1/orgs/{org}/rules:preview` (compile, diff and reachability delta).
- **Grants:** `POST /v1/grants`, `GET /v1/orgs/{org}/grants?direction=in|out&status=`, `GET /v1/grants/{id}`, `POST /v1/grants/{id}:revoke|:extend`.
- **Access check:** `GET /v1/orgs/{org}/access:check?src=&dst=&port=` (explain).
- **Audit:** `GET /v1/orgs/{org}/audit-events`, `GET …/audit-events:export` (NDJSON), `GET …/audit-events:verify`.
- **Webhooks:** CRUD `/v1/orgs/{org}/webhooks`, `POST …/{id}:test`, `GET …/{id}/deliveries`.
- **API keys:** CRUD `/v1/orgs/{org}/api-keys`.
- **Sandboxes:** `POST /v1/orgs/{org}/sandboxes` (register; returns enrollment and config), `GET …/sandboxes/{id}`, `POST …/sandboxes/{id}/events` (mode switches, acceptance results).
- **Real-time:** `GET /v1/orgs/{org}/events:stream` (SSE).
- **Cell protocol:** `cell.v1.CellService/Sync` (Connect, mTLS).
- **Ops endpoints:** `/healthz`, `/readyz` (false in recovery mode until overdue revocations are done), `/metrics` (separate listener), `/openapi.json`, docs UI.

**Status 2026-10-29:** built under `/v1`, with the org taken from the API key rather than an `{org}` path segment: `me`, enrollment tokens, `enroll` and `complete`, `devices` (list), grants (create, list, revoke), API keys (create), audit events (list, verify), **rules** (create, list, get, delete; `rules:read`, `rules:write`; create and delete return once the cell enforces the change) and the **access check** (`GET /v1/access-check?src=&dst=&port=`). The check names every template, rule, grant or platform rule that allows the connection on its own, using `policy.Explain`, which compiles each candidate alone with the same compiler (property-tested against the full policy). A device of another org that nothing lets through reads exactly like one that doesn't exist. The CLI has `headmesh rule add|ls|rm` and `headmesh access check`. Still to build: orgs and members, services, rule update with `If-Match` and `rules:preview`, webhooks, sandboxes and the event stream.

### 9.9 Authentication and authorization
- **Users:** email magic link (Resend), **passkeys** (go-webauthn) and GitHub/Google OIDC sign-in (zitadel/oidc RP). Per-org enterprise OIDC comes later.
- **Sessions:**
  - Server-side sessions in Postgres; `__Host-hm_session` cookie (Secure, HttpOnly, SameSite=Lax).
  - CSRF protection via Go's `http.CrossOriginProtection`.
  - Session rotation on privilege change.
- **OAuth authorization server** (zitadel/oidc OP) at `https://api.headmesh.dev`:
  - authorization code + PKCE S256;
  - **device flow RFC 8628** for the CLI;
  - refresh-token rotation with family reuse detection;
  - `client_credentials` for platforms (Xcelsior server-to-server);
  - **CIMD** (primary per MCP 2026-07-28) plus **DCR** (deprecated, kept for client compatibility, with Xcelsior-style rate limits and redirect host allowlists);
  - RFC 8707 resource indicators with audience-bound EdDSA JWTs (15 minutes);
  - RFC 9207 `iss` in responses;
  - RFC 8414 metadata.
- **Authorization:**
  - Roles: owner, admin, member, viewer, billing. Platform scopes: `platform:orgs`, `platform:grants`.
  - Plain Go permission checks with exhaustive table tests. Every route declares its permission, and a test enumerates the routes.

### 9.10 Webhooks
- **Signing:** Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`, HMAC-SHA256, secret rotation).
- **Delivery:** River retries with exponential backoff (1 minute up to 24 h, 10 attempts). An endpoint is auto-disabled after 24 h of failures; replay is available.
- **Event catalog:**
  - `org.*`, `member.*`, `device.enrolled|renamed|removed|online|offline`;
  - `grant.*`, `rule.changed`, `policy.applied|failed|drift_corrected`;
  - `sandbox.mode_changed`, `apikey.*`.

### 9.11 Observability and SLOs
- **Signals:**
  - Logs: `log/slog` JSON with request_id, org_id and trace_id.
  - Traces: OpenTelemetry (otelhttp, pgx tracer, River middleware).
  - Metrics: Prometheus.
  - Shipping: Grafana Alloy → **Grafana Cloud Free**, kept separate from Xcelsior's LGTM.
- **Key metrics:**
  - `headmesh_policy_compile_seconds`, `headmesh_policy_apply_seconds{cell}`, `headmesh_policy_apply_failures_total`;
  - `headmesh_revocation_enforced_seconds`, `headmesh_grants_active`, `headmesh_cell_last_sync_seconds`;
  - `headmesh_enrollment_duration_seconds`;
  - scraped Headscale `:9090` metrics.
- **SLOs:**
  - API availability 99.9%.
  - Enrollment end to end p95 < 15 s.
  - Revocation enforced p99 < 5 s.
  - Policy drift corrected in under 2 minutes.
- **Synthetic isolation prober** (every minute):
  - Two probe orgs with two tiny nodes each on hs1, run as ephemeral containers on hm-prod-1.
  - Asserts that cross-org traffic is **denied**, and that a probe grant flips reachability within the SLO and then flips back.
  - **Any unexpected cross-org success pages as P0.**
- **Alerting:** Grafana alerting → Telegram (as Xcelsior already does) and email.

### 9.12 Scale and cells
- Cell capacity comes from load tests:
  - k6 for the API.
  - Map-update fan-out with up to 500 real `tailscale` client containers on hourly `cx33` VMs (about €0.03 per run).
- Start limits for hs1: 1,000 nodes and 300 orgs. Add cells horizontally; a tenant is pinned to one cell.
- Cross-tenant grants require both orgs in the same cell. The platform's child orgs are always co-located.

---

## 10. Track E — CLI (`headmesh`)

**Status 2026-10-28:** `join`, `invite`, `devices` and `grant` are built. Releases: a `v*` tag runs `.github/workflows/release.yml` (GoReleaser 2.19, keyless cosign signature on the checksums, syft SBOMs, build-provenance attestations, deb/rpm). Homebrew and npm publishing wait for the tap repository and npm trusted publishing.

- **Commands:**
  - Account: `login` (device flow), `logout`, `whoami`.
  - Network: `join <token|url> [--name] [--isolated] [--instance <name>] [--accept-dns]`, `leave`, `status [--json]`.
  - Devices: `devices ls|rename|rm`, `invite device|member [--role] [--ttl] [--uses]`.
  - Access: `grant create --to <org> --device <d> --ports tcp:22 --ttl 4h`, `grant ls|revoke|extend`, `rules ls|apply -f|preview`.
  - Audit: `audit tail|verify`.
  - Sandbox: `sandbox init|render|up [--yolo --ttl]|down|recreate|status|doctor|logs|shell|attach|backup|scan-secrets|acceptance`.
  - Codex add-on: `agent run --provider openai`.
  - Tooling: `doctor`, `completion`, `version`, `update`.
- **`join` on Linux:**
  1. Detect existing `tailscaled` instances.
  2. If the host is already on another tailnet, or `--isolated` is set, create an **isolated instance**: systemd template `headmesh-tailscaled@<instance>.service` with its own `--state`, `--socket=/run/headmesh/<instance>.sock`, `--port` (41650+n) and `--tun=hm<n>`. This is Xcelsior's `tailscaled-xcelsior` pattern, generalized.
  3. If Tailscale isn't installed, install it from pkgs.tailscale.com after a consent prompt.
  4. sudo is requested only for steps that need it.
  - macOS: Tailscale standalone app with a custom login server (documented). Windows: M5+.
- **Credentials and output:**
  - Credentials live in the OS keyring (go-keyring), falling back to a 0600 file.
  - `--json` on every command, with documented exit codes.
  - No telemetry by default.
- **Distribution (GoReleaser):**
  - Targets: linux, darwin and windows on amd64 and arm64.
  - Signing and SBOM: cosign keyless signatures, SBOM, provenance.
  - Homebrew tap `headmesh-dev/homebrew-tap`; deb/rpm via nfpm.
  - npm `@headmesh/cli` with platform `optionalDependencies` (`@headmesh/cli-linux-x64`, …), published by CI with npm trusted publishing; plus a `headmesh` alias package.
  - `go install`.
  - `install.sh` at get.headmesh.dev verifies sha256 **and** the cosign signature.
- **Tests:** testscript golden tests for every command, plus e2e `join` against an ephemeral Hetzner VM in the nightly suite.

---

## 11. Track F — Xcelsior integration (Xcelsior repo, one folder)

- **Folder:** `control_plane/headmesh/`, because `control_plane` is Xcelsior's real package and the natural home.
  - `client/`: generated by openapi-python-client from the Headmesh OpenAPI, pinned. A CI diff check fails if the generated client is stale.
  - `service.py`: wrapper with httpx timeouts, tenacity retries, idempotency keys and a circuit breaker.
  - `onboarding.py`, `grants.py`, `isolation.py`, `webhooks.py`, `reconcile.py`, `settings.py`.
- **Route:** `api/routes/headmesh_webhooks.py`, registered in `ALL_ROUTERS`. It verifies Standard Webhooks signatures and passes Xcelsior's guard and coverage tests.
- **Configuration** (added to `.env.example` **and** the compose `x-api-environment` anchor): `XCELSIOR_HEADMESH_ENABLED`, `_API_URL`, `_CLIENT_ID`, `_CLIENT_SECRET` (`client_credentials`), `_PLATFORM_ORG`, `_CELL=xcelsior`, `_WEBHOOK_SECRET`.
- **Migration `129_headmesh_links.py`** (Xcelsior style: raw `op.execute`, `IF NOT EXISTS`, up/down/up):
  - `headmesh_links(tenant_id, kind ∈ host_device|attempt_grant|owner_org, local_id, headmesh_id, created_at)`, unique `(kind, local_id)`, index on `tenant_id`.
  - Added to `TABLE_DOMAINS` with grants to the service roles.
- **GPU-owner onboarding ("Connect your GPU"):**
  - `enroll_host` stops minting raw Headscale keys (`issue_mesh_enrollment`). It ensures a child org `xc-owner-<customer_id>` (template `locked`) under platform org `xcelsior`, then creates a single-use `gpu-host` enrollment token.
  - `install.sh join_mesh` either calls `POST /v1/enroll` and keeps the existing `tailscaled-xcelsior` unit (minimal change first), or runs `headmesh join --instance xcelsior`.
  - The wizard's `setupNetworking` conflict (default tailscaled) is fixed to use the dedicated instance.
  - The `device.enrolled` webhook marks the host network-ready.
  - **Host delete now deletes the Headmesh device** (closes the deprovisioning gap).
  - Any module shipped to hosts is added to `sign-static-agent.py`.
- **Billing hooks:** a new **projection sink `headmesh_grants`** in `control_plane/projection_delivery.py`, consuming outbox events.
  - Grant: `job.v1.attempt_status_changed` with status running, `instance_started` → `POST /v1/grants` with grantor = owner org, grantee = platform infra org, `src: role xc-control` (gateway/terminal nodes), `dst: host device`, ports `tcp:<job host_port>`, `expires_at = rental hold end + 10 min grace`, `external_ref = attempt_id`.
  - Revoke: `stopped`, `terminated`, `cancelled`, `preempted`, `lease_expired`, and the wallet-empty stop.
  - Delivery is at-least-once and safe through `external_ref`.
  - A reconcile task (bg_worker, every 5 minutes) compares active attempts with active grants. Xcelsior is the source of truth.
- **Renter isolation rules (`isolation.py`, platform template):**
  - Provider orgs: `locked`, with no subnet routes and no exit nodes.
  - Provider hosts reach control only on the SPIRE/NFS/Ganesha ports. They can't reach each other or Aaryn's personal org.
  - The platform reaches hosts on 22 (platform docker-over-SSH, standing rule) plus job ports **only through active grants**.
  - Container ports bind to the mesh IP. The existing egress isolation is unchanged.
  - Future direct renter mesh access: a renter child org device gets a grant to `host:job_port` only.
- **Tests** (named as sentences):
  - `tests/test_headmesh_grant_follows_the_attempt_lifecycle.py`
  - `tests/test_headmesh_grant_is_idempotent_per_attempt.py`
  - `tests/test_host_delete_removes_the_headmesh_device.py`
  - `tests/test_provider_hosts_cannot_reach_each_other.py` (live gate)
  - `tests/test_headmesh_webhook_rejects_bad_signatures.py`
  - `tests/test_headmesh_client_matches_the_openapi_contract.py` (schemathesis)
- **Rollout:** per-host feature flag. Cell `observe` → `enforce` (§13.8). Xcelsior live gates run at each step.

---

## 12. Track G — Web

### 12.1 `web/marketing` (headmesh.dev, deployed independently)

**SEO and agents (2026-10-28):** `/llms.txt`, `/llms-full.txt` and a Markdown copy of every docs page (linked with `rel="alternate"`), schema.org JSON-LD (Organization, WebSite, SoftwareApplication, SoftwareSourceCode), a `robots.txt` that names the AI crawlers it welcomes (the test site answers `Disallow: /`), and self-hosted fonts through the Astro Fonts API with metric-matched fallbacks (CLS 0). `AGENTS.md` and `CLAUDE.md` brief coding agents.

**Changed 2026-10-28 (ADR 0013):** headmesh.dev is served by nginx on hm-prod-1, not Cloudflare Pages. The waitlist is a Node 24 service on SQLite (same validation, Turnstile, tokens and email), deployed with `deploy/deploy.sh --target frontend`. Cloudflare stays the DNS, proxy, Turnstile and Web Analytics provider. The bullets below describe the original Pages design where they differ.
- **Stack:** Astro 7.4 static output, Tailwind 4.3, Starlight 0.42 at `/docs` (adds the API reference via `starlight-openapi` from M2).
  - Self-hosted fonts via Fontsource; dark mode with `prefers-color-scheme` and a toggle.
  - Build-time OG images; sitemap; `security.txt`.
  - `_headers`: strict CSP, HSTS (.dev is already on the HSTS preload list), Referrer-Policy, Permissions-Policy.
- **Pages:** `/` (landing), `/docs`, `/github` (→ `github.com/headmesh-dev/headmesh`), `/waitlist`, `/privacy`, `/terms`.
- **Copy (from the blueprint):**
  - Headline: **"Share machines, not your network."** (variation 3).
  - Subheadline: the blueprint's draft. Agent section heading: **"Give your AI agents a real machine, not a toy sandbox."** (variation 2).
  - Five feature blocks.
  - Proof: "Xcelsior Compute runs on Headmesh" (publish once M4 is live; until then "Built for Xcelsior Compute").
  - CTA: Join the waitlist / Star on GitHub.
- **Waitlist:**
  - A Cloudflare Pages Function `functions/api/waitlist.ts`: zod validation, server-side **Turnstile** verify, normalized email, Cloudflare rate-limit binding.
  - **D1** table `waitlist(id, email UNIQUE, use_case, source, utm, consent_version, ip_hash, created_at, confirmed_at, unsubscribed_at)`.
  - **Double opt-in** email through Resend with a signed token. **CASL**: an explicit consent checkbox, sender identification (Xcelsior Compute Inc. postal address) and one-click unsubscribe.
  - Admin export via `wrangler d1 execute`, behind Cloudflare Access for any admin route.
  - The sign-up count is the success metric.
- **Analytics:** **Cloudflare Web Analytics** (cookieless, free, no consent banner).
- **Quality gates:** Lighthouse CI (≥ 95 on all four categories), Playwright and axe, broken-link check.
- **Deploy:** Cloudflare Pages via GitHub Actions with a path filter `web/marketing/**`; preview deploys per PR. Marketing changes never touch the product.

### 12.2 `web/app` (sign-up first, dashboard later)
- **Stack:** Vite 8 + React 19 + TanStack Router (file-based, type-safe) + TanStack Query + `openapi-fetch` client generated from the API + shadcn/ui + Tailwind 4 + react-hook-form/zod. Built into `headmesh-server` with `go:embed` and served at `app.headmesh.dev`.
- **M3 pages:**
  - Sign up / sign in (email link, passkey, GitHub/Google), verify email, create org.
  - **Onboarding checklist:** "Add your first device" shows a one-liner and live SSE device appearance.
  - Devices list; invite links; grants list (read-only).
- **Later:** HuJSON rules editor (Monaco with schema), access explain, audit viewer, webhooks, API keys, sandbox status.
- **Tests:** Vitest and Playwright e2e in CI against an ephemeral server with Postgres.

---

## 13. Deploy and operations

### 13.1 Cost (from the Hetzner API on 2026-10-26; net €, VAT 0%) — "one VM + on-demand standby"

| Item | €/month | Notes |
|---|---|---|
| hm-prod-1 **`cx23`** (2 vCPU / 4 GB / 40 GB / 20 TB) in **nbg1** | 5.49 | **Created now on the expiring credits.** Created as `cx23`, then rescaled CPU/RAM-only to `cx33` while credits last (see the comparison below) |
| Primary IPv4 | 0.50 | |
| Hetzner automated backups | 0 (skipped) | R2 replication (RPO about 10 s) plus weekly drills beat daily snapshots; +€1.10 if wanted |
| Standby | 0 idle | Created only on failure (€0.0088/h). **Delete the fenced VM after review: powered-off servers still bill** |
| Restore drills (weekly, about 1 h `cx23`) | ≈ 0.04 | |
| E2E / load-test VMs (hourly) | cents per run | A janitor deletes `ephemeral=true` VMs older than 2 h |
| Cloudflare R2, Pages, D1, Workers cron, Turnstile, Email Routing, Web Analytics | 0 | Free tiers; R2 writes budgeted under 1M per month (§13.3) |
| Resend, Grafana Cloud, GitHub org | 0 | Free tiers |
| **Run-rate** | **≈ €6/month (≈ CA$9.50)** + the headmesh.dev renewal (≈ US$12–15/year; check in Porkbun) | The earlier warm-standby design was €19.68/month, so this is **~70% less** |

**`cx23` vs `cx33` — what the extra €3/month buys**

| | `cx23` | `cx33` |
|---|---|---|
| vCPU / RAM / disk | 2 / 4 GB / 40 GB | 4 / 8 GB / 80 GB (40 GB if rescaled CPU/RAM-only) |
| Price (+ IPv4) | €5.49 (€5.99) | €8.49 (€8.99) |
| Hourly | €0.0088 | €0.0136 |
| Fits | API + Postgres (≈0.8 GB) + Headscale + Caddy + Alloy for the first few hundred nodes; tight during image pulls, bursts or prober containers; needs a 2 GB swap file | Comfortable headroom: Postgres can cache the whole working set, the synthetic prober and e2e client containers run beside production, builds and pulls don't swap, and there's room for a few thousand nodes and an in-place load test |

- **Credits trick:** create as `cx23`, then rescale **CPU and RAM only** to `cx33`. The disk stays 40 GB, so you can drop back to `cx23` any time. A server *created* as `cx33` has an 80 GB disk and can never go back to `cx23`.
- **When credits run out:** stay on `cx33` only if memory use is over ~70% or API p95 latency regresses on `cx23` load tests. Otherwise rescale down to `cx23` (one reboot, about a minute of downtime, the IP is kept).
- **Use the remaining credits** on hourly e2e and load-test VMs before they expire.

**Money-savers applied:**
- EU instead of US (3–4× cheaper, 20 TB vs 1–3 TB of traffic).
- `cx` x86 instead of ARM `cax` or dedicated `ccx`.
- No idle standby, floating IP or load balancer.
- Replication instead of Hetzner backups.
- Start small and rescale later.
- **Use the expiring credits now:** hm-prod-1 is created in M0 and the credits pay for it and for hourly e2e/load VMs. Out-of-pocket cost starts only after they expire.
- Tailscale's public DERP as the free relay outside the VM until launch scale.
- Reclaim prod-hetzner's 22.6 GB of Docker cache instead of buying a Volume or rescaling (with approval).

### 13.2 hm-prod-1 stack (`deploy/compose/`)

**Changed 2026-10-28 (ADR 0013):** nginx 1.28 and systemd units instead of Caddy and Compose, Postgres 18 from Ubuntu, and `deploy/deploy.sh --env prod|test|all --target frontend|backend|all` with blue-green services and `nginx -t` before every reload; see `deploy/README.md`. `deploy/tests/e2e.sh` runs the whole pipeline against a systemd container. **Live 2026-10-29:** prod (headmesh.dev, api.headmesh.dev) and test (test.headmesh.dev, api-test.headmesh.dev) are deployed on hm-prod-1 with a self-signed origin certificate until the Let's Encrypt certificate is issued. **hs1 live 2026-10-29:** Headscale 0.29.6 serves `hs1.headmesh.dev` (grey-cloud, TTL 60) behind nginx with the Let's Encrypt certificate for `headmesh.dev` and `*.headmesh.dev` (DNS-01 through a token scoped to the zone's DNS); Cloudflare is on Full (strict). headmesh-cellagent runs in enforce mode and reconnects across every blue-green switch (`deploy/README.md`, "The hs1 cell"; covered by `deploy/tests/e2e.sh`). Litestream and pgBackRest are not deployed yet.
- **Services:**
  - Caddy 2 with automatic HTTPS (DNS-01 via Cloudflare so a recovery VM can get certificates before DNS moves). Reverse proxy for `api.`, `app.`, `hs1.` (h2c and WebSocket for `/ts2021`) and `*.sbx.`.
  - **Headscale v0.29.6** (tagged release image, pinned by digest): `policy.mode: database`, sqlite WAL, `server_url: https://hs1.headmesh.dev`, `dns.base_domain: mesh.headmesh.dev`, embedded DERP region 900 "nbg" with STUN on 3478, metrics on 127.0.0.1:9090, `logtail.enabled: false`.
  - headmesh-server, headmesh-cellagent[hs1] (unix socket).
  - Postgres 18 (`shared_buffers` 512 MB on `cx23`).
  - Litestream 0.5.17, pgBackRest, Grafana Alloy.
  - tailscaled (hs1 infra node) plus a second instance in the xcelsior cell as `tag:hm-ingress`/`tag:hm-ops`.
- **Memory budget on `cx23` (4 GB):** Postgres ~0.8 GB, Headscale ~0.3 GB, server ~0.3 GB, Caddy, Alloy and agents ~0.4 GB; 2 GB swap file.
- **Hetzner Cloud Firewall (free):**
  - Inbound 443/tcp, 80/tcp (redirect/ACME), 3478/udp (STUN), 41641/udp (tailscaled), 51820/udp (break-glass WireGuard).
  - **No public 22, no 50443.** SSH only over the tailnet or break-glass. The one exception is during bootstrap: 22 from Aaryn's IP only, removed afterwards (§5.8).
  - hm-prod-1 and its firewall were created by hand in the Console; `tofu import` them so OpenTofu owns them from then on.
- **OpenTofu (`deploy/tofu/`):**
  - Hetzner: server, primary IPv4, firewall, SSH keys, labels.
  - Cloudflare: DNS (hs1 **grey-cloud**, because Noise/WebSocket break behind the proxy), R2 buckets with **scoped tokens** (`hm-hs1-litestream`, `hm-pg`, `hm-keys`, `hm-tofu`), Pages project, D1, Email Routing, Turnstile, the Worker monitor.
  - State: R2 backend with OpenTofu **native state encryption**.
- **cloud-init:**
  - Users; `unattended-upgrades`; sysctl hardening; Docker from the official repo (pinned).
  - Fetch sops-decrypted env through the age key (on the VM only).
  - `deploy/compose/bootstrap.sh init|restore` is idempotent.
- **Secrets:** sops + age in `deploy/secrets/*.enc.yaml`. Recipients: Aaryn's age key and a recovery key kept as a GitHub Actions environment secret. Nothing in plaintext in git or R2.
- **Deploy pipeline:**
  1. Tag → build, sign and attest images.
  2. `tofu plan` (requires review) → apply.
  3. The deploy job joins the tailnet with an **ephemeral tagged CI key** (`tag:ci`, minted by Headmesh), SSHes over the tailnet and runs `docker compose pull && up -d` with health-gated rollout.
  4. Automatic rollback to the previous image digests if health fails.

### 13.3 Replication, backups and R2 budget (requirement 5)
- **Headscale sqlite → Litestream → R2 `hm-hs1-litestream`:**
  - `sync-interval: 10s` caps level-0 uploads at about 259k PUTs/month, and only when there are writes.
  - Compaction levels at 5 minutes and 1 hour add about 9.5k.
  - Snapshots every 24 h.
- **Postgres → pgBackRest → R2 `hm-pg`:**
  - WAL archiving with `archive_timeout=300` (about 8.6k segment PUTs/month at idle).
  - Weekly full and daily differential backups; retention of 4 full backups.
  - **`repo1-cipher-type=aes-256-cbc`** encryption.
- **Budget:** about 300k Class A operations/month, against the **1M free**. An R2 GraphQL usage probe alerts at 70%. The drill and recovery workflows mostly read (Class B, 10M free).
- **Noise and DERP private keys, plus Headscale config:**
  - Encrypted with **age** to the two recipients before upload to `hm-keys`.
  - Also kept sops-encrypted in git.
  - Never uploaded in plaintext; Litestream only ships `db.sqlite`.
- **SPIKE T-9:** check whether Headscale stores pre-auth keys hashed. They are 5-minute, single-use keys either way, and the replica bucket is private with a scoped token.
- **SPIKE T-10:** measure the real PUT rate for a week on hs1 and tune `sync-interval`.
- **Manifest:** every 5 minutes the primary writes `manifest.json` to R2 with node count, user count, Noise public-key fingerprint, policy hash, last grant boundary and Postgres LSN. Recovery and drills use it to detect empty or wrong replicas.

### 13.4 Failure detection, fencing and recovery (requirements 1, 4, 7)

**Status 2026-10-28:** recovery mode is in headmesh-server (`serve -recovery`): `/readyz` 503 and writes refused with `Retry-After` until overdue grant expiries are audited and every enforce-mode cell has applied a current policy. Normal starts and blue-green deploys only run the expiry sweep. The detection Worker, fencing tool and workflows are not built yet.
- **External detection** (the recovery logic never runs on the failed VM):
  - A **Cloudflare Worker with a 1-minute cron trigger** (free) probes `https://hs1.headmesh.dev/health` and `https://api.headmesh.dev/readyz` from the edge.
  - After **3 consecutive failures** it sends a `repository_dispatch` (`control-plane-down`, with a fine-grained token that can only dispatch) to `headmesh-dev/headmesh`.
  - Workflow **`recover.yml`** runs in environment `recovery`, single concurrency group, 30-minute timeout.
  - A first job **re-confirms from GitHub's network** (3 probes over 2 minutes) to rule out a Cloudflare-side false positive. A manual `workflow_dispatch` is also possible.
- **Steps:**
  1. **Fence:** read the server through the Hetzner API. If it's running, `poweroff`, wait until it's `off`, and label it `fenced=true`. Also detach its primary IPv4 (the API allows that while it's off) so it can never answer on the old address if someone powers it back on. Two control servers with the same keys **never** run at once. If the API can't confirm `off`, **abort and page**: no split brain.
  2. **Preflight capacity (requirement 7):**
     - Count servers in project `headmesh` against the configured limit `HCLOUD_SERVER_LIMIT` (from Console → Limits; **SPIKE T-11**: no limits endpoint is known in the API).
     - Try the fallback chain `cx23@fsn1 → cx23@hel1 → cx33@fsn1 → cx33@hel1 → cpx21@fsn1 → cax11@fsn1`. Images are multi-arch.
     - On `resource_unavailable` or `resource_limit_exceeded`, try the next option.
     - If the limit is reached, page with instructions (delete the fenced VM or raise the limit).
  3. **Create** the VM with cloud-init in **restore mode** and attach the firewall and SSH keys.
  4. **Restore and verify**, refusing on any failure (ported from Xcelsior's rules):
     - `litestream restore` then `sqlite3 'PRAGMA integrity_check'`;
     - node count ≥ the manifest's (**never promote an empty replica**);
     - age-decrypt the keys; the **Noise public-key fingerprint equals the manifest's** (otherwise it's a different tailnet: refuse);
     - DERP key present;
     - `pgBackRest restore` to the latest point.
  5. **Recovery mode (requirement 4):**
     - headmesh-server starts with `--recovery`: `/readyz` reports 503 and every mutating endpoint returns 503 with `Retry-After`.
     - It **first** sweeps overdue grants (`expires_at < now` → expired/revoked, audited as `grant.expired_late` with how late they were).
     - It compiles and applies the policy to **all** cells and waits for cell-agent acknowledgements of the new hash.
     - Only then does it accept other requests.
  6. **Start Headscale** with the same `server_url`, Noise key and DERP key. Caddy obtains the certificate and the workflow **proves the new VM serves `hs1.headmesh.dev` on 443 via `--resolve`** before moving DNS (as in `test_promotion_refuses_when_the_standby_cannot_serve_the_name`).
  7. **Flip DNS:** Cloudflare A/AAAA records (grey-cloud, TTL 60) for `hs1`, `api` and `app`.
  8. **Post-checks:** an ephemeral `tailscale` client in the GitHub runner registers with a CI key; `headscale nodes list` count; a policy hash match.
  9. **Notify** by Telegram and email with an RTO report. The fenced VM is kept powered off for forensics, and a reminder fires at 24 h to delete it (it still bills).
- **No automatic failback.** Cloud VMs are fungible, so the new VM simply becomes primary. Moving back to nbg1 is a manual, planned rebuild with the same rules: refuse diverged databases unless forced, and refuse a different Noise key as "two tailnets".
- **Ported tests** (`deploy/ha/*_test.go`, table-driven), from Xcelsior's `test_headscale_failover.py`, `test_failback_leaves_an_authoritative_primary_alone.py` and `test_headscale_replication_names_the_real_fault.py`:
  - empty replica not promotable;
  - replica without Noise key not promotable;
  - watchdog waits for the failure threshold;
  - healthy primary → no promotion;
  - promotion refuses when the standby can't serve the name;
  - no automatic failback;
  - diverged databases refused;
  - different Noise keys refused as two tailnets;
  - force is required to accept a loss;
  - overlapping refreshes never share a snapshot;
  - an empty snapshot never overwrites a good replica;
  - a persistent replication failure pages once, not every tick;
  - **new:** fencing must confirm `off` before restore; recovery mode blocks writes until overdue revocations are applied; fallback chain on `resource_unavailable`.
- **The ASUS is not used as a standby for hs1.** A laptop that sleeps behind home NAT isn't a reliable target; that was the limitation in Xcelsior's own runbook.

### 13.5 Restore drills (requirement 2)
- `restore-drill.yml` runs weekly (Mondays 06:00 UTC) and on demand.
- **Steps:**
  1. Create an ephemeral `cx23` labelled `drill=true,ephemeral=true`, with no DNS changes.
  2. Restore the latest Litestream and pgBackRest backups.
  3. **Checks:**
     - Headscale starts with the restored Noise key (fingerprint match);
     - `headscale nodes list` count equals the manifest;
     - `headscale policy check` passes on the stored policy;
     - headmesh-server `/readyz` passes after its recovery sweep;
     - `headmesh audit verify` passes for every org;
     - a sample `GET /v1/grants` returns the expected count;
     - a throwaway `tailscale` client registers against the drill instance's IP with `--login-server` and an internal CA.
  4. Record the measured RTO and RPO as metrics.
  5. **Delete** the VM.
- **Janitor:** an hourly workflow deletes any `ephemeral=true` server older than 2 h.
- **Cost:** about €0.01 per drill.

### 13.6 DERP outside the VM (requirement 3)
- **hs1 DERP map:**
  - Embedded DERP region **900 (nbg)** on hm-prod-1.
  - **Tailscale's public DERP map** (`derp.urls`) as the out-of-VM relay set, with regions all over the world and €0 cost.
- **During a control-plane outage,** clients keep their netmap and DERP map. Live direct and relayed connections continue through Tailscale's relays, so only re-registration, new enrollment and policy changes pause.
- **Before public launch or commercial scale:** add a Headmesh-run `derper` in a second location (`cx23` hel1, €5.99/month, with `--verify-clients` through a co-located tailscaled), or on prod-hetzner with approval. Then reduce reliance on Tailscale's relays.
- The DERP private key is replicated encrypted, exactly like the Noise key.

### 13.7 Break-glass WireGuard (generalizing `wg-backdoor`)
- `deploy/breakglass/`: a `wg-quick@wg-breakglass` interface on hm-prod-1 and on any recovery VM (cloud-init installs it with the same keys from sops).
- **Peers:** the ASUS (existing `wg-backdoor`) and the Mac.
- `sshd` listens only on tailnet and WireGuard addresses.
- **Quarterly drill:** reach the VM with tailscaled stopped.
- `headmesh doctor --breakglass` checks handshake age.
- Documented in `docs/runbooks/breakglass.md`.

### 13.8 Adopting the `xcelsior` cell (M4; every step needs approval)
1. **Install `headmesh-cellagent` on prod-hetzner** (outbound-only, unix socket; **no new inbound access**) in **observe** mode. It reports nodes and the current policy. Headscale stays in `policy.mode: file` for now; the agent reads `/etc/headscale/acl.json`.
2. **Import mapping** (`deploy/cells/xcelsior.import.yaml`, reviewed by Aaryn):
   - `aaryn-personal`: Aaryn's own devices, including the sandbox host as a personal device.
   - `xcelsior-infra`: prod-hetzner nodes, `tag:xc-control`.
   - One `xc-owner-*` org per provider host.
   - `sandbox` org: the ASUS sandbox nodes.
3. **Dry run.** `headmesh cell diff xcelsior` compiles the candidate policy and shows the **reachability matrix delta**, flow by flow, against §5.2's hardened policy using the evaluator. The candidate must preserve every flow the Xcelsior survey identified: SSH relay and web terminal, port exposure, SPIRE 8095, NFS, Ganesha, Lightning, Ollama, operator SSH.
4. **Maintenance window:**
   - Switch Headscale to `policy.mode: database` (config change plus restart; the replica and failover tool are updated first).
   - Move the agent to **enforce**, with the cell-invariants file protecting the critical flows.
   - Run Xcelsior live gates; `headmesh-cellagent rollback` is the instant undo.
5. **Keep IPs.** No re-enrollment; node IDs and 100.64.0.x addresses are unchanged.
6. Later, optionally move the xcelsior cell's standby from the ASUS to the same Litestream and recovery pattern (separate decision).

---

## 14. Security model

- **Threat actors:** prompt-injected cloud agents, malicious renters, malicious GPU owners, a leaked Headmesh API key, a compromised Headmesh VM, a compromised cell host.
- **Tenant isolation, enforced in five layers:**
  1. Compiler invariants, property-tested.
  2. Headscale policy `tests`, enforced by the server.
  3. Cell-agent invariants and rate limits.
  4. E2E isolation matrix in CI (nightly, plus every PR that touches `core/policy`).
  5. The production prober, paging P0.

  Every enrollment, grant and revocation is audited with a hash chain.
- **Headscale API key exposure:** removed entirely. The cell agent uses the unix socket. Any remote gRPC option needs Aaryn's approval (§9.4).
- **Sandbox:**

  | Risk | Controls |
  |---|---|
  | Lateral movement | Bridge network namespace (no host network); own node identity; egress firewall to LAN and host |
  | Privilege | Agents run as uid 1000 with no capabilities and `no-new-privileges`; no `docker.sock`, no sudo |
  | Secrets | Denylist plus masks; `scan-secrets` |
  | YOLO | Read-only `/host` (recursive read-only); TTL; preflight backup; warning about untrusted content |
  | Git | Fine-grained token; client-side hooks against force-push; server rulesets when the plan allows |
  | Runner secret | Privilege split (T-1) |

- **Supply chain:**
  - Pinned digests and SHAs; Renovate brings new stable releases as they ship.
  - cosign keyless signing; SLSA provenance attestations; syft SBOMs.
  - govulncheck, osv-scanner, trivy, CodeQL, zizmor, gitleaks, Scorecard.
  - `SECURITY.md` with private vulnerability reporting; `/.well-known/security.txt` on headmesh.dev.
- **Pre-launch checklist:**
  - OWASP ASVS L2 pass on the API and OAuth server.
  - OAuth conformance checks modelled on Xcelsior's `gx0_conformance.py`.
  - Fuzzing campaigns.
  - Dependency licence review.

---

## 15. Testing and CI

| Layer | Tooling | Where |
|---|---|---|
| Unit | go test (race), Vitest | every PR |
| Property / fuzz | rapid invariants (§9.3), `go test -fuzz` (HuJSON, token parsing, selectors) | every PR (short), nightly (long) |
| Integration | testcontainers-go: postgres:18, headscale:v0.29.6, tailscale:v1.104.2 clients, cellagent | every PR |
| E2E isolation | `test/e2e`: 3 orgs × 2 devices + platform + grants → reachability matrix (`tailscale ping` / `nc`), netmap visibility (`tailscale status --json`), revocation latency, established-flow cut, quarantine of a rogue node | nightly on an ephemeral Hetzner VM (real NAT/DERP) and locally |
| API contract | schemathesis against OpenAPI; oasdiff breaking-change gate | every PR |
| CLI | testscript golden tests; nightly `join` against an ephemeral VM | PR / nightly |
| Sandbox | hadolint, container-structure-test, trivy, compose config lint, acceptance harness on the ASUS self-hosted runner (`workflow_dispatch` only, owner-restricted) | PR / manual |
| Plugins | `claude plugin validate --strict`, schema validation, install smoke test | every PR touching `plugins/**` |
| Web | Biome, Vitest, Playwright + axe, Lighthouse CI | PR |
| Ops | `restore-drill.yml` weekly; `recover.yml` game day monthly (against a staging replica); janitor hourly | scheduled |

**Workflows:** `ci-go.yml`, `ci-web.yml`, `sandbox-image.yml`, `plugins.yml`, `e2e.yml`, `release.yml`, `deploy.yml`, `recover.yml`, `restore-drill.yml`, `janitor.yml`, `codeql.yml`, `scorecard.yml`, `renovate.json`.

---

## 16. Blueprint coverage matrix

| Blueprint item | Section |
|---|---|
| Sandbox: real networking, files, dependencies, interactive exec, persistent state | §6.1–6.8 |
| Storage decisions (bind mounts; volumes only for caches); visibility (broad read, write to projects) | §6.4 |
| Persistence rules (tailscale state, home, projects bound; caches in volumes) | §6.4 matrix |
| Pitfalls 1–7 (TUN/NET_ADMIN, key expiry, DNS, uid, OS match, GPU, uptime) | §6.3, §6.6, §6.2, §6.13, §5.3 |
| Security requirements (own node + ACLs, auth on tunnel/MCP, no secret mounts) | §6.6, §6.8, §7.4, §6.4, §14 |
| Build order 1–4 (sandbox plugin, cloud connection, Xcelsior plugin, marketplace) | §6, §7, §8 |
| Acceptance test (5 checks + recreate bonus) via MCP, Claude runner, Codex | §6.15 (Codex via SSH **and** exec-server, plus the Tunnel route) |
| Claude Remote Control as fallback/baseline | §7.2 |
| Self-hosted runner and exec-server evaluation | §7.1, §7.6, §1 research |
| All-in-one shared sandbox, supervision, modes, diagnostics | §6.7, §6.11, §6.14, §7.7 |
| Concurrency (no hard lock; optional lock; rebase before push; ask, don't overwrite) | §6.10, §6.9 |
| Regular/YOLO profiles, rules (never default, dedicated key, system read-only, home backup, untrusted-content warning, mode shown) | §6.3, §6.9, §6.11, §6.12 |
| Git workflow (main by default, pull/rebase, no force-push, identity) | §6.9, §7.1 |
| Headmesh core: onboarding, multi-tenancy, sandboxes, temporary grants | §9.2–9.6, §10 |
| Build on Headscale/Tailscale/WireGuard (no networking or crypto rewrite) | §9.2 (no fork; Headscale ≥0.29.6, official clients) |
| Xcelsior-specific logic stays in the Xcelsior repo (onboarding, billing hooks, renter isolation) | §11 |
| Tenant security model (deny by default, time-limited logged grants, immediate revocation, failing cross-tenant tests, audit) | §9.3, §9.6, §9.7, §14, §15 |
| Repo layout (core, api, cli, sandbox, web/marketing, web/app, plugins, docs, deploy) | §3.1 |
| Marketing site (static, pages, waitlist + use-case field, privacy-friendly analytics, dark mode, copy) | §12.1 |
| Consumer slice: CLI + sign-up page first | §10, §12.2 |
| Accounts: GitHub org, npm org/scope, contact email | §5.5 |
| Open to-dos (domain, email forwarding, runner enablement, exec-server access, how diffs show) | §5.5, §7.1 (diffs need a push), §7.6, §19 |
| Success metrics (waitlist sign-ups, GPU owners onboarded, 5-step test) | §12.1, §11, §6.15 |
| Failover WireGuard (`wg-backdoor`) and Headscale standby (from Aaryn) | §5.1, §13.4, §13.7 |

---

## 17. Spikes (each time-boxed to ≤ 0.5 day, with a fallback)

| ID | Question | Fallback |
|---|---|---|
| T-1 | Runner as root plus `setpriv` exec wrapper stops sessions reading the environment secret | Accept the exposure; rotate every 30 days; alert on unknown runners |
| T-2 | Remote Control restarts without a TTY once trust and enablement persist | Keep it in a tmux window, re-armed by the s6 wrapper |
| T-3 | Codex desktop "Remote connections" works with a Tailscale SSH host | OpenSSH as uid 1000 on :2222, bound to the tailnet IP |
| T-4 | Headscale 0.29.6 tag limits; `tagOwners: []` for API-assigned tags | **Resolved: `tagOwners: []` works** (ADR 0014) |
| T-5 | `hscontrol/policy/v2` importable as a Go library | **Fallback taken:** Headscale checks every policy; Headmesh has its own verifier (ADR 0014) |
| T-7 | Mask mounts over a `bind.recursive: readonly` parent | Explicit per-subtree read-only binds without masks |
| T-8 | `read_only` rootfs with tailscaled DNS management | Writable rootfs (other hardening unchanged) |
| T-9 | Pre-auth key storage in the Headscale DB (hashed?) | Already short-lived and single-use; private bucket |
| T-10 | Real R2 PUT rate from Litestream | Raise `sync-interval` to 30 s |
| T-11 | Hetzner Cloud limits are visible only in the Console | Configured limit plus error-driven fallback |
| T-12 | Which plugin manifest Codex prefers (`plugin.json` vs `.codex-plugin`) | **Resolved:** root `plugin.json` wins (ADR 0014) |
| T-13 | Established flows are cut when a grant is revoked | **Resolved 2026-10-28: yes, within about 1 s** (ADR 0014) |
| T-14 | Headscale policy `tests` semantics for tags with no nodes | **Resolved:** canaries only for orgs with registered nodes (ADR 0014) |
| T-15 | `nvidia/cuda` runtime image for Ubuntu 26.04 | Install CUDA runtime libraries per project via pip/uv wheels |
| T-16 | `codex exec-server` as non-root in the container (add-on) | Run it in a dedicated child container with the same mounts |
| T-17 | R2 object lock for audit anchors | Backblaze B2 object lock |

---

## 18. Verification (end to end)

- **M0:**
  - `sudo headscale-failover status --json` shows `"promotable": true`.
  - `systemctl is-active headscale` is `inactive` on the ASUS, and the journal restart counter has stopped.
  - `headscale policy check` plus policy `tests` pass on prod.
  - The reachability matrix script shows providers can't reach `100.64.0.3`, `.4` or `.6`.
- **M1:**
  - `headmesh sandbox up && headmesh sandbox status --json` shows everything healthy and mode=regular.
  - `go test ./sandbox/tests/acceptance -run TestFiveChecks -args -route=mcp` passes.
  - Claude runner dispatch, Codex SSH, Codex exec-server (`headmesh agent run --provider openai`) and ChatGPT tunnel runs each produce a 5/5 JSON.
  - `headmesh sandbox recreate` passes the bonus test.
  - `headmesh sandbox up --yolo` refuses without a fresh restic snapshot, then auto-halts at the TTL and the next `up` is Regular.
  - `claude plugin validate --strict .` passes; the install smoke test is green.
  - headmesh.dev is live; a waitlist double opt-in round trip works.
- **M2:**
  - `go test ./...` and `go test -tags e2e ./test/e2e/...` are green, with revocation p99 < 5 s in the report.
  - `curl -fsSL https://get.headmesh.dev | sh -s -- join hmj_…` works on a fresh `cx23`, and the device appears with its MagicDNS name.
  - The cell agent in observe/enforce on hs1 shows no drift.
  - The first `restore-drill.yml` run is green.
- **M3:**
  - Playwright: sign-up → org → device in under 5 minutes.
  - OAuth conformance script passes.
  - Claude.ai custom connector reaches `https://asus.sbx.headmesh.dev/mcp` via OAuth.
- **M4:**
  - `headmesh cell diff xcelsior` shows no unexpected flow removals.
  - After enforcement, Xcelsior live gates and `tests/test_anchor_workloads.py` pass.
  - A test rental creates a grant, the gateway can reach the job port, and on termination the grant is revoked within 5 s.
  - Provider-to-provider and provider-to-personal probes fail.
- **Ops:**
  - Game day: stop hm-prod-1's network, then confirm the Worker detects it, `recover.yml` fences, restores to fsn1/hel1, sweeps overdue grants and flips DNS within 15 minutes, with the old VM powered off and its IP detached.
  - R2 usage stays under 70% of the free tier.

---

## 19. Decisions and manual to-dos remaining for Aaryn

1. **Approvals needed before execution** (each asked at its step):
   - sudo changes on the ASUS (§5.1, §5.3);
   - the prod ACL and policy change and maintenance window (§5.2, §6.6);
   - prod-hetzner Docker reclaim (**approved 2026-10-26**) and the daemon.json/timer/alert changes (§5.4);
   - installing the cell agent on prod-hetzner (§13.8);
   - switching hs.xcelsior.ca to `policy.mode: database`;
   - the Headscale version pin (v0.29.6 tag vs the `main`/"0.30" build).
2. **Accounts and settings** in §5.5. I can't create accounts or enter credentials.
3. **License: decided 2026-10-28** (ADR 0015, `LICENSING.md`): Apache-2.0 for the CLI, SDKs, plugins, sandbox and web; AGPL-3.0-only for the server and cell agent. Still open: a CLA or DCO before the first external contribution.
4. **Repo visibility: public since 2026-10-28** (`headmesh-dev/headmesh`, published from a history with internal operations details removed; the full history stays in the private `headmesh-archive`). Branch rulesets on `main` are now available and still to be turned on.
5. **Mailbox for sending as aaryn.biro@headmesh.dev** (Zoho free vs Fastmail).
6. **Second DERP location** before public launch (€5.99/month), or relying on Tailscale's public relays until then.
7. **Tokens:** create the Hetzner "Headmesh" project API token (§5.5).
