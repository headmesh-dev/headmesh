# ADR 0002: Go for the core

- Status: accepted (2026-10-26)
- Plan: §0, §3.1, §3.2

## Context

Headmesh builds on Headscale, the Tailscale clients and WireGuard, all written in Go. The control layer talks to
Headscale over its gRPC API, compiles its policy format, and ships a CLI and two daemons to many machines.

## Decision

Go 1.26 for `core`, `api`, `cli`, `headmesh-sandboxd` and `headmesh-cellagent`, in one module
(`github.com/headmesh-dev/headmesh`). Xcelsior stays Python and calls the REST API through a generated client.

## Consequences

- Exact API parity with Headscale through stubs generated from its v0.29.6 protos, and the option to reuse its
  policy code (ADR 0014, T-5).
- Static binaries (`CGO_ENABLED=0`) for the CLI, the cell agent and the server, so packaging and the deploy are a
  single file each.
- The JavaScript parts (marketing site, web app, plugin tooling) live in a separate pnpm workspace at the root.
