# ADR 0003: two Headscale cells

- Status: accepted (2026-10-26)
- Plan: §0, §9.2, §9.12, §13.8

## Context

Xcelsior already runs Headscale at `hs.xcelsior.ca`, and its code hardcodes `100.64.0.x` addresses. Public
sign-ups need a network that Xcelsior's production tailnet never depends on.

## Decision

Headmesh manages Headscale servers as **cells**, and starts with two:

- **`xcelsior`**: the existing `hs.xcelsior.ca`, adopted in place (observe first, enforce later), keeping every node
  ID and IP. No re-enrollment.
- **`hs1`**: a fresh cell at `hs1.headmesh.dev` (MagicDNS `mesh.headmesh.dev`) on hm-prod-1 for everyone else.

A tenant is pinned to one cell; cross-tenant grants require both orgs in the same cell.

## Consequences

Cells scale horizontally later. The compiler and the cell protocol are per cell from the start, and the adoption of
`xcelsior` is a reviewed, reversible procedure (§13.8) rather than a migration.
