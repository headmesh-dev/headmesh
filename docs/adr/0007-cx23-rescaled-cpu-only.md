# ADR 0007: create hm-prod-1 as a cx23 and rescale CPU/RAM only

- Status: accepted (2026-10-26; server created 2026-10-27)
- Plan: §0, §13.1

## Decision

Create hm-prod-1 now, as a `cx23` (2 vCPU, 4 GB, 40 GB), so expiring Hetzner credits pay for it. While credits last,
rescale **CPU and RAM only** to `cx33`; that keeps the 40 GB disk, so it can drop back to `cx23` (€5.99/month with
IPv4) afterwards. Stay on `cx33` (€8.99/month) only if memory use passes about 70% or API latency regresses in load
tests.

## Consequences

A server created as `cx33` would get an 80 GB disk and could never return to `cx23`. Postgres is tuned for 4 GB
(`shared_buffers = 512MB`) and the host has a 2 GB swap file.
