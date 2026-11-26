# Headmesh: implementation plan to production

- **Date:** 2026-11-18. **Owner:** Aaryn Biro (Xcelsior Compute Inc.). **Status:** proposed.
- **What this is:** the order in which everything the planning documents ask for gets built, and the bar each piece
  must clear before it counts as done. It doesn't replace the design documents. Where a design document and the
  code disagree, this plan says which one wins and what to fix.
- **Sources, read in full:**
  1. `docs/plan.md`: the technical plan (milestones M0–M5, tracks A–G), including the uncommitted §6.16 and T-18
     edits.
  2. `docs/client-roadmap.md`: Track H, clients phases C0–C6 (proposal, 2026-11-14).
  3. `sandbox/docs/adr/0002-self-hosted-mcp-gateway-and-isolation.md`: the third-party MCP gateway (proposed,
     2026-11-16).
  4. `docs/Headscale Sandbox Plugin Handoff.md`: the original brief. It's byte-identical to the tracked
     `docs/handoff.md`.

  Also used: `docs/mcp-oauth.md`, `docs/browser-sign-in.md`, `sandbox/README.md`, `deploy/README.md`,
  `plugins/SUBMISSIONS.md`, ADRs 0001–0015 and the sandbox spike ADRs. Host-specific details (addresses,
  inventories, open findings) stay in the internal ops notes. This file only points at them by section.
- **How to use it:** every task has an ID (`P1.4`, `G7`, `C2.3`). Commits reference it (`feat(api): webhooks
  delivery (impl P1.2)`). §16 maps every item in the four sources to a task ID, so nothing gets lost.

---

## 0. Summary

**Where things stand.** The hard, rare part already works and is live on hs1:

- a deterministic policy compiler with an independent verifier and property tests;
- a cell agent with signed operations, freeze and rollback;
- enrollment, and grants that cut established flows within about 1 s;
- a hash-chained audit log with offline verification;
- rules, services and an access check that explains itself.

Browser sign-in (GitHub, Hugging Face) and an OAuth server for MCP exist. The sandbox is built and verified locally,
and the plugins and marketing site are live.

**What's missing** is what makes it safe to let other people in:

- no job queue, metrics, alerting or isolation prober;
- no off-box backups and no tested recovery;
- no orgs, members or user-scoped API, and no console;
- no device flow;
- the sandbox isn't enrolled yet and hasn't passed its five acceptance routes.

**Phases.** Each phase lists what it unblocks. Lengths assume one full-time engineer with coding agents. A part-time
Apple-platform contractor joins for P5.

| Phase | Closes | Unblocks | Rough length |
|---|---|---|---|
| **P0 Stabilise** | M0/M1 leftovers, work in progress, time-critical dates, long-lead accounts | everything | 2 weeks |
| **P1 Production foundations** | M2 (jobs, webhooks, API completion, observability, backups and recovery, distribution) | Gate B, M4 | 5–6 weeks |
| **P2 Identity, orgs, console MVP** | M3 core, C1 (person-owned devices, device flow, client endpoints) | clients, M4 enforce, Gate B | 5–6 weeks (overlaps P1) |
| **P3 Ingress, OAuth connector, MCP gateway** | M3 remainder, sandbox ADR 0002 | 1-click MCPs, Claude.ai connector | 4–5 weeks |
| **P4 xcelsior cell + Xcelsior** | M4 | the "runs on Headmesh" proof | 3–4 weeks plus maintenance windows |
| **P5 Clients I** | C0, C2, C3 (engine, macOS alpha and beta, console v1) | P6 | 12–15 weeks |
| **P6 Clients II** | C4, C5 (Windows, Linux, iOS, Android) | GA on every platform | 14–20 weeks |
| **P7 Hardening, launch, GA+** | M5, C6 | GA | ongoing |

**Critical path.** Four chains decide the calendar:

1. **Apple enrollment** (D-U-N-S, entitlements) → C2-S1 spike → macOS alpha. It has the longest lead time, so it
   starts in P0.
2. **ADR 0017, person-owned devices** → compiler user principals → C1 exit, **and** M4 enforce. The xcelsior cell's
   personal devices are user-owned nodes, so the compiler must handle user principals before Headmesh can enforce
   that cell (P4.3).
3. **Backups, recovery drills and observability** (P1.6–P1.9) → Gate B. Nobody outside Xcelsior gets an account on
   hs1 until a restore drill is green and paging works.
4. **Spike T-18** → MCP gateway.

**Launch gates** (details in §3.3): **Gate A** internal dogfood (Aaryn and Xcelsior's team on hs1), **Gate B**
public beta sign-up on hs1, **Gate C** general availability.

---

## 1. How this plan uses the sources

- `plan.md` stays the design reference for the server, sandbox and operations. Its milestones M0–M5 map onto
  phases P0–P4 and P7.
- `client-roadmap.md` becomes Track H once its ADRs are accepted (decision D1). C1 shares almost all of its work with
  M3, so P2 does both together.
- Sandbox ADR 0002 is accepted with the amendments in G0 (one container per MCP, a pure-Go rules engine, a dedicated
  scope, stdio servers first, and two new spike checks).
- The handoff brief is covered by `plan.md` §16. This plan adds its open to-dos and success metrics (§16.4).

---

## 2. Baseline (checked in the code, 2026-11-18)

| Area | Built | Missing |
|---|---|---|
| Policy compiler (`core/policy`) | Grants-only output, isolation canaries as Headscale `tests`, independent verifier, `Explain`, preview, timeline, rapid property tests (5,000 checks in CI), `FuzzParsePort` | Perf-budget gate in CI; fuzzing the renderer; long nightly property runs; user principals (P2.5); exit-node invariants (C3) |
| Cell agent and hub (`core/cellagent`, `core/cellserver`) | Hello / Observed / DesiredPolicy / signed Ops over Connect mTLS; observe and enforce modes; invariants; delete and retag rate limits; last-known-good; `freeze`, `unfreeze`, `rollback`, `status` | Local `revoke-grant <id>` and `revoke-all-grants` (plan §9.4); `EnsureUser`; metrics; deployment on prod-hetzner |
| API (`api/`) | Key-scoped `/v1`: `me`, enrollment tokens (create), `enroll`, `complete`, devices (list), grants (create, list, revoke), API keys (create), rules (CRUD, `If-Match`, preview), services, access check, audit (list, verify, export). Route-permission test, committed OpenAPI, oasdiff gate | Orgs, members, invites; device get, patch, delete and expire; token list and delete; approval queue; grant get and extend; API key list and revoke; webhooks; sandboxes; SSE; `Idempotency-Key` (the table exists but nothing uses it); rate limits; cursor pagination beyond audit; SDKs; schemathesis |
| Jobs | A 30-second ticker audits grant expiries | River (pinned in plan §3.2, not in `go.mod`) |
| Webhooks | Schema and sqlc queries (migration 4) | API, signer, delivery worker, SSRF guard |
| Authentication | GitHub and Hugging Face browser sign-in; MCP OAuth server (PKCE, RFC 8707, RFC 9207, CIMD, public DCR, refresh rotation with reuse detection) | Magic link, passkeys, Google and Microsoft sign-in, device flow, `client_credentials`, first-party native clients, user principals on `/v1` |
| Observability | Request-ID and recoverer middleware; slog | `/metrics`, OpenTelemetry export, Alloy, dashboards, SLO alerts, synthetic prober |
| Backups and recovery | Daily local backup timer (14 kept); recovery mode in `headmesh-server serve -recovery` | Litestream, pgBackRest, R2, manifest, detection Worker, fencing tool, `recover.yml`, restore drills, janitor, OpenTofu |
| Audit | Per-org hash chain, verify, NDJSON export, offline verify | Signed daily anchor (T-17) |
| CLI (`cli/`) | `join`, `invite`, `devices ls`, `grant create/ls/revoke`, `rule add/set/ls/rm`, `service add/ls/rm`, `access check`, `audit tail/export/verify`; GoReleaser with cosign, SBOM, provenance, deb and rpm | `login`, `logout`, `whoami`, `status`, `up`, `down`, `switch`, `leave`, `devices rename/rm`, `grant extend`, `rules apply/preview`, `doctor`, `update`; keyring (today only `HEADMESH_API_KEY`); testscript goldens; Homebrew, npm and `install.sh` |
| E2E (`test/e2e`) | Real Headscale and Tailscale: cross-tenant isolation, netmap invisibility, a grant opens exactly its port, established-flow cut, revocation latency | Quarantine of a rogue node; p99 over many revocations (the test fails only above 15 s); nightly runs on Hetzner; mixed node kinds |
| Deploy (`deploy/`) | nginx and systemd blue-green; hs1 on Headscale 0.29.7; cell agent enforcing; certbot DNS-01; Hetzner Cloud Firewall; break-glass WireGuard; `deploy/tests/e2e.sh` | `sandbox-ingress.conf.tmpl` exists but `deploy.sh` doesn't use it; no `*.sbx.headmesh.dev` certificate; Alloy; OpenTofu |
| Sandbox (`sandbox/`) | Image, Regular and YOLO profiles, sandboxd (31 tools), host CLI, acceptance harness, YOLO TTL, OAuth resource server; locally verified; `sandbox-v0.1.0` tagged | Host firewall and backup installed on the ASUS; enrollment; live front doors; five-route acceptance; MCP gateway |
| Plugins | `headmesh` and `xcelsior` 0.1.0, validated, install-smoke CI, directory-submission drafts | Directory submissions and MCP Registry publication (external steps) |
| Marketing | headmesh.dev and waitlist live | `/download` (C3), API reference, "runs on Headmesh" (after M4) |
| Not started | `web/app`, `clients/`, `deploy/{tofu,ha,install}`, `docs/{runbooks,acceptance,threat-model.md}`, `NOTICE`, workflows `e2e`, `deploy`, `recover`, `restore-drill`, `janitor` | |
| Xcelsior repo | The legacy MCP audience sunset is configured for 2026-11-30 | Tailnet policy is still two ACL files with no `grants` and no `tests` (plan §5.2 isn't done); no `control_plane/headmesh/`; migrations are at 163, not the 129 that plan §11 names |

---

## 3. The production bar

### 3.1 Definition of done (every task)

A task is done only when every applicable line holds. Reviewers check this list, not a feeling.

**Code**
- Every I/O call has a context with a deadline, and every outbound HTTP client sets timeouts. No panics on request
  paths; the recoverer is a backstop that raises an alert, not a design.
- Errors are wrapped with `%w`, carry the operation, and never contain secrets. A test feeds the log handler known
  secrets and asserts they don't appear.
- Configuration is validated at start-up, and bad configuration exits non-zero with a precise message.
- Graceful shutdown drains HTTP, SSE, River and the cell streams within the systemd stop timeout. Blue-green depends
  on it.
- Everything is bounded: request bodies, page sizes, goroutines per request, queue depths, response sizes, retry
  counts. Retries use jittered backoff and run only for idempotent operations.
- Risky behaviour sits behind an org setting or a feature flag that defaults to off.

**Data**
- Migrations are goose SQL, tested up → down → up against Postgres 18 in CI.
- **Expand, then contract.** Blue-green runs the old colour against the new schema during a switch, and
  `deploy.sh` rolls the schema back with the colour. So a release may add columns and tables, while dropping or
  renaming waits for the next release.
- Long operations use `CREATE INDEX CONCURRENTLY` and `SET lock_timeout`.
- Every tenant table has `FORCE ROW LEVEL SECURITY` (the existing test enforces it). The app role keeps least
  privilege; River's tables are granted explicitly.

**API**
- Every route declares a permission and scopes (the existing test enforces it). Errors follow RFC 9457.
- `Idempotency-Key` on POST; cursor pagination on lists; `ETag` / `If-Match` on mutable resources.
- Every mutation writes an audit event, and a webhook event where the catalogue (§9.10 of the plan) lists one, in
  the same transaction as the change.
- The committed OpenAPI is regenerated, oasdiff passes (additive only on `/v1`), and the docs page is updated.

**Tests**
- Unit and table tests; failure paths (database down, cell disconnected, Headscale refuses a policy, timeouts).
- **Isolation-relevant changes** (`core/policy`, grants, the cell agent, the gateway firewall generator): property
  tests plus `test/e2e` on real Headscale. This is an `AGENTS.md` rule, not an option.
- Fuzzing for every parser; integration tests on real Postgres; the race detector everywhere; a regression test for
  every bug fixed.

**Security**
- Security-relevant pull requests carry a short threat-model note.
- Any user-supplied URL goes through the SSRF-safe dialer (P1.2).
- Role-by-route authorization tests are generated from the route registry.
- govulncheck, osv-scanner, trivy and gitleaks stay clean. New dependencies follow ADR 0010.

**Operations**
- New subsystems get metrics, at least one alert, a dashboard panel and a runbook entry.
- The rollback path is written down and tested through blue-green.
- Backups cover any new state, and the capacity impact is measured.

**Licensing**
- `cli/`, `cmd/headmesh`, `sandbox/` and `clients/` never import `api/`, `core/`, `internal/` or `db/`. P0.5 makes
  this a CI check.

### 3.2 Battle-tested (required before Gate C)

- Weekly restore drills green for **four consecutive weeks**, plus one full game day with an RTO under 15 minutes.
- The synthetic isolation prober running **30 days** with no P0 page.
- Load tests at **2× the expected launch scale**: API p95 and revocation p99 within their SLOs.
- A 7-day soak on every desktop client: no manual reconnects, crash-free sessions above 99%.
- External penetration test of the API, OAuth and clients, with criticals and highs fixed and retested.
- At least 20 external beta users through Gate B for four weeks, with a support runbook exercised.
- Fuzzing campaigns of at least 24 CPU-hours each on the HuJSON renderer, token parsing, selectors and the MCP
  `tools/list` parser, with no open crashers.

### 3.3 Launch gates

| Gate | Who gets in | Must be true first |
|---|---|---|
| **A: internal dogfood** | Aaryn and Xcelsior's team on hs1 | P0 done. Paging works (P1.6). Litestream and pgBackRest replicate to R2 (P1.7) |
| **B: public beta on hs1** | Anyone through sign-up | Gate A, plus: a restore drill green; `recover.yml` tested once on a game day; prober live; rate limits, capacity limits and abuse controls (P2.9); privacy policy and terms for the hosted service; status page; incident runbook; orgs, members and the console MVP; device approval available |
| **C: GA** | Everyone, including desktop apps | §3.2 complete; own DERP regions (P7.7); client auto-update and signing for every shipped platform; ADR 0024 release trains live |

---

## 4. Decisions needed (with recommendations)

The first eleven come from `client-roadmap.md` §14. The rest come from `plan.md` §19, ADR 0002, or this plan's
review of the code.

| # | Decision | Recommendation | Blocks |
|---|---|---|---|
| D1 | Client strategy | Own apps on the upstream `tailscale.com` library (option C). Official Tailscale apps stay supported as "bring your own client" | P5, P6 |
| D2 | Identity for personal devices | One Headscale user per (person, org); infrastructure stays tagged. Amends ADR 0011 | P2.5, P4.3, C1 |
| D3 | Exit nodes | Allow `autogroup:internet` only with `via` naming the same org's exit nodes, behind invariants I8 and I9 and e2e. Changes the `AGENTS.md` rule, so it needs Aaryn's explicit sign-off | C3.1 |
| D4 | First platform | macOS 14+, Developer ID, Network Extension as a system extension, with a daemon fallback | C2 |
| D5 | Windows and Linux GUI | Go tray app shared by both | C4 |
| D6 | Android | Fork `tailscale-android` | C5 |
| D7 | Downloads | Public; sign-in happens in the app; invite links open the app | C2, P2.8 |
| D8 | DERP | Own regions before GA | P7.7 |
| D9 | Client licence and layout | Apache-2.0 in a separate `clients/` Go module, with `NOTICE` for BSD-3 code | C0 |
| D10 | Accounts | Apple now (critical path); Azure Artifact Signing by about week 6; Google Play by about week 16 | C2, C4, C5 |
| D11 | One CLI or two | One `headmesh` CLI. `cli/` stays where it is until `headmeshd` exists; move into `clients/` in C4 | C0, C4 |
| D12 | Headscale 0.29.7 | Upgrade hs1 first, then the xcelsior cell. Production change: announce, then go-ahead | P0.2 |
| D13 | MCP gateway topology | **One container per MCP** (`sandbox-mcp-<name>`) instead of one shared sidecar. `oci:` packages need their own container anyway, and this isolates the mounts and secrets of one MCP from another | G0 |
| D14 | Tool-poisoning rules engine | Pure Go (RE2 plus structural checks), no cgo YARA. Vendor patterns only after checking their licences | G6 |
| D15 | Scope for third-party tools | A new `sandbox:mcp` scope with an optional per-token allowlist of MCP names. Existing `hms_` tokens and OAuth grants don't get it implicitly | G11 |
| D16 | API path shape | Keep the live key-scoped routes. Add `/v1/orgs/{org}/…` twins served by the same handlers for user principals. One org resolver; a test pairs every route with its twin | P1.3, P2.2 |
| D17 | Console origin | Serve the console at `app.headmesh.dev`, with nginx routing `/v1` and `/oauth` on the same origin. That avoids CORS and keeps `__Host-` cookies | P2.8 |
| D18 | Background jobs | Adopt River now (already pinned in plan §3.2) and retire the ticker | P1.1 |
| D19 | Contributions | CLA if a proprietary or dual-licensed edition of the AGPL parts is ever possible; otherwise DCO. Needed before the first external pull request | P7.3 |
| D20 | Mailbox for sending as aaryn.biro@headmesh.dev | Fastmail (reliable, standard IMAP/JMAP); Zoho's free tier is fine if cost matters more | P0.4 |
| D21 | Second DERP location | Hetzner `hel1` `cx23` before Gate B; full regions before Gate C | P7.7 |
| D22 | xcelsior cell | Approve installing the cell agent in observe mode, then the switch to `policy.mode: database` in a window | P4 |
| D23 | Where the xcelsior import mapping lives | Not in the public repo: sops-encrypted under `deploy/secrets/` or in the internal notes. It names hosts | P4.3 |
| D24 | Third-party MCP transports in v1 | stdio servers first. HTTP and OCI servers only when they can listen on a unix socket, or with a loopback-owner rule (G9) | G9 |

---

## 5. Phase P0: stabilise and close M0/M1 (about 2 weeks)

### P0.1 Repository hygiene and drift

- **Uncommitted policy refactor.** `core/policy/{compile,document,ports,verify}.go` render without reflection
  (`appendDocument`, sort in place). It belongs to whoever started it: ask before touching it (`AGENTS.md`). To
  land it:
  - the fingerprint check over fixed `-rapid.seed` values gives byte-identical hashes before and after;
  - `TestAppendDocumentMatchesEncodingJSON` exists and passes. The comment in `document.go` names it, but no test
    file defines it yet. Write it as a rapid property test: for generated documents, `appendDocument` must equal
    `encoding/json` output byte for byte, and must decode back to the same value;
  - the benchmark result is recorded against the 200 ms budget (plan §9.3);
  - `zz_fingerprint_test.go` and `zz_phase_test.go` are deleted before the commit (both say "Temporary, not
    committed").
- **Uncommitted sandbox image metadata.** The OCI labels and annotations in `sandbox/image/Dockerfile` and
  `docker-bake.hcl`: commit them, then check on the next sandbox tag that GHCR shows title, description and licence.
- **Documents:**
  - commit `docs/client-roadmap.md` (status: proposal), sandbox ADR 0002 (proposed) and the plan's §6.16 and T-18
    edits;
  - delete `docs/Headscale Sandbox Plugin Handoff.md`, a byte-identical duplicate of `docs/handoff.md`;
  - `plan.md` line 3 still names a source path outside the repo: point it at `docs/handoff.md`;
  - `sandbox/README.md` says "the root `go.work` includes it", but no `go.work` exists: fix the sentence, or add a
    committed `go.work` (`.gitignore` only ignores `go.work.local`);
  - `plan.md` §11 names Xcelsior migration `129`; use the next free number at implementation time (currently 164 or
    later);
  - link this file from `plan.md` and `README.md`, and add sandbox ADR 0002 to the index in `sandbox/README.md`.
- **Operations hygiene:**
  - the hs1 cell's one-time admin API key file on hm-prod-1 (`deploy/README.md`, "The hs1 cell"): move it to the
    password manager, then delete the file;
  - rotate the credentials listed under §19 item 7 in the internal ops notes.

### P0.2 Time-critical external dates and versions

- **Xcelsior legacy MCP audience sunsets 2026-11-30** (plan §8.3, item 2). Work in the Xcelsior repo:
  - confirm the `xcelsior` plugin, the quickstarts and every first-party client use the canonical audience;
  - run `tests/test_mcp_connector_flow.py` against it;
  - check connector logs for clients still on the legacy audience and contact them before the date.

  Also close the rest of plan §8.3:
  - **scope gap:** extend the connector ceiling for `ssh:read` only, with consent copy, and add
    `tests/test_advertised_scopes_match_the_connector_ceiling.py`;
  - add a Codex section to `api/docs/mcp-quickstarts.md`;
  - fix the stale `.vscode/mcp.json` path.
- **Headscale 0.29.6 → 0.29.7** (patch and security release, 2026-10-30; D12):
  1. Read the release notes for database migrations and changed APIs. Diff `proto/headscale/v1` between the tags
     and regenerate `gen/headscale` if needed.
  2. Bump every pin: `HEADSCALE_VERSION` and its checksum in `deploy/server/bootstrap.sh`; `headscaleImage` in
     `test/e2e/isolation_test.go`; the version string in `cmd/headmesh-cellagent/main_test.go`; both checks in
     `deploy/tests/e2e.sh`; the comment in `proto/buf.gen.headscale.yaml`. `grep -rn 0.29.6` must come back empty
     apart from history.
  3. Run `deploy/tests/e2e.sh`, then deploy to `--env test`, then announce and upgrade prod hs1 (back up first with
     `deployctl backup`; keep the 0.29.6 package for rollback).
  4. The xcelsior cell on prod-hetzner follows, which needs Aaryn's go-ahead.

  **Done 2026-11-18.**
  - hs1 runs v0.29.7. The bootstrap now takes an online, integrity-checked database backup before each package
    upgrade, and `deploy/tests/e2e.sh` covers the 0.29.6 → 0.29.7 upgrade path.
  - The xcelsior cell (prod-hetzner and the ASUS standby) runs upstream `main` at `545ace56`, which carries all six
    0.29.7 security fixes. It stays off the release tag because its database already came from a `main` build
    past v0.29.6. Return it to a tagged release when one includes that build.
  - Details, backups and rollback are in the internal ops notes (§5.9).
- **Tailscale 1.104.2 → 1.104.7**: `tailscaleImage` in `test/e2e/isolation_test.go`; the sandbox
  `TAILSCALE_IMAGE` tag **and digest** plus the `tailscale version` check further down
  `sandbox/image/Dockerfile`; `TAILSCALE_VERSION` in the base-host bootstrap `deploy/compose/bootstrap.sh` (used
  on hosts that are already running, so roll it out with an announced apt upgrade, not a re-bootstrap). Find out
  why Renovate didn't propose it, and fix that in P0.5.

  **Done 2026-11-18** for the pins and for tailscaled on prod-hetzner, hm-prod-1 and the ASUS. P0.5 answers
  the Renovate question.
- **Hetzner credits:** check their expiry date in the Console, then schedule the P1.11 load tests and the first
  e2e runs on hourly VMs before it.

### P0.3 Sandbox go-live (closes M1)

In this order. Each step that touches production, sudo or accounts is Aaryn's or needs his go-ahead.

1. **Host egress firewall** (`sandbox/host/firewall`, sudo). Install it before anything is enrolled.
   - Prove it from inside the container: `nc` to LAN addresses, to the host's own services and through the host's
     tailnet must fail; the internet and Docker DNS must work.
   - `headmesh-sandbox doctor` shows `egress firewall ✓`.
2. **Home backup** (`sandbox/host/backup`, sudo): install the units, take the first restic snapshot, and schedule
   the monthly `restic check --read-data-subset=5%`.
3. **Secret masking:**
   - run `headmesh-sandbox scan-secrets`, review, then `--apply`;
   - make sure each mounted project's root `.env` files are masked, including headmesh's own;
   - add sensitive data directories from the internal notes to `[mounts] extra_denylist`;
   - add `.venv-sandbox` to every mounted project's `.gitignore`.
4. **Xcelsior tailnet hardening** (plan §5.2; Xcelsior repo; production ACL change, so a maintenance window). It's
   not done: the cell still runs two ACL files without `grants` or `tests`.
   - Write one `api/infra/headscale/policy.hujson` with `grants`, tags (`tag:xc-control`, `tag:xc-host`,
     `tag:sandbox`, `tag:sandbox-yolo`), the Tailscale SSH rule for `agent`, and a `tests` block encoding the
     reachability matrix.
   - Retag provider nodes. Set `XCELSIOR_HEADSCALE_ACL_TAGS`.
   - Add the apply script that runs `headscale policy check` first.
   - Bind container SSH ports to the mesh IP, then re-sign the agent.
   - Verify with the live gates, `tests/test_anchor_workloads.py` and a reachability matrix from each node class.
     Rollback is the previous file.
   - Shape it so the P4.3 import mapping maps 1:1 onto it.
5. **Enroll `asus-sandbox`** with a single-use key carrying `tag:sandbox`, then empty `secrets/ts_authkey`. `status`
   shows the tailnet running and `dns-check` passing.
6. **Bring each front door up** (`sandbox/README.md`, step 4) and close the open live checks:
   - Claude runner: what it puts in the sessions' environment (the residue of T-1);
   - Remote Control restarts without a TTY after login (T-2);
   - Codex desktop over Tailscale SSH (T-3; fallback: OpenSSH as uid 1000 on `:2222`, tailnet IP only);
   - tunnel `doctor` is green;
   - an `exec-server` session completes.
7. **Acceptance** (plan §6.15):
   - all five checks through each of the five routes (MCP, Claude runner, Codex over SSH, Codex `exec-server`,
     ChatGPT through the tunnel), plus the recreate bonus;
   - `acceptance verify`, then `acceptance report --out docs/acceptance/<date>.md` (create the directory);
   - record how Claude and Codex show diffs for this setup (the handoff's last open to-do);
   - file an issue for each failure. M1 exits only at 5/5 on every route.
8. **YOLO:**
   - create the dedicated `ed25519` deploy key and pin `known_hosts`;
   - take a backup, then `up --yolo --ttl 4h`;
   - enroll `asus-sandbox-yolo` with `tag:sandbox-yolo`;
   - confirm the TTL halt and that the next `up` is Regular.
9. **Image:** check that `sandbox-v0.1.0` exists on GHCR with a valid cosign signature, SBOM and provenance
   (`cosign verify --certificate-identity-regexp … --certificate-oidc-issuer https://token.actions.githubusercontent.com`).
   Pin it by digest in `sandbox.toml`, and cut `0.1.1` with the P0.1 metadata.
10. **Spike T-15** (optional `-cuda` tag on `nvidia/cuda` for Ubuntu 26.04), and BIOS "power on after AC loss" (by
    hand).

**Done when** plan §18 "M1" holds in full, the acceptance report is committed, and `doctor` is clean.

### P0.4 Accounts and settings (Aaryn, by hand)

Long-lead items first, because they gate later phases:

| Item | Why now |
|---|---|
| **Apple Developer Program**, organization enrollment for Xcelsior Compute Inc. (existing D-U-N-S); then the capabilities (Network Extensions packet tunnel, System Extension, Keychain sharing, App Groups) and a request for the `-systemextension` packet-tunnel entitlement if TN3134 requires it; an App Store Connect API key | C2 can't start without it (days to weeks) |
| **Google Play** organization account (US$25 once; D-U-N-S plus a website verified in Search Console). Android developer verification goes global on certified devices in 2027, so even an APK distributed outside Play needs a verified developer and a registered package name and signing key | C5 (about week 22); verify identity by about week 16 |
| **Azure Artifact Signing** (renamed from Trusted Signing): a paid subscription (free and trial ones are refused), Basic SKU, organization identity validation for Xcelsior Compute Inc. (Canada is eligible; 1–30 business days; the legal name becomes the certificate CN) | C4 (about week 12); start by about week 6 |
| **GitHub org:** require 2FA, base permissions none, Actions allow-list, secret scanning and push protection, private vulnerability reporting; **branch rulesets on `main`** blocking force-push and deletion (possible now the repo is public) | Supply chain |
| **Hetzner** "Headmesh" project API token, stored only as the `recovery` environment secret and in sops; note the server limit from Console → Limits (spike T-11) | P1.7, P1.8 |
| **Cloudflare:** DNSSEC (DS record at Porkbun); R2 enabled; scoped tokens (P1.7) | P1.7 |
| **Grafana Cloud** free stack for Headmesh; a **Telegram** bot and chat for alerts | P1.6 |
| **npm:** claim `headmesh` with a 0.0.0 placeholder through trusted publishing; **PyPI:** trusted publisher for `headmesh` | P1.10, P4 |
| **Claude Team:** self-hosted environments, Remote Control, cloud sessions, GitHub connector. **OpenAI Platform:** tunnel and a Read+Use runtime key; Agents API application and environment keys | P0.3 |
| Mailbox for sending (D20); check Porkbun's .dev renewal price | §19 |

### P0.5 CI and supply-chain gaps

- **Licence boundary check** (`LICENSING.md`: "a test in CI could enforce this later"). Add a Go test in
  `ci-go` that runs `go list -deps` for `./cli/...`, `./cmd/headmesh`, the sandbox module and, later, `clients/`,
  and fails on any `github.com/headmesh-dev/headmesh/{api,core,internal,db}` import.
- **Renovate custom managers** for the pins it can't see: `HEADSCALE_VERSION` in `deploy/server/bootstrap.sh`, the
  image constants in `test/e2e`, Dockerfile `ARG`s (Tailscale, tunnel-client, Claude Code, Codex, gitleaks, mise),
  certbot requirements. Add a test that each pin has a Renovate annotation.
- **Fuzzing:** add `FuzzRender` (renderer against `encoding/json`), `FuzzParseSelector`, `FuzzTokenParse`
  (`hmj_`, `hmk_`, `hms_`) and `FuzzVerify` (random tampering never passes). Thirty seconds each on every PR;
  a nightly job runs them longer and adds `-rapid.checks=100000` for the property tests.
- **`buf breaking`** against `main` for `proto/cell/v1`, so a cell agent one release behind keeps working
  (blue-green and rolling agent upgrades).

**Done (2026-11-25).**

- **Licence boundary:** `TestPermissiveCodeNeverImportsAGPLCode` in [`test/repo`](../test/repo/) walks the full Go
  dependency graph, tests included, of the CLI, `cmd/headmesh`, the generated cell and Headscale code and the sandbox
  module. A control test confirms it does see the AGPL imports of `cmd/headmesh-server`.
- **Renovate:** it missed Tailscale 1.104.7 because the base-host pin had no annotation and `config:recommended`
  ignores `test/` and `tests/`, which hid the e2e image. A custom regex manager now reads `# renovate:` annotations,
  and 44 pins carry one. `TestEveryPinIsVisibleToRenovate` runs Renovate's own regex over every version in scripts,
  Dockerfiles, Go image constants and workflows. Groups move Headscale, Tailscale, certbot, the code generators,
  golangci-lint, gitleaks and pnpm in one PR each. Pins with a checksum next to them get a PR note to update it.
  The certbot lock header is in the form pip-compile can replay, and the workflow Node and pnpm pins must match
  `mise.toml` and `package.json`.
- **Fuzzing:** `FuzzRender`, `FuzzVerify`, `FuzzParsePort`, `FuzzParseSelector`, `FuzzParseInvite`,
  `FuzzSecretMatches` (`hmj_`, `hmk_`, `hmc_`) and `FuzzStoreVerify` (`hms_`) run 30 seconds each on PRs and
  10 minutes each nightly. The nightly job also runs the policy properties at 100,000 checks and opens a tracking
  issue on failure. A test keeps both lists equal to the `Fuzz*` functions. Fuzzing found that `ParseSelector`
  accepted `all:` and `users:`; that is fixed.
- **`buf breaking`** runs on every PR against its base branch.
- **Found on the way:** the sandbox module's tests and lint never ran in CI. `ci-go` has a job for them now.
- **Open:** the first hosted CI run, and installing the Renovate app on the org.

### P0.6 prod-hetzner leftovers (plan §5.4; Xcelsior repo; approvals)

- Install the validated `daemon.json` (`local` log driver, BuildKit GC) in a window. It restarts dockerd, which
  restarts every container.
- Move image builds off prod: build on the ASUS runner, push to GHCR by digest, and have prod only pull.
- Confirm the disk alerts (80% warning, 90% page) exist for `/` and the loop-volume backing files. If they don't,
  add them.
- The move of the loop volumes to a Hetzner Volume stays a separate decision; list it in §17.

---

## 6. Phase P1: control-plane production foundations (finishes M2, about 5–6 weeks)

The order inside P1 is deliberate: **P1.6 and P1.7 come first**, because nothing else matters if the only copy of
the control plane is lost.

### P1.1 Background jobs on River

- **Build:**
  - add `riverqueue/river` with `riverpgxv5`, pinned per plan §3.2 or a newer stable per ADR 0010;
  - River's migrations run inside `headmesh-server migrate` with its migrator, as their own versioned step after
    goose, with grants for the app role;
  - one River client per process; the periodic jobs use leader election, so blue-green's brief overlap of two
    colours is safe;
  - job argument structs carry a `v` field, so the old colour can still decode jobs the new colour enqueued.
- **Jobs:**
  - grant boundary events (activated and expired: audit, webhook, SSE). Enforcement itself stays in the policy
    timeline, so a dead queue never delays a revocation;
  - webhook delivery (P1.2), the enrollment reconciler (P1.4), the audit anchor (P1.9) and the R2 usage probe
    (P1.7);
  - clean-ups for expired enrollments, idempotency keys, login flows and browser sessions;
  - membership expiry (P2.2).
  - The 30-second ticker sweep becomes a periodic job. Recovery mode keeps its synchronous sweep at start-up.
- **Tests (integration, Postgres 18):**
  - uniqueness under concurrent enqueue;
  - at-least-once delivery: kill a worker mid-job and the handler stays idempotent;
  - retry schedule;
  - shutdown drains within the stop timeout;
  - a job enqueued by version N is processed by N−1 and by N+1.
- **Done when** no `time.NewTicker` remains in `cmd/headmesh-server`, and the River dashboard metrics (queue depth,
  job latency, failures) are exported (P1.6).

### P1.2 Webhooks end to end (plan §9.10)

- **API:** `POST|GET /v1/webhooks`, `GET|PATCH|DELETE /v1/webhooks/{id}`, `POST /v1/webhooks/{id}/test`,
  `GET /v1/webhooks/{id}/deliveries`, `POST /v1/webhooks/{id}/deliveries/{did}/replay`, plus the org-scoped twins
  (D16). Permission: `webhooks:write`, admin and above.
- **Signing:** Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`, HMAC-SHA256), using the
  official Go library.
  - Secret rotation keeps two secrets valid for 24 h, and both signatures go in the header.
  - Secrets are stored encrypted with a server key, not as plaintext.
- **Delivery:** River with exponential backoff from 1 minute to 24 h, 10 attempts. An endpoint is disabled after 24 h
  of failures (`webhook.endpoint_disabled` audit entry and an email to org admins). Delivery is at-least-once with no
  ordering guarantee, and the docs say so.
- **Outbox:** deliveries are enqueued in the same transaction as the audit event (the existing
  `EnqueueWebhookDeliveries`), so an event can't be lost or invented.
- **SSRF guard** (shared helper `internal/netguard`, reused by anything that dials user-supplied URLs):
  - HTTPS only; no redirects; 10 s total timeout; response bodies capped at 64 KiB and never stored in full.
  - A custom `net.Dialer.Control` checks the **resolved** address at connect time, which also defeats DNS
    rebinding. It refuses loopback, RFC 1918, link-local, CGNAT `100.64.0.0/10`, ULA, IPv4-mapped forms of all of
    these, multicast, unspecified, the metadata range and hm-prod-1's own addresses.
- **Event catalogue:** `org.*`, `member.*`, `device.enrolled|renamed|removed|online|offline|quarantined`,
  `grant.*`, `rule.changed`, `policy.applied|failed|drift_corrected`, `sandbox.mode_changed`, `apikey.*`,
  `access_request.*` (P2.6). Payloads are versioned (`"type"`, `"version": 1`).
- **Tests:**
  - the Standard Webhooks test vectors;
  - an SSRF table of more than 40 cases (decimal and octal IPs, `[::ffff:127.0.0.1]`, a rebinding resolver);
  - the retry schedule as a property test; disable after 24 h; replay is idempotent;
  - a receiver verifies signatures with an independent implementation.

### P1.3 API completion (M2 surface)

- **Endpoints:**
  - `GET|PATCH|DELETE /v1/devices/{id}` (rename, roles) and `POST /v1/devices/{id}/expire`. Delete goes through the
    cell agent's `DeleteNode` and its guardrails; it's audited and sends a webhook.
  - `GET|DELETE /v1/enrollment-tokens[/{id}]`.
  - The approval queue (`approval_required` already exists as `pending_approval`):
    `GET /v1/enrollment-requests`, `POST /v1/enrollment-requests/{id}/approve|reject`.
  - `GET /v1/grants/{id}` and `POST /v1/grants/{id}/extend`, bounded by the org's `max_grant_ttl` and idempotent. It
    recompiles the timeline.
  - `GET /v1/api-keys`, `DELETE /v1/api-keys/{id}`, key expiry, and sampled `last_used_at` updates (at most once per
    minute per key, to avoid write amplification).
- **Conventions:**
  - **`Idempotency-Key`** middleware on every POST, using `idempotency_keys` and the IETF httpapi draft semantics:
    - scoped to principal, method, path and body hash, for 24 h;
    - a replay returns the stored response;
    - the same key with a different body returns 422;
    - a concurrent duplicate returns 409 while the first is in flight (an insert-first row lock).
  - Opaque cursor pagination (`created_at`, `id`; `limit` ≤ 200) on every list.
  - **Rate limits:** token buckets per principal in memory for general traffic, with `RateLimit-Policy` and
    `RateLimit` headers, and 429 with `Retry-After`. **Security-sensitive** limits (sign-in, magic links, device
    codes, `enroll` per token and per IP) are Postgres-backed, so they survive restarts and colour switches.
- **Contract:**
  - OpenAPI committed; oasdiff gate unchanged.
  - Generated SDKs: TypeScript (`openapi-typescript` and `openapi-fetch` → `@headmesh/sdk`) and Python
    (`openapi-python-client` → `headmesh` on PyPI, consumed by Xcelsior in P4). Both are built and checked for
    staleness in CI.
  - **schemathesis** runs against an ephemeral server with Postgres in CI, with stateful links on.
  - The API reference goes on headmesh.dev with `starlight-openapi` (plan §12.1).

### P1.4 Enrollment reconciler and quarantine

- A periodic job per cell compares the cell agent's `Observed` nodes with `devices`:
  - **minted but never completed:** attach the device using the pre-auth key ID;
  - **unknown node in an enforce cell:** `SetTags` to `tag:hm-quarantine`, then audit, a webhook
    `device.quarantined` and an alert;
  - **removed device still registered:** `DeleteNode` through the guardrails.
- Add the local cell-agent commands from plan §9.4 that don't exist yet: `revoke-grant <id>` and
  `revoke-all-grants`, working from the cached grant-to-rule map while the API is unreachable, with audit entries
  queued for the next sync.
- **e2e:** register a node straight through Headscale with a key Headmesh didn't mint. It must be quarantined within
  60 s, and while quarantined it can't reach anything and nothing can reach it.

### P1.5 Policy compiler gates

- **Performance budget** (plan §9.3): compiling 5,000 devices, 2,000 orgs and 10,000 grants takes under 200 ms.
  - Land P0.1's refactor.
  - The CI job runs the benchmark with `-count=10` and `benchstat` against the baseline from `main`. It fails over
    budget or on a regression above 20%.
- **Size bound I7:** add a nightly e2e that loads the policy for the largest supported cell into a real Headscale.
  It proves `SetPolicy` and `tests` accept it within the timeout.
- **Fuzzing** and long property runs from P0.5.

### P1.6 Observability and alerting (plan §9.11)

- **Metrics:** `prometheus/client_golang` on a separate loopback listener in `headmesh-server` and
  `headmesh-cellagent`. Export the plan's metric names, plus RED metrics per route, pgx pool statistics, River
  queues and the Headscale `:9090` scrape.
- **Traces:** OpenTelemetry SDK (`otelhttp`, an `otelpgx` tracer, River middleware), OTLP to Alloy. Sample 10%, and
  keep every error.
- **Logs:** slog JSON with `request_id`, `org_id` and `trace_id` on every line. A test asserts the fields.
- **Shipping:** Grafana Alloy as a systemd unit installed by `deploy/server/bootstrap.sh`, sending to the Grafana
  Cloud stack. Dashboards and alert rules live as code in `deploy/grafana/`.
- **Alerts** (multi-window burn rates where there's an SLO), to Telegram and email:
  - API availability 99.9%; enrollment p95 under 15 s; revocation p99 under 5 s; drift corrected within 2 minutes;
  - cell last sync over 60 s; policy apply failures;
  - certificates expiring within 14 days (TLS, the cell CA, cell client certificates);
  - disk over 80%; backup or WAL archive age; R2 usage over 70%; River failures; disabled webhook endpoints.
- **Synthetic isolation prober** (`cmd/headmesh-prober`, AGPL side):
  - two probe orgs with two userspace-networking nodes each, as containers on hm-prod-1;
  - **every minute:** assert cross-org traffic is denied;
  - **every 10 minutes:** create a probe grant, measure enforcement, revoke it, measure again. That feeds
    `headmesh_revocation_enforced_seconds`;
  - **any unexpected cross-org success pages as P0.**
- **Status page:** the detection Worker (P1.7) also publishes a small status JSON and page at
  `status.headmesh.dev`. Needed for Gate B.

### P1.7 Backups, replication and disaster recovery (plan §13.3–§13.5, §13.7)

- **R2:** buckets `hm-hs1-litestream`, `hm-pg`, `hm-keys`, `hm-tofu` and `hm-audit-anchors`, each with its own
  scoped token (P1.8 creates them).
- **Headscale SQLite:** Litestream with a 10 s sync interval and a 24 h snapshot, as a systemd unit. Spike T-10
  measures a week of PUTs and tunes the interval.
- **Postgres 18:** pgBackRest with WAL archiving (`archive_timeout=300`), a weekly full and daily differential
  backup, 4 fulls retained, and `repo1-cipher-type=aes-256-cbc` with the key in sops. The existing local daily
  backup stays as a second copy.
- **Keys:** the Noise and DERP private keys and the Headscale configuration are age-encrypted to two recipients
  (Aaryn and the recovery key) and stored in `hm-keys`, and sops-encrypted in `deploy/secrets/*.enc.yaml`. Never in
  plaintext.
- **Manifest:** every 5 minutes, a periodic job writes `manifest.json` to R2: node and user counts, the Noise
  public-key fingerprint, the policy hash, the last grant boundary and the Postgres LSN.
- **Spike T-9:** are pre-auth keys stored hashed in Headscale's database? Record the answer in an ADR.
- **`deploy/ha/` (`headmesh-recover`, Go, AGPL side, using hcloud-go and cloudflare-go):**
  1. **Fence.** Power off, wait for `off`, label `fenced=true`, detach the primary IPv4. If `off` can't be
     confirmed: abort and page. Never two control servers with the same keys.
  2. **Capacity preflight.** Check the configured server limit; try the fallback chain
     `cx23@fsn1 → cx23@hel1 → cx33@fsn1 → cx33@hel1 → cpx21@fsn1 → cax11@fsn1`.
  3. **Create** the VM with cloud-init in restore mode (`deploy/server/bootstrap.sh restore`; ADR 0013 replaced the
     Compose path the plan describes).
  4. **Restore and verify.** `PRAGMA integrity_check`; node count at least the manifest's; Noise fingerprint equal
     to the manifest's; DERP key present; pgBackRest to the latest point.
  5. **Recovery mode** (already built): sweep overdue grants, apply to every cell, wait for acknowledgements.
  6. **Certificates and proof.** Get certificates by DNS-01 before DNS moves, and prove the new VM serves hs1 on
     443 with `--resolve`.
  7. **Flip DNS** (grey-cloud, TTL 60).
  8. **Post-checks.** An ephemeral client registers; node count and policy hash match.
  9. **Notify** with an RTO report. Set a 24 h reminder to delete the fenced VM, which still bills.

  It ships with the table-driven tests ported from Xcelsior that plan §13.4 lists (empty replica, missing Noise
  key, threshold, healthy primary, can't serve the name, no automatic failback, diverged databases, different Noise
  keys, force required, overlapping refreshes, empty snapshot, page once), plus three new ones: fencing must
  confirm `off`; recovery mode blocks writes; the fallback chain advances on `resource_unavailable`.
- **Detection:** a Cloudflare Worker with a 1-minute cron probes hs1 `/health` and the API `/readyz`. After 3
  failures it sends `repository_dispatch` with a dispatch-only token.
- **`recover.yml`:** runs in the `recovery` environment, in a single concurrency group, with a 30-minute timeout. It
  first re-confirms the outage from GitHub's network.
- **`restore-drill.yml`:** weekly on Mondays at 06:00 UTC, with the checks in plan §13.5 and RTO and RPO recorded as
  metrics. **`janitor.yml`:** hourly; deletes `ephemeral=true` servers older than 2 h.
- **Scope:** the test environment isn't recovered (it's rebuilt), which keeps `recover.yml` simple.
- **Break-glass:**
  - quarterly drill (reach the VM with tailscaled stopped);
  - `deployctl breakglass-check` reports the WireGuard handshake age. Plan §13.7 puts this in
    `headmesh doctor --breakglass`, but it needs root on the server, so it belongs in `deployctl`;
  - `docs/runbooks/breakglass.md`.
- **Done when** the first `restore-drill.yml` run is green (M2 exit) and one game day has passed (Gate B).

### P1.8 Infrastructure as code (plan §13.2)

- `deploy/tofu/`:
  - **Hetzner:** server, primary IPv4, firewall, SSH keys and labels. `tofu import` the server and firewall that
    were made by hand.
  - **Cloudflare:** DNS (hs1 grey-cloud), R2 buckets with scoped tokens, Turnstile, Email Routing, and the detection
    Worker.
  - State in R2 with OpenTofu native state encryption.
- **CI:** `tofu plan` on every pull request that touches `deploy/tofu/`; apply manually in a protected `infra`
  environment; a weekly drift check that opens an issue.

### P1.9 Audit daily anchor (plan §9.7, spike T-17)

- A daily job signs each org's chain head with a dedicated Ed25519 anchor key, not the op-signing key. The manifest
  goes to `hm-audit-anchors` with R2 object lock in compliance mode (400-day retention), or to Backblaze B2 object
  lock if T-17 fails.
- The public key is published at `/.well-known/headmesh-audit-keys`. `headmesh audit verify --anchors DIR` checks a
  local export against downloaded anchors offline.
- **Tests:** tampering is detected; a missing anchor warns; key rotation keeps old anchors verifiable.

### P1.10 CLI and distribution (M2 exit: `curl … | sh -s -- join hmj_…` on a fresh `cx23`)

- **`install.sh`** at `get.headmesh.dev` (an nginx vhost on hm-prod-1):
  - POSIX `sh`; detects OS and architecture;
  - downloads the release and verifies its sha256 **and** its cosign signature. It fetches a cosign binary whose
    checksum is pinned in the script, and verifies the certificate identity of the release workflow and its OIDC
    issuer;
  - `--no-verify-signature` is an explicit, loud opt-out;
  - tested with shellcheck and bats across Ubuntu 26.04 and 24.04, Debian, Fedora, Alpine and macOS.
- **Packages:**
  - Homebrew tap `headmesh-dev/homebrew-tap` through GoReleaser;
  - npm `@headmesh/cli` with per-platform `optionalDependencies`, plus the `headmesh` alias, through trusted
    publishing with provenance;
  - `go install` works.
- **Commands:**
  - `status [--json]`; `leave` (logs out the instance and removes the device); `devices rename|rm`;
    `grant extend`;
  - `rules apply -f FILE [--prune] [--dry-run]`: declarative, showing the preview diff first;
  - `doctor`: instance health, MagicDNS, API reachability, clock skew, versions;
  - `update`: self-update with cosign verification, deferring to the package manager when installed by one;
  - shell completions documented.
  - `login`, `logout`, `whoami`, `up`, `down` and `switch` come in P2.7.
- **Credentials:** the OS keyring through go-keyring, with a 0600 file fallback. `HEADMESH_API_KEY` still wins for
  CI.
- **Tests:** testscript golden tests for every command; a nightly `join` on an ephemeral VM (P1.11).

### P1.11 End-to-end and load testing

- **`e2e.yml`** nightly on ephemeral Hetzner `cx23` VMs (real NAT and DERP), with the janitor as backstop. Matrix:
  Headscale {current, next} × Tailscale {pinned, latest}.
- **Revocation SLO, measured properly:** 20 revocations per run, with a sub-second probe. Compute p99 and fail at
  5 s or more. The current test only fails above 15 s, with a probe timeout of up to 5 s.
- **New cases:**
  - quarantine (P1.4); the approval flow; device removal; grant extend;
  - a scheduled grant start and expiry applied by the cell agent **while `headmesh-server` is stopped** (the policy
    timeline);
  - freeze and rollback;
  - `install.sh` join on a fresh VM.
- **Load (plan §9.12):**
  - k6 against the API;
  - map-update fan-out with up to 500 real `tailscale` containers on hourly `cx33` VMs, on the Hetzner credits before
    they expire.
  - Record the measured hs1 limits (starting at 1,000 nodes and 300 orgs) in an ADR, and **enforce** them: creating
    an org or enrolling a device past a cell's limit returns 409 with a clear message.

---

## 7. Phase P2: identity, orgs, user-facing API and console MVP (M3 core + C1, about 5–6 weeks)

Write **ADR 0017** (person-owned devices), **ADR 0020** (native-app OAuth) and the API path ADR (D16) before any
code.

### P2.1 Users, sessions and sign-in methods (plan §9.9)

- **Existing:** GitHub and Hugging Face, explicit account linking, hashed 12-hour browser sessions.
- **Add:**
  - **Google** OIDC.
  - **Microsoft** (Entra `common`). Validate the tenant-specific issuer from the `tid` claim; a static issuer check
    fails for multi-tenant apps.
  - **Email magic link** through Resend: single use, 15 minutes, stored hashed. Login CSRF protection: the link
    completes only in the browser that asked for it, otherwise the user types a 6-digit code. Turnstile on the
    request form, plus Postgres-backed per-address and per-IP limits.
  - **Passkeys** with go-webauthn: discoverable credentials, attestation `none`, RP ID `headmesh.dev`.
- **Sessions:** `__Host-hm_session` (Secure, HttpOnly, SameSite=Lax); Go's `http.CrossOriginProtection`; rotation on
  sign-in and on any privilege change; an idle timeout on top of the 12 h limit.
- **Account lifecycle:** email verification before a device can be created; self-service account deletion and data
  export (PIPEDA, and GDPR because hs1 runs in the EU); recovery when every sign-in method is lost, through support
  with a documented identity check.

### P2.2 Orgs, members and invites (plan §9.8, client roadmap C1)

- **Schema:** an `invites` table (token hash, optional email, role, `expires_at`, `max_uses`).
  - Typed org settings: `allow_subnet_routes`, `allow_exit_nodes`, `max_grant_ttl`, `ssh_enabled`,
    `device_approval_required`, `personal_key_expiry_days` (default 180).
  - Slug rules: DNS-safe, at most 24 characters, a reserved-word list.
- **Endpoints:**
  - `GET /v1/me` now also describes users, plus `GET /v1/me/orgs`;
  - `POST|GET /v1/orgs`, `GET|PATCH|DELETE /v1/orgs/{org}`, `POST /v1/orgs/{platform}/children`;
  - members CRUD with `expires_at` (contractors; a River job expires them);
  - `POST /v1/orgs/{org}/invites`, `POST /v1/invites/{token}/accept`;
  - org-scoped twins of every P1.3 route (D16).
- **Rules:**
  - A personal org is created at the first sign-in.
  - The last owner can't be removed or demoted.
  - Deleting an org is a soft delete: remove its devices through the cell agent, revoke its grants, audit, then a
    purge job runs 30 days later.
  - A removed member's personal devices in that org are expired and removed (P2.5).
- **Authorization:**
  - one role × permission matrix (owner, admin, member, viewer, billing);
  - API keys can't carry more than their creator's role;
  - platform scopes `platform:orgs` and `platform:grants` act on child orgs only (a parent check in SQL plus RLS);
  - `SET LOCAL headmesh.org_ids` comes from the membership set for user principals;
  - **generated tests** cover every route × every role × own org, other org and child org.

### P2.3 OAuth server extensions

- **Device flow (RFC 8628)** for `headmesh-cli`:
  - user codes with 8 base-20 characters and no ambiguous letters; 10-minute expiry; `slow_down` honoured; rate
    limits;
  - the verification page in the console names the client, shows the requesting IP's coarse location, and asks for
    an explicit confirmation (phishing defence).
- **First-party public clients:**
  - `headmesh-desktop`: PKCE; the claimed `headmesh://` scheme on Apple; loopback `127.0.0.1` with any port on
    Windows and Linux (RFC 8252, exact match apart from the port);
  - `headmesh-cli` (device flow). Both are pre-registered and get an account and org chooser instead of a consent
    screen.
- **`client_credentials`** for platforms (Xcelsior), with `private_key_jwt` preferred over a client secret, and
  `platform:*` scopes.
- **The API as a resource server:** the security middleware accepts OAuth access tokens with audience
  `https://api.headmesh.dev` beside `hmk_` keys, and maps scopes to permissions.
- **Conformance:** a script modelled on Xcelsior's `gx0_conformance.py`, plus the OpenID conformance suite's OP
  profile in Docker as a nightly job.

### P2.4 Discovery and client configuration

- `GET /.well-known/headmesh`: issuer, API base, OAuth endpoints, `min_client_version`.
- `GET /v1/client/config`, per org the user belongs to: login server, MagicDNS suffix, feature flags (exit nodes,
  SSH, file sharing), DERP policy, minimum and recommended client versions. Each org becomes one engine login
  profile.

### P2.5 Person-owned devices (ADR 0017): isolation-critical

- **Spike C1-S1** (from client roadmap §4.5): does Headscale keep the same node when a known machine key logs in with
  a new pre-auth key, and how does it set expiry on user nodes registered with a pre-auth key? Also check the
  Headscale user-name syntax and limits for `hm-u-<user10>-<org10>`. Headscale 0.29.7 changed two related rules,
  and the spike must confirm both on 0.29.7: another user's key or login on a tagged machine now creates a new
  device for that user, instead of re-authenticating the tagged node; and an expired tagged node needs a valid
  pre-auth key to re-register.
- **Compiler:**
  - an org's principals become "org tags ∪ that org's Headscale users";
  - restate invariants I1–I7 over both kinds; the rapid generators produce mixed tagged and user-owned nodes;
  - the verifier re-derives cross-org edges for both kinds; canaries cover user principals;
  - `autogroup:self` and `autogroup:member` stay out of tenant output.
- **Cell protocol:**
  - `EnsureUser{name, display_name, email}`;
  - `CreatePreAuthKey{user, …}` with no tags;
  - `DeleteUser`, refused while the user owns nodes;
  - guardrails and the invariants file extended.

  `buf breaking` must pass (old agents ignore the new ops), and the server refuses user ops to agents that are too
  old to understand them.
- **Migration:** `devices.kind` gains `personal` and `devices.owner_user_id`; a new
  `headscale_users(cell_id, user_id, org_id, name, headscale_user_id)` table. Expand only.
- **Key expiry and re-auth:** the org setting applies to personal devices only (tagged devices never expire). The
  client re-runs registration in the background while its refresh token is valid; otherwise it asks the user to
  sign in again.
- **e2e:**
  - the isolation matrix with mixed node kinds;
  - **a person in two orgs on the same cell**: the org A laptop can't reach org B's devices, and the reverse;
  - Taildrop works between the same user's devices and fails across orgs;
  - the revocation SLO is unchanged.
- **Gate:** Aaryn's sign-off on ADR 0017; property tests and e2e green.

### P2.6 Product-plane endpoints for clients

- **`POST /v1/devices/register`:** user-authenticated enrollment of a personal device. It returns a single-use,
  5-minute, user-bound pre-auth key minted by the cell agent, and queues for approval when the org requires it.
  Completion reuses `/v1/enrollments/{id}/complete`.
- **`GET /v1/client/overview`:** one call for the menu: my devices, org devices, shared with me (with expiry), shared
  by me, services, sandboxes, pending requests. It returns an `ETag` and answers `304` when nothing changed.
- **Event stream:** `GET /v1/events/stream`, plus the org-scoped `GET /v1/orgs/{org}/events:stream`.
  - Postgres `LISTEN/NOTIFY` fans out to the SSE connections. An `events` table (separate from audit, 24 h
    retention) backs `Last-Event-ID` resume.
  - Every event is filtered by the subscriber's org visibility.
  - A heartbeat every 15 s; per-user connection caps; slow consumers get `event: resync` and are dropped.
  - nginx has `proxy_buffering off` and long timeouts for this route.
  - **A blue-green switch drops every stream**, so clients must reconnect with `Last-Event-ID`. An e2e test covers
    exactly that.
- **Access requests:**
  - `POST /v1/access-requests`; `POST …/{id}/approve|deny` (the grantor org's admins);
  - approval creates the grant in the same transaction;
  - `access_request.*` webhooks and SSE events; pending requests expire.
- **`POST /v1/diagnostics`:** an opt-in upload, redacted on the client, capped at 10 MiB, encrypted at rest in a
  private bucket, deleted after 30 days.

### P2.7 CLI for people

- `headmesh login` (device flow; tokens in the keyring), `logout`, `whoami`, `up`, `down`, `status`, and `switch`
  (org and profile).
- Until `headmeshd` exists (C0), `up` drives an isolated `headmesh-tailscaled@<instance>` with
  `/v1/devices/register`. Afterwards it talks to `headmeshd`'s LocalAPI.

### P2.8 Console MVP (`web/app`; plan §12.2; client roadmap §8, C1)

- **Stack:** Vite 8, React 19, TanStack Router and Query, shadcn/ui, Tailwind 4, react-hook-form and zod, and an
  `openapi-fetch` client generated from the committed OpenAPI. It's embedded in `headmesh-server` with `go:embed`
  and served on the same origin as `/v1` (D17).
- **Pages:**
  - sign-up and sign-in (every method), email verification;
  - create org, accept invite, the device-flow verification page;
  - the **onboarding checklist** ("Add your first device": a one-liner, and the device appears live over SSE);
  - devices, invite links, grants (read-only);
  - "Download the app" with OS detection (a placeholder until C2).
- **Hardening:**
  - a strict CSP with no inline scripts; `frame-ancestors 'none'`; Trusted Types where React allows;
  - dependencies pinned through `pnpm-lock.yaml`; Biome.
- **Tests (`ci-web.yml`):**
  - Vitest; axe;
  - Playwright against an ephemeral server, Postgres and a containerised Tailscale client, measuring **sign-up → org
    → first device in under 5 minutes** (the M3 exit).

### P2.9 Gate B controls

- **Quotas** per org and per user (orgs per user, devices, grants, enrollment tokens per hour), enforced on the server
  and visible in the API.
- **Abuse:** Turnstile on sign-up; email verification; a report-abuse address.
- **Legal:** a privacy policy and terms for the hosted service (what's processed, retention, sub-processors,
  deletion); an update to `security.txt`.
- **Support:** a support channel and the incident runbook (P7.3).

---

## 8. Phase P3: sandbox ingress, OAuth connector and MCP gateway (M3 remainder, about 4–5 weeks)

### P3.1 Headmesh ingress for MCP (plan §7.5)

- **nginx:** wire the existing `deploy/nginx/sandbox-ingress.conf.tmpl` into `deploy.sh`, rendered from the private
  inventory.
  - Add a `*.sbx.headmesh.dev` certificate by DNS-01; `*.headmesh.dev` doesn't cover two labels.
  - Rate limits, a 10 MiB body cap, streaming without buffering, timeouts matched to `exec`, and access logs without
    bodies.
- **Tailnet:** a second tailscaled instance on hm-prod-1 in the xcelsior cell as `tag:hm-ingress`, with a grant
  `tag:hm-ingress → tag:sandbox:8780` and nothing else. This is a production ACL change, so it needs approval.
- **Done when** `https://asus.sbx.headmesh.dev/mcp` answers the RFC 9728 challenge, and nothing else on the tailnet is
  reachable from the ingress node (a negative probe).

### P3.2 OAuth live rehearsal and the Claude.ai connector

- `docs/mcp-oauth.md` says production acceptance needs a live rehearsal of authorize, refresh, MCP use and
  revocation. Automate it as `test/e2e/oauth_mcp_test.go`, with Playwright driving consent against the test
  environment, then run it against prod.
- Connect a Claude.ai custom connector to the ingress URL. Record the result in `docs/acceptance/`.
- Once Gate B controls exist, replace the operator-only `oauth-grant` command with a console flow: an owner grants
  their sandbox to their own identity.

### P3.3 Sandboxes in the API (plan §9.8)

- `POST /v1/sandboxes` registers a sandbox and returns its enrollment and config; `GET /v1/sandboxes/{id}`;
  `POST /v1/sandboxes/{id}/events` for mode switches, acceptance results and forwarded audit.
- sandboxd forwards its audit log (plan §6.8: "Forwarded to Headmesh audit from M3") with a sandbox-scoped
  credential (`client_credentials`, scope `sandbox:events`). It buffers offline and retries with backoff.
  `sandbox.mode_changed` webhooks follow.
- **Licensing:** the sandbox talks to the API over HTTP and never imports AGPL packages (P0.5 enforces it).

### P3.4 MCP gateway (sandbox ADR 0002, amended)

**G0 Spike T-18, before any implementation** (on the ASUS; ≤ 3 days; results in
`sandbox/docs/adr/spike-t18-*.md`). ADR 0002's four checks, plus five more that its design depends on:

| Check | Question |
|---|---|
| T-18a | A sidecar on `network_mode: service:sandbox` with `cap_drop: [ALL]`: does a `meta skuid 951` rule loaded in `sandbox` count and drop its packets? |
| T-18b | Mesh-only uid 951 reaches `<tailnet target>:5432`, while `1.1.1.1:443`, `127.0.0.1:8780`, `127.0.0.1:8082` and UDP/TCP `:53` all fail immediately |
| T-18c | In YOLO, uid 951 in `sandbox-mcp-yolo` gets `ENOENT` on `/host`, `/home/aaryn` and `/run/hm-secrets/github_ssh_key` |
| T-18d | Changing one character of a locked mock server's `tools/list` makes sandboxd withhold its tools and report `degraded` with `tools_sha256 mismatch` |
| **T-18e (new)** | **netns survival.** Docker restarts `sandbox` after a crash. Plan §6.1 rejected a Tailscale sidecar for exactly this reason. Does the MCP container keep a dead network namespace? Does a watchdog that exits when `eth0` or `tailscale0` is missing make Docker's restart rejoin the new namespace? |
| **T-18f (new)** | Can each MCP container bind-mount its own `/etc/resolv.conf` and `/etc/hosts` while sharing the namespace? `wan-only` needs `127.0.0.11`, while `sandbox` uses `100.100.100.100`; `mesh-only` needs pre-resolved hosts |
| **T-18g (new)** | Does `inet hm_mcp_egress` coexist with tailscaled's `--netfilter-mode=on` chains and Docker's rules, for IPv4 and IPv6? Drop is final across tables, but accept isn't: confirm nothing else drops the allowed flows |
| **T-18h (new)** | Memory and latency of the stdio ↔ unix-socket shim (G9) with three Node and Python MCPs running |
| **T-18i (new)** | Does `strace -f -e trace=file` inside the `--network=none` dry-run container give a usable path-probe report? |

**Fallback if T-18a or T-18e fails.** The plan's fallback (a dedicated bridge IP per container, with host rules)
loses the tailnet for `mesh-only`. Use **forwarders** instead:

- MCP containers get `network_mode: none`;
- the shim exposes each declared `reach` target as a local unix-socket forwarder dialled by sandboxd from inside the
  tailnet namespace;
- `wan-only` goes through an SNI-checking CONNECT proxy with a host allowlist.

This is also the stronger long-term design (see G15).

**ADR 0002 amendments** (as an addendum, accepted with D13–D15 and D24):

- one container per MCP;
- the pure-Go rules engine;
- the `sandbox:mcp` scope;
- stdio first;
- the new spike checks;
- `osv-scanner` imported as a Go module into the sandbox module directly. The ADR mentions `go.work`, which doesn't
  exist (P0.1). Both modules are Apache-2.0.

**G1 Configuration.**

- `[mcps.<name>]` gains `enabled`, `package`, `profile`, `reach`, `secret_env`, `mounts`, optional
  `tools_allow` and `tools_deny`, and `limits` (cpu, memory, pids) in `sandbox.schema.json` and
  `sandbox.example.toml`.
- Names match `^[a-z][a-z0-9_]{0,15}$`, so that `<name>__<tool>` fits the 64-character tool-name limit some clients
  enforce (see commit e659936). Longer combinations are rejected at `add`.
- Built-in tool names (`exec`, `fs_*`, `session_*`, `process_*`, `git_*`, `artifact_*`, `net_check`, `agents_*`,
  `sandbox_status`) are reserved.

**G2 Host CLI.**

- `headmesh-sandbox mcp search|add|remove|list|verify|update|apply`.
- The registry client for `registry.modelcontextprotocol.io` handles pagination and validates `server.json`
  against its schema. Git and local manifest URLs are supported for private tools.

**G3 Resolution and pinning.**

- **npm:** exact version; the lockfile resolved in a throwaway container with `--ignore-scripts` and recorded
  `integrity` (sha512). Install with `npm ci --ignore-scripts`; lifecycle scripts need an explicit
  `--allow-scripts`.
- **uv:** `uv lock` or `pip compile --generate-hashes`, installed with `--require-hashes`.
- **OCI:** tag → digest; verify the cosign signature when one exists; record the platform digest.
- The caches are filled at `add` time and mounted read-only at runtime.
- **`${root}/state/mcp-lock.json`:** versioned JSON Schema; atomic writes. It holds the package digest, a hash of
  the dependency tree, `tools_sha256`, the scanners' versions and verdicts, `verified_at`, the UID and the profile.

**G4 Supply-chain gate.**

- osv-scanner (as a library) runs over the resolved lockfiles.
- **`MAL-*` advisories always fail.** CVEs at high or critical fail unless `--allow-vuln <id> --until <date>` says
  otherwise.
- **Minimum age:** 48 hours from the registry's own publish time, unless `--allow-unverified`.
- The advisory database is cached, with a freshness check.

**G5 Offline dry-run.**

- An ephemeral container with `--network=none`, a read-only rootfs, `cap_drop: [ALL]` (plus `SYS_PTRACE` only if
  T-18i is adopted), pid and memory limits, synthetic placeholder secrets, and a 60 s timeout.
- It runs `initialize`, `tools/list`, and `prompts/list` and `resources/list` when advertised. A crash fails the
  `add`.
- The path-probe report (`~/.ssh`, `/proc/*/environ`, `/run/secrets`) is a warning, not a boundary.

**G6 Tool-poisoning scanner** (pure Go, D14).

- **Inputs:** names, titles, descriptions, every string in the schemas (descriptions, enums, defaults, examples), and
  prompt and resource text.
- **Rules:**
  - imperatives aimed at the model ("ignore previous", "do not tell the user", `<IMPORTANT>` blocks);
  - invisible Unicode (tag characters U+E0000–E007F, zero-width characters, bidi overrides);
  - mentions of other tools or reserved names (shadowing);
  - requests for credentials or for files such as `~/.ssh` or `.env`;
  - URLs and base64 or hex blobs in descriptions.
- Severities run from info to block, and `add` refuses at block.
- An optional semantic pass against a configured mesh model (for example Ollama on the tower) is advisory only. It
  can add a finding, never clear one.
- Rule patterns are vendored from `mcp-scan` and `mcp-scanner` only after confirming their licences, with notices.
- A corpus of known-bad and known-good fixtures drives the tests.

**G7 `tools_sha256`.**

- RFC 8785 (JCS) canonical JSON over the tools array sorted by name. Each tool keeps `name`, `title`,
  `description`, `inputSchema`, `outputSchema` and `annotations`.
- The specification and golden vectors go in `sandbox/docs/`.
- A `notifications/tools/list_changed` triggers re-verification; a mismatch marks the MCP `degraded`.
- Servers whose tool list depends on their environment are flagged at `add` (two dry-runs with different
  placeholders) and refused unless `--allow-dynamic-tools`, in which case only names and schemas are pinned.

**G8 Render.**

- `render` emits one `sandbox-mcp-<name>` service per MCP:
  - `network_mode: service:sandbox` (or `service:sandbox-yolo`); `user: <uid>:<uid>`; `read_only: true`;
    `cap_drop: [ALL]`; `no-new-privileges:true`;
  - `pids_limit`, `mem_limit`; `/tmp` as tmpfs with `nosuid,nodev,noexec`;
  - the caches read-only; the socket directory `/run/hm-mcp/<name>`; declared mounts only;
  - secrets as `/run/hm-secrets/mcp-<name>` (owned by the MCP's uid, 0400, written copy → chmod → chown per
    `container-contract.md`);
  - its own resolver files (T-18f); a restart policy and a health check.
- YOLO uses the same definition. MCP containers **never** get host mounts.
- **UIDs** are deterministic and persisted in the lock: `offline` 940–949, `mesh-only` 950–969, `wan-only`
  970–989. Running out of a range is a clear error at `add`, not a silent reuse.
- **New image** `ghcr.io/headmesh-dev/sandbox-mcp` (Node, uv and Python, plus the shim), built, scanned with trivy
  and cosign-signed by `sandbox-image.yml`.

**G9 `headmesh-mcp-shim`** (pure Go, sandbox module).

- It runs as PID 1 in the MCP container, starts the stdio server as a reaped child, and bridges it to
  `/run/hm-mcp/<name>/mcp.sock`.
  - The directory is 0750, owned by the MCP's uid with the `agent` group, so sandboxd can connect and other MCPs
    can't.
  - One client at a time; the child restarts with backoff.
- It's also the **netns watchdog** (T-18e).
- **HTTP-transport servers** (D24) are allowed only if they bind a unix socket, or with a rule that restricts their
  loopback port to the shim's uid. Otherwise any process in the shared namespace (including `agent`) could call the
  MCP directly and skip redaction and audit while using its secrets.

**G10 `mcp-firewall`.**

- `render` writes `/etc/headmesh/mcp-firewall.nft`, reviewed with `render --diff`. A root s6 oneshot in `sandbox`
  loads it atomically (`flush table` and the new rules in one `nft -f` transaction).
- **Fail closed:** sandbox health includes "table loaded with the expected hash", and MCP containers depend on that.
  sandboxd re-checks the table hash every 30 s; drift marks every MCP `degraded` and withholds its tools.
- **Rules:**
  - an output chain at priority `filter - 10`; IPv4 and IPv6;
  - per-uid allow sets, then default drop for 940–989, with counters and rate-limited logging;
  - `offline`: every `AF_INET` and `AF_INET6` socket dropped;
  - `mesh-only`: declared `ip:port` pairs only, with names pre-resolved into the hosts file at apply time; port 53
    dropped; loopback dropped;
  - `wan-only`: TCP 443 (or declared ports) to public addresses, plus `127.0.0.11:53`. Dropped: `100.64.0.0/10`
    (which includes `100.100.100.100`), `fd7a:115c:a1e0::/48`, RFC 1918, link-local, `fc00::/7` and loopback;
  - non-TCP traffic dropped unless declared.
- **`mcp apply`:** render → load the table → check its hash → start or restart only the affected MCP containers →
  sandboxd reloads its MCP registry → verify every `tools_sha256`.

**G11 sandboxd gateway.**

- One go-sdk client per MCP over its unix socket.
- Lock verification at connect and reconnect.
- `<name>__<tool>` namespacing; `tools_allow` and `tools_deny`; per-MCP concurrency and timeouts; cancellation passed
  through; response caps (8 MiB on the tunnel route).
- **Mandatory redaction** of every third-party response, whatever `redact_output` says.
- **Audit fields:** `mcp_name`, `mcp_uid`, `profile`, `package_digest`, `duration_ms`, `result`.
- Requires `sandbox:mcp` (D15); the consent page lists it.
- `tools/list_changed` is forwarded to clients.

**G12 Status and diagnostics.**

- `hm status`, `sandbox_status` and `doctor` show for each MCP: state (starting, ready, degraded, failed) with the
  reason, the last verification, the nft table hash and the uid map.

**G13 Tests.**

- **Unit:** JCS vectors; the scanner corpus; golden files for the nft generator; properties of the uid allocator
  (stable, unique, never outside its range); fuzzing the `tools/list` parser.
- **Integration on real Docker:** T-18a–e as permanent tests in `sandbox/tests/e2e/mcp_*`.
- **Acceptance:** a pinned `postgres` (mesh-only) and `github` (wan-only) reachable through `:8780/mcp` over the
  tunnel and over the ingress, plus a rug-pull drill.

**G14 Documentation.**

- `sandbox/README.md`, `container-contract.md`, the `headmesh-sandbox` plugin skill (how attached tools appear and
  what `degraded` means) and the threat model (P7.3).

**G15 Later.**

- The console's "Sandbox MCPs" page with a 1-click catalogue (C6 in the client roadmap §8).
- `wan-only` with a host allowlist through an SNI-checking CONNECT proxy, so a `github` MCP can reach only
  `api.github.com`. As specified, `wan-only` allows any public 443 and is therefore an exfiltration channel for
  whatever the MCP holds. Make the allowlist the default once it's built.

**Done when** M3's exit holds: pinned `mesh-only` and `wan-only` community MCPs are multiplexed on `:8780/mcp`, and
T-18 is green.

---

## 9. Phase P4: adopt the xcelsior cell and integrate Xcelsior (M4, about 3–4 weeks plus windows)

**Prerequisites:**

- P0.3 step 4 (hardened policy) and P0.2 (Headscale 0.29.7 on both cells);
- P1.6 and P1.7 (observability, backups);
- P1.2 (webhooks), P1.3 (device delete, `external_ref` grants already exist), P2.2 (platform child orgs),
  P2.3 (`client_credentials`), the Python SDK;
- **P2.5 compiler user principals**: Aaryn's personal devices on that cell are user-owned.

### P4.1 Reach the API from prod-hetzner

The cell endpoint (nginx port 7443) is reachable from prod-hetzner only over the tailnet. Add the policy grant for
it (production ACL change, so it needs approval). Prod-hetzner gets no new inbound port.

### P4.2 Cell agent in observe mode (plan §13.8, step 1)

- Install `headmesh-cellagent` with the hardened unit: outbound only, Headscale's unix socket. Headscale stays in
  `policy.mode: file`.
- Announce the step and get the go-ahead. Run observe mode for at least a week, and watch drift and op metrics.

### P4.3 Import mapping

- Orgs: `aaryn-personal` (user-owned nodes, including the sandbox host as a personal device), `xcelsior-infra`, one
  `xc-owner-*` org per provider host, and `sandbox`.
- The file names hosts, so it lives sops-encrypted or in the internal notes (D23), not in the public tree.
- `headmesh cell import --dry-run` validates it against the observed nodes.

### P4.4 `headmesh cell diff xcelsior`

- Compile the candidate policy and show the reachability delta **flow by flow** against the hardened policy, using
  the evaluator. It must preserve every surveyed flow: SSH relay and web terminal, port exposure, SPIRE 8095, NFS,
  Ganesha, Lightning, Ollama and operator SSH.
- Cross-check with `headscale policy check`. Review the report in the internal notes.

### P4.5 Cell invariants

- `/etc/headmesh/cell-invariants.hujson` for xcelsior: critical flows (for example `tag:xc-control → tag:xc-host:22`)
  and node IDs that must never be deleted. Kept internal.

### P4.6 Maintenance window (plan §13.8, step 4)

1. Update the failover and standby tooling for database mode first.
2. Switch Headscale to `policy.mode: database`.
3. Move the agent to `enforce`.
4. Run the Xcelsior live gates and `tests/test_anchor_workloads.py`.

**Undo:** `headmesh-cellagent rollback`, then back to file mode.

IPs and node IDs don't change, and no device re-enrolls.

### P4.7 Xcelsior repo integration (plan §11)

- **`control_plane/headmesh/`:**
  - the pinned generated client, with a CI staleness check;
  - `service.py`: httpx timeouts, tenacity retries with idempotency keys, a circuit breaker;
  - `onboarding.py`, `grants.py`, `isolation.py`, `webhooks.py`, `reconcile.py`, `settings.py`.
- **`api/routes/headmesh_webhooks.py`:** Standard Webhooks verification with a timestamp window, de-duplication by
  `webhook-id`, registered in `ALL_ROUTERS`, and passing the guard and coverage tests.
- **Configuration:** the `XCELSIOR_HEADMESH_*` variables in `.env.example` **and** the compose `x-api-environment`
  anchor.
- **Migration** at the next free number: `headmesh_links`, unique `(kind, local_id)`, added to `TABLE_DOMAINS`.
- **"Connect your GPU":**
  - a child org `xc-owner-<customer_id>` (template `locked`) and a single-use `gpu-host` token;
  - `install.sh join_mesh` calls `/v1/enroll` and keeps the `tailscaled-xcelsior` unit;
  - fix the wizard's `setupNetworking`;
  - `device.enrolled` marks the host network-ready;
  - **host delete deletes the Headmesh device**;
  - add any module shipped to hosts to `sign-static-agent.py`.
- **Billing hooks:** the `headmesh_grants` projection sink.
  - Running or `instance_started` creates a grant with `external_ref = attempt_id`.
  - Stopped, terminated, cancelled, preempted, `lease_expired` and the wallet-empty stop revoke it.
  - A reconcile job every 5 minutes compares the two; Xcelsior is the source of truth.
- **Renter isolation templates** as plan §11 describes.
- **The six tests** named in plan §11.
- **Rollout:** a per-host flag, observe → enforce, the live gates at every step. Retire raw Headscale key minting
  (`issue_mesh_enrollment`) once every host is on Headmesh.

### P4.8 Verify and announce

- Plan §18 "M4" in full: `cell diff` shows no unexpected removals; a test rental gets a grant and it's revoked within
  5 s of termination; probes from provider to provider and from provider to personal devices fail.
- Then publish "Xcelsior Compute runs on Headmesh" on headmesh.dev (plan §12.1).
- **Later and separate:** move the xcelsior cell's standby from the ASUS to the Litestream and recovery pattern.

---

## 10. Phase P5: clients I, engine and macOS (C0, C2, C3; about 12–15 weeks)

C0 can start as soon as D1 and D9 are decided, in parallel with P1, because coding agents do most of it. C1 is P2
(§7).

### C0 Foundations

- **ADRs:**
  - 0016: own clients on `tailscale.com`, no fork, no phoning home;
  - 0018: two planes, LocalAPI extensions, the `clients/` module;
  - 0019: macOS Network Extension, Developer ID, Sparkle;
  - 0021 (exit nodes) before C3; 0022–0025 before their phases.
- **`clients/`** is its own Go module (Apache-2.0), laid out as in client roadmap Appendix A. It pins `tailscale.com`
  to the latest v1.104.x.
- **Engine wrapper (`clients/engine`):**
  - the control URL always comes from discovery and is never the upstream default;
  - log upload compiled out (check the exact feature-tag names at the pinned version) or pointed at a Headmesh
    endpoint;
  - `clientupdate` disabled; DERP comes from the cell's map.
- **`NOTICE`** with the BSD-3 notices, and a generated "Open-source licences" list for the apps.
- **`cmd/headmeshd`:** a Linux systemd service plus `headmeshd@<name>` instances, the LocalAPI on a unix socket, and
  `/localapi/v0/headmesh/*` (overview, orgs, grants, access requests, services, sandboxes, notifications,
  diagnostics). Every GUI is written against a `LocalClient` interface.
- **The no-phoning-home test:**
  - the client runs in a network namespace with an nftables allowlist (the cell, the API, Headmesh DERP, STUN) that
    logs everything else;
  - a DNS log check catches any lookup of a Tailscale-owned name;
  - any hit fails the build.
- **Compatibility:**
  - a client × Headscale matrix covering every version the cells run;
  - `test/e2e` parameterised to use `headmeshd` in place of `tailscaled`;
  - a weekly job bumps `tailscale.com` and opens a pull request when the matrix is green.
- **Brand:** menu bar template icons, app icons, tray icons.
- **Exit:** `headmeshd` and the CLI join hs1 on Linux with zero connections to Tailscale-owned hosts, and `test/e2e`
  passes with `headmeshd`.

### C2 macOS alpha

- **Week 1 spikes:**
  - **C2-S1**, the Network Extension bridge: the extension starts, joins hs1, pings a peer, resolves MagicDNS, and
    survives sleep and wake. Fallback: the `headmeshd` launchd daemon registered with `SMAppService.daemon`.
  - **C2-S2**, coexistence with an installed Tailscale: both claim `100.64.0.0/10` and `100.100.100.100`. Can a cell
    use a narrow sub-range? The xcelsior cell's hardcoded `100.64.0.x` makes this cell-specific. Until it's solved,
    detect the conflict and explain it in the menu.
- **`HeadmeshKit.xcframework`:** `go build -buildmode=c-archive` for arm64 and x86_64, merged with `lipo`. A small C
  API: `hm_start(config_json, tun_fd)`, `hm_stop()`, `hm_localapi(req)`, and callbacks for state and tunnel
  settings. Use `wireguard-apple` (MIT) as the reference.
- **Tunnel settings** from the netmap, re-applied on every change: addresses, included routes, DNS `100.100.100.100`
  with match domains.
- **App:**
  - `MenuBarExtra(.menu)` with the states, icons and menu from client roadmap §5.3;
  - notifications, each switchable, with nothing for routine online and offline changes;
  - onboarding: extension approval → VPN approval → sign-in → org choice;
  - settings tabs: Accounts, General, Network, Advanced;
  - `headmesh://join` deep links;
  - tokens in a shared Keychain access group; launch at login through `SMAppService.mainApp`; no privileged helper.
- **Robustness (exit criteria):** sleep, wake and lid close; Wi-Fi ↔ Ethernet ↔ hotspot; captive portals; connect on
  boot; a clean uninstall with no DNS or route residue.
- **Distribution:** Developer ID, hardened runtime, `notarytool`, stapling, a DMG, and Sparkle 2 with EdDSA-signed
  appcasts at `pkgs.headmesh.dev/macos/{stable,beta}` that refuse downgrades. Crash reporting is opt-in.
- **Tests:** XCTest for the view models; UI tests on a mocked `LocalClient`; Go tests for the bind layer; a nightly
  soak on a Mac mini runner restricted to the owner.
- **Exit:** a fresh Mac goes from download to connected in under 2 minutes; a 7-day soak with no manual reconnects;
  zero connections to Tailscale-owned hosts; crash-free sessions above 99%. Dogfood with Xcelsior renters reaching
  rented GPUs through grants.

### C3 macOS beta and console v1

- **C3.1 Exit nodes** (D3 and ADR 0021 first, plus Aaryn's written sign-off; update `AGENTS.md` in the same change):
  - the compiler emits `autogroup:internet` **only** together with `via` naming approved exit nodes of the same org;
  - new invariants I8 (no node routes through another org's exit node) and I9 (exit-node visibility stays inside the
    org), as property tests;
  - e2e proves no cross-tenant visibility or routing;
  - `allow_exit_nodes` is off by default, and approving an exit node is an audited admin action.
- **C3.2 Subnet routes:**
  - spike C3-S1: does Headscale choose primary routes cell-wide when two tenants advertise overlapping RFC 1918
    prefixes?
  - accept-routes in the client; approval only within the org's own prefixes; off by default.
- **C3.3 Features:**
  - MagicDNS polish; Taildrop between your own devices; the SSH action; the services menu;
  - access requests with approve and deny in the menu;
  - several orgs as engine profiles; device approval from the menu.
- **C3.4 Management:**
  - the managed preferences domain `dev.headmesh.macos` (`ServerURL`, `OrgHint`, `RequireSignIn`,
    `AllowedExitNodes`, `UpdateChannel`, `HideCustomServer`, `StartOnLogin`);
  - sample profiles for the system-extension policy and the VPN configuration;
  - a signed pkg; a Homebrew cask.
- **C3.5 `headmesh.dev/download`:** OS and architecture detection on the Astro site. `app.headmesh.dev/join/<token>`
  tries `headmesh://` first and falls back to the download.
- **C3.6 Console v1:**
  - Machines (rename, remove, approve, key expiry, roles), Users, Sharing (grants both ways, requests);
  - Access: a rules editor (Monaco with schema), preview, and the access-check explanation;
  - the audit log viewer;
  - Settings: API keys, webhooks, exit nodes, SSH, file sharing.
  - No feature exists only in the console.
- **Exit:** 20–50 external beta users; a privacy-preserving funnel from sign-up to connected (server-side counts
  only); a support runbook.

---

## 11. Phase P6: clients II, Windows, Linux and mobile (C4, C5)

### C4 Windows and Linux (can start after C1; needs a second pair of hands to overlap with C3)

- **Windows:**
  - `headmeshd` as a Windows service on Wintun, with a named-pipe LocalAPI whose ACL admits only the interactive
    user and SYSTEM;
  - the Go tray app (shared with Linux);
  - a WiX MSI signed through Azure Artifact Signing with GitHub OIDC (no exported key);
  - a signed update manifest with percentage gates, downgrade refusal and a silent MSI install; winget;
  - ADMX and ADML templates using the same keys as the macOS MDM domain;
  - `install.ps1`.
- **Linux:**
  - `headmeshd` deb and rpm (nfpm); signed apt and yum repositories at `pkgs.headmesh.dev` (an offline GPG primary
    key and a CI subkey); AUR;
  - the tray app (StatusNotifierItem);
  - `install.sh` installs `headmeshd`.
  - **Migration from `headmesh-tailscaled@<name>`** reuses each instance's state file, so nodes keep their identity
    and IPs. Test that explicitly.
- **Exit:** the same funnel and soak criteria as C2 on Windows 11, Ubuntu 26.04 and one rpm distribution.

### C5 Mobile

- **iOS:**
  - shared SwiftUI with an NE app extension; the engine slimmed with `ts_omit_*` tags to fit the roughly 50 MB
    extension limit, with memory **measured on devices in CI** before every release;
  - TestFlight, then App Review (VPN rules, privacy policy);
  - a share-sheet extension, "Send to a Headmesh device".
- **Android:**
  - fork `tailscale-android` into `headmesh-dev/headmesh-android`;
  - rebrand completely, replace the control-URL and logging defaults, add Headmesh sign-in and the overview screens;
  - rebase on upstream monthly; Play internal testing, then production, declaring `VpnService` as the core function.
- **Exit:** both store listings live; mobile smoke tests in CI (join, reach a peer, receive a share).

---

## 12. Phase P7: hardening, launch and GA differentiation (M5, C6)

- **P7.1 Load and game day** at 2× launch scale (repeat P1.11 at the new limits); a recovery game day under
  15 minutes.
- **P7.2 Security assurance:**
  - OWASP ASVS L2 on the API and OAuth server; the OAuth conformance suite;
  - 24-hour fuzzing campaigns; a dependency licence review;
  - an **external penetration test** of the API, OAuth and the clients ($5–15k), with criticals and highs fixed and
    retested.
- **P7.3 Documents and process:**
  - `docs/threat-model.md`;
  - `docs/runbooks/`: incident response, revoke-all, compromised API key, compromised cell host, key rotation (op
    signing, cell CA, OAuth seeds, webhook secrets), cell recovery, break-glass;
  - the CLA or DCO (D19); `SECURITY.md` coverage for the clients;
  - a privacy policy with App Store privacy labels; a launch checklist.
- **P7.4 CI deploy pipeline** (plan §13.2): a tag builds, signs and attests. The deploy job joins the tailnet with an
  **ephemeral `tag:ci` key minted by Headmesh**, which is dogfooding, and runs `deploy.sh` with health-gated
  rollback.
- **P7.5 Release engineering (ADR 0024):**
  - `pkgs.headmesh.dev` on R2;
  - nightly, beta and stable channels with phased rollouts;
  - `min_client_version` enforcement;
  - the signing matrix (client roadmap §9.3) in a protected `release` environment.
- **P7.6 Cost review:** when the Hetzner credits run out, drop back to `cx23` unless memory stays above 70% or API p95
  regresses in load tests (plan §13.1).
- **P7.7 C6, in value order:**
  1. **Own DERP regions** (EU and North America at least, APAC ideally; `derper --verify-clients`; ADR 0025). Before
     GA, and the `hel1` relay before Gate B (D21).
  2. **GitHub Action `headmesh-dev/connect`:** an ephemeral tagged CI node per job. It exchanges the GitHub OIDC
     token for a short-lived enrollment, so no long-lived secret is needed.
  3. **HTTPS certificates:** ACME DNS-01 on `mesh.headmesh.dev`; the device keeps its private key;
     `/v1/devices/{id}/cert-challenge`; a warning that names appear in Certificate Transparency logs.
  4. **Public links:** generalise the ingress into `headmesh expose`.
  5. **Device posture:** `/v1/devices/{id}/posture`; the compiler moves non-compliant devices into quarantine. Never
     the last line of defence.
  6. **Peer relays:** `cap/relay` for org-owned relay nodes only, with an invariant.
  7. **Enterprise identity:** OIDC, then SAML, then SCIM.
  8. **Terraform provider.**
  9. **Per-org DNS:** split DNS through the product plane, applied by Headmesh's own client.
  10. **Mac App Store build.**
  11. **Flow logs.**
  12. **Tailnet Lock** research.
  13. **Several networks at once** on desktop.

  Also: Taildrive (`cap/drive` per org); file sharing across orgs (spike: does Headscale pass app capabilities
  through?); Serve identity headers; console pages for DNS, SSO, posture, Sandbox MCPs, billing, and cell and usage
  views.

  **Post-GA:** Kubernetes operator; app connectors. **Not planned:** commercial VPN exit nodes.

---

## 13. Cross-cutting

### 13.1 CI workflows to add

| Workflow | Phase | Content |
|---|---|---|
| `ci-go` (extend) | P0.5, P1.5 | Licence boundary, fuzzing, `buf breaking`, benchmark gate, River migrations up/down/up |
| `ci-contract` | P1.3 | schemathesis, SDK staleness |
| `ci-web` | P2.8 | Biome, Vitest, Playwright with axe, the sign-up-to-device timing test |
| `e2e` | P1.11 | Nightly on Hetzner; the version matrix |
| `fuzz-nightly` | P0.5 | Long fuzzing and property runs; crashers become issues |
| `restore-drill`, `recover`, `janitor` | P1.7 | As plan §13.4–§13.5 |
| `tofu` | P1.8 | Plan on pull requests, manual apply, weekly drift |
| `conformance` | P2.3 | OpenID OP profile, nightly |
| `clients` | C0+ | No-phoning-home, client × Headscale matrix, weekly engine bump |
| `macos`, `windows` | C2, C4 | Build, sign, notarise, soak |
| `deploy` | P7.4 | Tailnet-joined deploy with rollback |

### 13.2 ADR schedule

| ADR | Topic | Before |
|---|---|---|
| sandbox 0002 addendum | Gateway amendments (D13–D15, D24) | G1 |
| 0016–0020 | Clients, person-owned devices, architecture, macOS, native OAuth | C0, P2.5, P2.3 |
| 0026 (new) | API path shape (D16) | P1.3 |
| 0027 (new) | Measured hs1 capacity limits | P1.11 exit |
| 0028 (new) | Audit anchoring (T-17 outcome) | P1.9 |
| 0021 | Exit nodes | C3.1 |
| 0022–0025 | Tray technology, Android fork, release engineering, DERP | C4, C5, P7.5, P7.7 |

### 13.3 Documentation upkeep

Every phase exit updates the status lines in `plan.md` (as §9.7 and §9.8 already do), `README.md`, the relevant
component README and the docs on headmesh.dev. `docs/acceptance/` keeps a dated report for every gate.

---

## 14. Schedule and dependencies

Week 0 starts Monday 2026-11-23. Agents run tasks in parallel where files don't overlap (ADR 0012), and the
holidays at the end of December cost about a week.

```
Week            0    2    4    6    8   10   12   14   16   18   20   22   24   26   28 … 40
P0 Stabilise    ███
P1 Foundations   ██████████            (P1.6 and P1.7 in weeks 1–3)
P2 Identity           ██████████
P3 Gateway                  ████████   (T-18 spike can run in week 3)
P4 xcelsior                     ███████ (maintenance windows as approved)
C0 Engine         █████                (parallel; needs D1 and D9)
C2 macOS α                       ███████████   (needs the Apple account and P2.5)
C3 macOS β                                  ███████████
C4 Win/Linux                         ████████████████████  (needs a second engineer to overlap)
C5 Mobile                                         ██████████████████████ …
Gates           A at ~week 3 · B at ~week 11–12 · C (macOS, Windows, Linux) at ~week 26–30
```

**Hard dependencies** (A → B means B can't finish before A):

- P0.4 Apple enrollment → C2
- P0.3 step 4 → P0.3 step 5 → M1 exit
- P1.6 and P1.7 → Gate A
- P1.7 drill and game day, P1.6 prober, P2.2, P2.8, P2.9 → Gate B
- P2.5 → C1 exit, P4.6, C2
- P1.2, P1.3, P2.2, P2.3 → P4.7
- T-18 (G0) → G1–G14
- ADR 0021 and D3 → C3.1
- §3.2 complete → Gate C

---

## 15. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| A multi-tenant bug leaks access across orgs (person-owned devices, exit nodes, subnet routes, MCP firewall) | Medium | **Critical** | Five isolation layers (plan §14); property tests and e2e mandatory; P0 prober; owner sign-off on rule changes |
| Single VM lost before P1.7 lands | Low | Critical | P1.7 is first in P1; Gate A waits for replication |
| Blue-green with a non-backward-compatible migration | Medium | High | §3.1 expand and contract; an old-colour-against-new-schema test in `deploy/tests/e2e.sh` |
| `tailscale.com` internals change between releases | High | Medium | Thin wrapper; pinned tags; weekly bump with the full matrix |
| Headscale's capability window drops a client version | Medium | High | Matrix; upgrade cells before clients; `min_client_version` |
| Apple entitlement, notarisation or review delays | Medium | High | Start in P0.4; daemon fallback; App Store optional on macOS |
| Network Extension complexity | High | High | Time-boxed spike; `wireguard-apple`; a specialist contractor; soak tests |
| Conflict with an installed Tailscale | High for this audience | Medium | C2-S2; clear in-app explanation |
| `service:sandbox` MCP containers keep a dead namespace after a sandbox crash | Medium | High | T-18e watchdog; forwarder fallback |
| A third-party MCP exfiltrates over `wan-only` 443 | Medium | Medium | Redaction and audit; G15 host allowlist; least-privilege secrets |
| Shared checkout with several agents (uncommitted work collides) | Medium | Medium | ADR 0012 rules; P0.1 lands or drops the work in progress first |
| Hetzner credits expire before load tests run | Medium | Low | P0.2 checks the date; P1.11 is scheduled before it |
| Directory, plugin or store policy changes | Medium | Medium | Submissions stay drafts until reviewed in real clients; no claims of acceptance |
| One maintainer (bus factor) | High | High | Runbooks, ADRs, break-glass documentation, recovery automated end to end |
| Scope creep toward full parity | High | High | Client roadmap §7.1; C6 driven by usage data |

---

## 16. Coverage matrix (every source item → task)

### 16.1 `docs/plan.md`

| Section | Item | Task |
|---|---|---|
| §0 | Locked decisions | Done (ADRs 0001–0015); D12, D22 for what's still open |
| §1 | Ground truth (hosts, apt suites, Headscale, MCP spec, SDKs, runner, tunnel, plugins, pricing) | P0.2 (versions), P0.5 (Renovate), P7.6 (pricing) |
| §2 | Architecture | Built; P1.6–P1.8 for the missing boxes (Alloy, Litestream, pgBackRest, R2) |
| §3.1 | Layout (`deploy/{tofu,ha,install,secrets}`, `docs/{runbooks,acceptance,threat-model}`, `web/app`) | P1.7, P1.8, P1.10, P0.3, P7.3, P2.8 |
| §3.2 | Pinned libraries not yet adopted (River, go-webauthn, go-keyring, OpenTelemetry, client_golang, standard-webhooks, testscript, k6) | P1.1, P2.1, P1.10, P1.6, P1.2, P1.11 |
| §3.3 | Standards: commits, ADRs, CI security, Go rules, branch protection, package discipline, shared checkout | §3.1, P0.4 (rulesets), P0.5, §13.2 |
| §4 | M0–M5 exit criteria | P0 (M0, M1), P1 (M2), P2 and P3 (M3), P4 (M4), P7 (M5) |
| §5.1 | Headscale standby | Done |
| §5.2 | Xcelsior tailnet hardening | P0.3 step 4 |
| §5.3 | ASUS host prep; BIOS | Done; P0.3 step 10 |
| §5.4 | prod-hetzner disk leftovers | P0.6 |
| §5.5 | Accounts and settings | P0.4 |
| §5.6 | Repo bootstrap; ADR index | Done; P0.1 |
| §5.7 | prod-hetzner 26.04.1 | Done |
| §5.8 | hm-prod-1 under OpenTofu | P1.8 |
| §6.1–§6.14 | Sandbox design | Built; P0.3 (firewall, backups, masks, enrollment, modes, GPU default) |
| §6.15 | Acceptance through five routes plus the recreate bonus; report | P0.3 step 7 |
| §6.16 | MCP gateway | P3.4 (G0–G15) |
| §7.1 | Claude runner (T-1 residue, Team toggle) | P0.3 step 6, P0.4 |
| §7.2 | Remote Control (T-2 residue) | P0.3 step 6 |
| §7.3 | Codex over SSH (T-3 residue) | P0.3 step 6 |
| §7.4 | OpenAI tunnel | P0.3 step 6, P0.4 |
| §7.5 | Headmesh ingress; `headmesh expose`; OAuth connector; gateway over ingress | P3.1, P3.2, P7.7 (4), G13 |
| §7.6 | `codex exec-server` | P0.3 step 6 |
| §7.7 | Unified status | Built; G12 extends it |
| §8.1–§8.2 | Catalogues and plugin contents | Built |
| §8.3 | Xcelsior-side fixes (scopes, canonical audience, Codex docs, `.vscode` path) | P0.2 |
| §8.4 | Validation and release (`claude plugin tag`), user docs | Built; directory submissions and MCP Registry in §16.5 |
| §9.1 | Domain model (`users`, memberships, invites, `oauth_*`, webhooks, `idempotency_keys`) | P2.1, P2.2, P1.2, P1.3 |
| §9.2 | Tenancy on Headscale; adopted user-owned nodes; routes and exits off | Built; P2.5, P4.3, C3.1, C3.2 |
| §9.3 | Compiler invariants, evaluator, timeline, perf budget | Built; P1.5, P0.1 |
| §9.4 | Cell agent guardrails; local `revoke-grant` and `revoke-all-grants`; metrics | Built; P1.4, P1.6 |
| §9.5 | Enrollment: install one-liners, approval, reconciler, quarantine | P1.10, P1.3, P1.4 |
| §9.6 | Grants: extend, events, SLO | P1.3, P1.1, P1.11 |
| §9.7 | Audit daily anchor | P1.9 |
| §9.8 | API remainder (orgs, members, webhooks, sandboxes, SSE, conventions, SDKs) | P1.3, P2.2, P1.2, P3.3, P2.6 |
| §9.9 | Authentication and authorization | P2.1, P2.3, P2.2 |
| §9.10 | Webhooks | P1.2 |
| §9.11 | Observability, SLOs, prober, alerting | P1.6 |
| §9.12 | Scale and cells; limits | P1.11 |
| §10 | CLI commands, `join` on Linux, credentials, distribution, tests | P1.10, P2.7 |
| §11 | Xcelsior integration | P4.7 |
| §12.1 | Marketing: API reference, proof section | P1.3, P4.8 |
| §12.2 | `web/app` | P2.8, C3.6 |
| §13.1 | Cost | P7.6 |
| §13.2 | Stack, OpenTofu, secrets, deploy pipeline | P1.6, P1.7, P1.8, P7.4 |
| §13.3 | Replication and R2 budget; T-9, T-10; manifest | P1.7 |
| §13.4 | Detection, fencing, recovery, ported tests | P1.7 |
| §13.5 | Restore drills, janitor | P1.7 |
| §13.6 | DERP outside the VM; second location | P7.7 (1), D21 |
| §13.7 | Break-glass drill, check, runbook | P1.7 |
| §13.8 | xcelsior cell adoption | P4.1–P4.6 |
| §14 | Security model; pre-launch checklist | §3.1, P7.2, P7.3 |
| §15 | Testing layers and workflows | §13.1 |
| §16 | Blueprint coverage | §16.4 below |
| §17 | Spikes: T-1, T-2, T-3 (live residue) | P0.3 step 6 |
| §17 | T-4, T-5, T-12, T-13, T-14 | Resolved (ADR 0014) |
| §17 | T-7, T-8, T-16 | Resolved (sandbox spike ADRs) |
| §17 | T-9, T-10 | P1.7 |
| §17 | T-11 | P0.4, P1.7 |
| §17 | T-15 | P0.3 step 10 |
| §17 | T-17 | P1.9 |
| §17 | T-18 | G0 |
| §18 | Verification per milestone | Each phase's "done when" |
| §19 | 1: approvals | D12, D22, P0.3, P4 |
| §19 | 2: accounts | P0.4 |
| §19 | 3: CLA or DCO | D19 |
| §19 | 4: rulesets | P0.4 |
| §19 | 5: mailbox | D20 |
| §19 | 6: second DERP | D21 |
| §19 | 7: Hetzner token | P0.4 |

### 16.2 `docs/client-roadmap.md`

| Section | Item | Task |
|---|---|---|
| §0, §1 | Goal, quality bar, "different product" | C0–C5; brand in C0 |
| §2 | Headscale 0.29.7; Tailscale 1.104.7 | P0.2 |
| §3.2 | No fork; no phoning home; CI egress check; versions; licences and `NOTICE`; separate module | C0 |
| §4.1–§4.3 | Layers, two planes, engine host per platform | C0, C2, C4, C5 |
| §4.4 | LocalAPI extensions, per-platform token stores | C0, C2, C4, C5 |
| §4.5 | Sign-in and enrollment; device flow; re-auth spike; custom server warning | P2.3, P2.6, P2.5 (C1-S1), C2 |
| §4.6 | Person-owned devices | P2.5 |
| §4.7 | Discovery and cells | P2.4 |
| §4.8 | Every endpoint: `.well-known`, `me`/`me/orgs`, orgs and members, register, config, overview, events, access requests, posture, cert challenge, diagnostics | P2.4, P2.2, P2.6, P7.7 (3, 5) |
| §5.1–§5.4 | macOS bundle, bridge, UX, robustness, coexistence | C2, C3 |
| §5.5 | Distribution, MDM, App Store later | C2, C3.4, P7.7 (10) |
| §5.6 | Apple prerequisites | P0.4 |
| §6.1 | Windows | C4 |
| §6.2 | Linux | C4, P1.10 |
| §6.3 | iOS | C5 |
| §6.4 | Android | C5 |
| §6.5 | Headless CLI; GitHub Action | P2.7, C0, P7.7 (2) |
| §7 | Feature matrix rows | C2/C3 (apps, MagicDNS, SSH, Taildrop, profiles, approval, requests, MDM, updates, console, webhooks, audit viewer); C3.1 (exit nodes); C3.2 (subnet routes); P2.1 and P7.7 (7) (SSO); P2.5 (key expiry); P7.7 (DERP, peer relays, Taildrive, cross-org files, Serve, public links, HTTPS, posture, flow logs, per-org DNS, several networks, Terraform, Tailnet Lock, Kubernetes, app connectors) |
| §7.2 | Exit-node isolation design | C3.1 |
| §8 | Console phases C1, C3, C6 | P2.8, C3.6, P7.7 |
| §9.1–§9.2 | Download experience; `pkgs.headmesh.dev` | C3.5, P7.5 |
| §9.3 | Signing matrix | C2, C4, C5, P7.5 |
| §9.4 | DERP | P7.7 (1) |
| §9.5 | Release trains and compatibility | P7.5, C0 |
| §10 | Client security and privacy; pen test; privacy policy; crash reporter | C2, P7.2, P7.3 |
| §11 | C0–C6 with exit criteria | C0, P2, C2, C3, C4, C5, P7.7 |
| §12 | Team, accounts, costs (Apple contractor, Mac mini runner) | P0.4, C2, P7.6 |
| §13 | Risks | §15 |
| §14 | Decisions 1–11 | D1–D11 |
| §15 | ADRs 0016–0025 | §13.2 |
| §16 | First two weeks | P0.4, P0.2, C0, P2.5, P2.6 (OpenAPI first), C2-S1 |
| Appendix A | Layout; licence boundary check | C0, P0.5 |
| Appendix B | Upstream LocalAPI used | C0, C2 |
| Appendix C | Sources checked on 2026-11-14 | Re-check at each ADR (0016–0025), since upstream moves |
| §7.1, timeline sketch | Picking the 20%; per-phase timing | §14 (this plan's calendar) |

### 16.3 `sandbox/docs/adr/0002-…`

| Section | Task |
|---|---|
| Problem; rejecting paid reputation feeds | Accepted as is (G0) |
| Layer 1: registry discovery | G2 |
| Layer 2, steps 1–4: digests, OSV and age gate, offline dry-run, poisoning scan and `tools_sha256` | G3, G4, G5, G6, G7 |
| Layer 3: companion container, mount isolation, shared namespace | G8 (one container per MCP, D13), T-18e and T-18f |
| Layer 3: per-UID nftables, three profiles, DNS-tunnelling closure | G10, T-18a, T-18b, T-18g |
| Layer 4: lock verification, namespacing, mandatory redaction, audit | G11 |
| Configuration sketch | G1 |
| Spike T-18a–d | G0, kept as permanent tests in G13 |
| Plan §4 M3 exit; plan §7.5 single connector | G13, P3.1, P3.2 |

### 16.4 The handoff brief (`docs/handoff.md`)

| Item | Task |
|---|---|
| Everything in plan.md §16's matrix | As mapped in 16.1 |
| Pitfalls, security requirements, persistence rules, build order, the two modes, the all-in-one setup | Built into the sandbox (plan §16 maps each); P0.3 proves them live |
| Tenant security model | Built (compiler, cell agent); P2.5, C3.1, G10 extend it under the same rules |
| Marketing site; repository layout | Live; P1.3 (API reference), C3.5 (downloads), P4.8 (proof section) |
| Acceptance through the MCP, runner and `exec-server` routes; the recreate bonus | P0.3 step 7 |
| Open to-do: confirm self-hosted runners on the Team workspace | P0.4, P0.3 step 6 |
| Open to-do: verify `codex exec-server` access | P0.3 step 6 |
| Open to-do: how Claude and Codex show diffs | P0.3 step 7 (recorded in the report) |
| Domain and email forwarding | Done; D20 for sending |
| Consumer slice: CLI and sign-up page before desktop or mobile apps | P1.10 and P2.8 come before C2 |
| Success metric: waitlist sign-ups | Live; added to the Grafana dashboard (P1.6) |
| Success metric: GPU owners onboarded through Headmesh | P4.7 (a metric in Xcelsior) |
| Success metric: the five-step test | P0.3 step 7 |
| Licence and business model | Done (ADR 0015); D19 |

### 16.5 Supporting documents

| Source | Item | Task |
|---|---|---|
| `docs/mcp-oauth.md` | Live rehearsal of authorize, refresh, MCP and revocation | P3.2 |
| `docs/mcp-oauth.md` | Device flow and platform client credentials "remain separate future work" | P2.3 |
| `docs/browser-sign-in.md` | Separate prod and test registrations; MCP Registry validate and publish | P0.4; Aaryn publishes `sandbox/server.json` with `mcp-publisher` after P0.3 |
| `plugins/SUBMISSIONS.md` | OpenAI and Claude directory submissions: publisher verification, reviewer access, walkthroughs, review cases in real clients, commerce review | Aaryn, after P0.3; never claim acceptance before review |
| `sandbox/README.md` | Security findings (firewall first, `extra_denylist`, `.venv-sandbox`); steps 1–7 | P0.3 |
| `deploy/README.md` | hs1 bootstrap key file; Full (strict); cell endpoint over the tailnet for xcelsior | P0.1, done, P4.1 |
| Internal ops notes | Credential rotation; prod-hetzner leftovers; OpenTofu import | P0.1, P0.6, P1.8 |

---

## 17. Consolidated checklist for Aaryn

**Decide:** D1–D11 (client roadmap §14); D12 and D22 (production upgrades and adoption); D13–D16, D18 and D24
(engineering defaults; a quick yes is enough); D19 (CLA or DCO); D20 (mailbox); D21 (DERP timing); D23 (where the
import mapping lives); and, from P0.6, whether to move the prod-hetzner loop volumes to a Hetzner Volume.

**Do by hand:** everything in P0.4; BIOS power-on (P0.3); the sandbox host steps (firewall, backup; sudo); the
Headscale policy window (P0.3 step 4); the Platform, Team and Claude.ai settings; the directory submissions and the
MCP Registry publication; the delete of the hs1 bootstrap key file and the credential rotation (P0.1).

**Approve when announced:**

- the Headscale 0.29.7 upgrades (P0.2);
- prod-hetzner `daemon.json` (P0.6);
- the ingress grant and its second tailscaled (P3.1);
- the cell agent on prod-hetzner (P4.2) and the policy-mode window (P4.6);
- the cell-endpoint grant (P4.1);
- exit nodes (C3.1, written sign-off).
