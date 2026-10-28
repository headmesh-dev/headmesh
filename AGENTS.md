# Working in this repository (for AI coding agents)

Headmesh is an open, self-hostable, multi-tenant control layer for Headscale meshes. Start with `README.md`, then
`docs/plan.md` (architecture, milestones, security model) and `docs/adr/` (decisions). `LICENSING.md` says which
license covers which path.

## Layout

- Go module `github.com/headmesh-dev/headmesh` (Go 1.26): `cmd/` binaries, `core/` (policy compiler, cell agent and
  hub), `api/` (REST API), `internal/` (store, services, Headscale client), `db/` (goose migrations, sqlc queries),
  `cli/`, `proto/` + `gen/` (cell protocol, vendored Headscale API), `test/e2e/` (real Headscale + Tailscale).
- `sandbox/` is a separate Go module: the agent sandbox image, host CLI and `headmesh-sandboxd` MCP server.
- pnpm workspace at the root: `web/marketing` (headmesh.dev) and `plugins` (Claude Code and Codex marketplace).
- `deploy/`: `deploy.sh` (nginx, blue-green services on hm-prod-1), the server bootstrap, the base-host bootstrap,
  break-glass WireGuard and the cell agent package.

## Commands

Toolchains are pinned in `mise.toml` (`mise install`). If `/tmp` is small, point `TMPDIR` and `GOTMPDIR` elsewhere.

| What | Command |
| --- | --- |
| Go unit and property tests | `CGO_ENABLED=1 go test -race ./...` |
| Postgres integration tests (Docker) | `CGO_ENABLED=1 go test -race -tags integration ./internal/store/ ./api/` |
| Isolation on real Headscale + Tailscale (Docker, `/dev/net/tun`) | `go test -tags e2e ./test/e2e/` |
| Go lint | `golangci-lint run --build-tags integration,e2e ./...` |
| Regenerate protobuf and sqlc code (committed; CI diffs it) | `mise run generate` |
| Sandbox module | `cd sandbox && go test -race ./...` |
| Marketing site | `pnpm install`, then `pnpm --filter @headmesh/marketing run lint`, `test`, `build`, `test:e2e` |
| Plugins | `pnpm plugins:gen`, then `pnpm --dir plugins run check` |
| Deploy pipeline (Docker) | `deploy/tests/e2e.sh` |

## Rules

- **Isolation is the product.** Changes to `core/policy`, grants or the cell agent need property tests and should
  pass `test/e2e`. Never emit `*`, `autogroup:internet` or `autogroup:danger-all` from tenant data.
- **Licensing boundary.** The Apache-2.0 parts (`cli/`, `cmd/headmesh`, `sandbox/`) must not import the AGPL-3.0
  packages (`api/`, `core/`, `internal/`, `db/`).
- **Packages.** Start from the versions pinned in the plan; switch only to a better one (newer stable or better
  maintained) and say why in the commit, plus an ADR when replacing a library (ADR 0010). Commit lockfiles.
- **Git.** Work on `main` unless asked for a branch, in small conventional commits (`feat(scope): …`). Run
  `git pull --rebase` before pushing. Never force-push `main`. Other agents may be working in the same checkout: touch
  only your own files and ask before overwriting changes you didn't make.
- **Secrets and internal details.** Never commit `.env` files, `deploy/env/*/*.env` or anything under
  `docs/internal/`. Keep server addresses, host inventories and open security findings out of tracked files; they
  belong in the internal ops notes. CI runs gitleaks over the whole history.
- **Production.** hm-prod-1 and prod-hetzner are production. Announce any step that touches them before it runs, and
  get the owner's go-ahead for anything that changes access, firewalls or Headscale policy.
