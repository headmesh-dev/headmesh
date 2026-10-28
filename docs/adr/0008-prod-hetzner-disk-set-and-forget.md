# ADR 0008: prod-hetzner disk, set and forget

- Status: accepted (2026-10-26; automation deployed 2026-10-27)
- Plan: §0, §5.4

## Decision

Reclaim once (build cache and old images, never volumes, never images a container references), then make the box
maintain itself: a weekly `prod-docker-gc.timer` that keeps rollback tags, a journald cap, disk alerts at 80% and 90%
plus a fill projection, and BuildKit garbage collection. The root cause, building images on prod, moves to CI.

## Consequences

Status 2026-10-28: the reclaim, timer, journald cap and alerts are live (Xcelsior `94fe3f63`, `aad7be4e`). Still
open: installing `daemon.json` needs a Docker restart window, the loop volume's backing files can still grow inside
`/`, and images are still built on prod.
