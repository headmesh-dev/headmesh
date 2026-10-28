# Headmesh

**Share machines, not your network.**

Headmesh is an open, self-hostable, multi-tenant control layer for private WireGuard meshes. It builds on
[Headscale](https://github.com/juanfont/headscale) and the official Tailscale clients — it does not reimplement
networking or cryptography — and adds what Headscale leaves out:

- **True multi-tenancy** — isolated organisations, each with its own members, devices and deny-by-default rules.
- **One-click onboarding** — invite links and one-line install commands that enrol a machine into the right
  network with the right permissions.
- **Temporary access** — grant one machine or service to another tenant for a set time; it revokes itself.
- **Agent sandboxes** — let cloud AI agents work on your real files and private services from a sandbox on your
  own hardware, in a scoped or full-access mode.
- **Yours to run** — no closed control server.

Xcelsior Compute is the first production user (GPU owners and renters connected without exposing anyone's home
network).

> Status: pre-release. Apache-2.0 for the CLI, sandbox, plugins and web; AGPL-3.0 for the server and cell agent.
> See [LICENSING.md](LICENSING.md).

## Use Headmesh from AI agents

The repository is also a plugin marketplace for Claude Code and Codex, with a `headmesh` plugin (connects an agent to
a Headmesh sandbox on your own hardware) and an `xcelsior` plugin (rent GPUs on Xcelsior Compute). Details are in
[plugins/README.md](plugins/README.md).

```bash
claude plugin marketplace add headmesh-dev/headmesh --sparse .claude-plugin --sparse plugins
claude plugin install headmesh@headmesh
```

```bash
codex plugin marketplace add headmesh-dev/headmesh --sparse .agents/plugins --sparse plugins
codex plugin add headmesh@headmesh
```

For agents reading about Headmesh: [headmesh.dev/llms.txt](https://headmesh.dev/llms.txt) indexes the docs, and every
docs page has a Markdown copy (for example [headmesh.dev/docs/quickstart.md](https://headmesh.dev/docs/quickstart.md)).
Agents working on this code should read [AGENTS.md](AGENTS.md).

## Repository layout

| Path | What lives there |
|---|---|
| `cmd/` | Binaries: `headmesh` (CLI), `headmesh-server` (API, OAuth, workers), `headmesh-cellagent` (beside each Headscale) |
| `core/` | Domain logic: tenancy, devices, enrollment, grants, audit, the policy compiler |
| `api/` | Public REST API (OpenAPI 3.1) and the OAuth authorization server |
| `cli/` | CLI commands, including isolated `tailscaled` instance management |
| `internal/` | Shared plumbing: config, logging, database, Headscale client |
| `proto/` | Cell agent protocol and vendored Headscale API definitions |
| `db/` | Migrations and SQL queries (sqlc) |
| `sandbox/` | Agent sandbox image, Compose profiles (Regular / YOLO), `headmesh-sandboxd` MCP server |
| `plugins/` | Claude Code and Codex plugin marketplace (Headmesh, Xcelsior) |
| `web/marketing/` | headmesh.dev (deployed separately from the product) |
| `web/app/` | Sign-up page and dashboard (embedded into `headmesh-server`) |
| `deploy/` | Self-hosting and production deployment, recovery tooling, break-glass WireGuard |
| `docs/` | Blueprint (`handoff.md`), implementation plan (`plan.md`), ADRs, runbooks |

## Development

```bash
mise install          # pinned Go, Node, pnpm, buf, sqlc, golangci-lint
mise run generate     # protobuf + sqlc code generation
mise run test         # unit + property tests (race detector)
mise run lint
```

Integration tests start Postgres, Headscale and Tailscale clients with Docker: `mise run test:integration`.

### Environment files

Each real environment file is git-ignored and has a committed example beside it. The root `.env` holds
workstation operator credentials; the application and deployment script read their own files.

| File | Purpose |
|---|---|
| [`.env.example`](.env.example) → `.env` | Operator tokens for npm, Hetzner and Ubuntu Pro; not automatically loaded by services |
| [`web/marketing/.env.example`](web/marketing/.env.example) → `web/marketing/.env` | Local marketing builds and waitlist service |
| [`deploy/env/local.env.example`](deploy/env/local.env.example) → `deploy/env/local.env` | This workstation's SSH/deploy settings |
| `deploy/env/test/{build,waitlist,backend}.env` | Test deployment, with a separate example for each file |
| `deploy/env/prod/{build,waitlist,backend}.env` | Production deployment, with a separate example for each file |
| [`sandbox/compose/.env.example`](sandbox/compose/.env.example) → `sandbox/compose/.env` | Local sandbox Compose configuration |

The test deployment targets `test.headmesh.dev`, and production targets `headmesh.dev`. Test uses
Turnstile test keys and logs confirmation links instead of sending email; production needs real Turnstile
and Resend credentials. Keep credentials scoped to the tools or service that use them. Database credentials,
backend signing keys and cell certificates are generated separately on the server for each environment.
See [deployment configuration](deploy/README.md#configuration) and [marketing setup](web/marketing/README.md#one-time-setup-aaryn).

## Security

See [SECURITY.md](SECURITY.md). Please report vulnerabilities privately.
