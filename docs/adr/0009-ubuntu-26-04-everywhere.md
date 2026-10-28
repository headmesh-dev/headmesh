# ADR 0009: Ubuntu 26.04 LTS everywhere

- Status: accepted (2026-10-27)
- Plan: §0, §3.3, §5.7

## Decision

Every machine and image targets Ubuntu 26.04 LTS ("resolute"): the ASUS (26.04.1), the tower, hm-prod-1 and
prod-hetzner (upgraded from 24.04 on 2026-10-27, Aaryn running each command). Container bases are `ubuntu:26.04`
pinned by digest. CI runs on `ubuntu-26.04` runners.

## Consequences

- One libc and one package set, so toolchains match between host, sandbox and CI (handoff pitfall 5).
- Third-party apt sources point at `resolute`.
- Ubuntu 26.04 restricts unprivileged user namespaces through AppArmor, so Chrome-based CI steps (Lighthouse) lift
  that restriction on the throwaway runner.
- PostgreSQL 16 on prod-hetzner still runs the noble build and needs PGDG `resolute` or an upgrade to 18 (open).
