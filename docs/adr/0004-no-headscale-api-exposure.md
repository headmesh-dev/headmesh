# ADR 0004: Headscale's API is never exposed

- Status: accepted (2026-10-26)
- Plan: §0, §9.4, §14

## Context

Whoever holds a Headscale API key controls the whole tailnet. The xcelsior cell is Xcelsior's production control
plane on prod-hetzner, which also hosts its WireGuard hub.

## Decision

- A **cell agent** runs beside each Headscale, talks to it over the local unix socket (or loopback), and only
  connects **outbound** to headmesh-server over mutual TLS. Headscale's gRPC port is never exposed, and no Headscale
  API key leaves its host.
- The agent enforces local guardrails even against a buggy or compromised server: signed, expiring operations;
  protected nodes; delete and retag rate limits; must-keep reachability invariants; last-known-good rollback; and a
  local freeze and revoke.
- Any design that opens Headscale's gRPC API needs Aaryn's explicit approval first.

## Consequences

No inbound port opens on prod-hetzner for Headmesh. Policy timelines are precomputed and pushed, so scheduled grant
expiries are enforced even while the server is unreachable.
