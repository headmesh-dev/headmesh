# ADR 0001: names and domain

- Status: accepted (2026-10-26; npm 2026-10-27)
- Plan: §0, §5.5

## Context

The product needs a name it owns on GitHub, npm and DNS from day one, separate from Xcelsior (handoff, "Go-to-market").
The plain `headmesh` GitHub organization is taken by an unrelated, empty 2022 account.

## Decision

- GitHub organization **`headmesh-dev`**; the monorepo is `github.com/headmesh-dev/headmesh`.
- Domain **headmesh.dev**, registered at Porkbun with DNS on Cloudflare.
- Contacts **hello@headmesh.dev** and **aaryn.biro@headmesh.dev** (Cloudflare Email Routing to the Xcelsior inbox;
  sending needs a mailbox provider, still open in §19).
- npm organization **`headmesh`**, reserving `@headmesh/cli`, `@headmesh/sdk` and the per-platform CLI packages.
  CI publishes with npm trusted publishing.

## Consequences

The Go module path, plugin homepages, marketplace metadata, the CLI's install URL (`get.headmesh.dev`) and every
public URL use these names. Nothing references the taken `headmesh` GitHub name.
