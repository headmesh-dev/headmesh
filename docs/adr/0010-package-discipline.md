# ADR 0010: package discipline

- Status: accepted (2026-10-27)
- Plan: §3.2, §3.3

## Decision

Every track starts from the libraries, tools and versions pinned in the plan. A session may switch only to a
**better** option: a newer stable release, or a more battle-tested, better-maintained library for the same job,
recorded in the commit message and, when it replaces a library, in an ADR. Never a weaker, pre-release or merely
equivalent one, and no dependency the plan doesn't need. Lockfiles are committed, and Renovate brings upgrades.

**Amended 2026-10-29 (Aaryn):** no minimum release age. The newest stable release is used as soon as it ships; the
3-day wait in Renovate and pnpm is removed. Pre-releases are still excluded, and the lockfiles, digest pins and CI
scanners (osv-scanner, govulncheck, gitleaks) are the safeguards against a bad release.

## Consequences

Deviations so far are recorded where they happen: Go 1.26.10 for standard-library fixes (`937e158`), the
`argon2` addition for the sandbox (sandbox ADR 0001), and nginx in place of Caddy (ADR 0013).
