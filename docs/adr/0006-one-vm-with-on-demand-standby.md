# ADR 0006: one VM with an on-demand standby

- Status: accepted (2026-10-26)
- Plan: §0, §13.1, §13.3–§13.5

## Context

A warm standby server costs as much as the primary and sits idle. The control plane's state is small: Headscale's
SQLite database, its Noise and DERP keys, and Postgres.

## Decision

- One VM, **hm-prod-1** in Hetzner `nbg1`.
- Continuous replication to Cloudflare R2: Litestream (Headscale SQLite, 10 s) and pgBackRest (Postgres WAL), with the
  keys age-encrypted.
- Detection from outside (a Cloudflare Worker cron), fencing through the Hetzner API, and a scripted restore to
  `fsn1` or `hel1` with a capacity fallback chain. The restored server starts in **recovery mode** (ADR 0013): it
  audits overdue grant expiries and waits for every cell to apply a current policy before it accepts writes.
- No Canadian data-residency requirement.

## Consequences

About €6 a month instead of about €20. Recovery is measured by weekly restore drills and a monthly game day with a
15-minute target. The ASUS is not a standby for hs1.
