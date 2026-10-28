# ADR 0015: Apache-2.0 with an AGPL-3.0 control plane

- Status: accepted (2026-10-28, Aaryn chose the plan's recommendation)
- Plan: §19.3, the gate before the repository goes public

## Decision

- **Apache-2.0** for everything people embed, extend or build clients on: the CLI, the agent sandbox, the plugins,
  the cell protocol definitions, the web code, deploy scripts and docs. This is the root `LICENSE`.
- **AGPL-3.0-only** for the control plane: `api/`, `core/`, `internal/`, `db/`, `cmd/headmesh-server/`,
  `cmd/headmesh-cellagent/` and `test/`, each with its own `LICENSE`. Anyone may self-host or modify it; a modified
  version offered to others over a network must publish its source. That protects a future hosted Headmesh from a
  closed fork without limiting self-hosting.
- Vendored Headscale protos keep Headscale's BSD-3-Clause license.
- The Headmesh name and logo are trademarks and not licensed.

`LICENSING.md` maps every directory to its license.

## Consequences

- The split only holds if the Apache-2.0 binaries never link AGPL packages. Today the CLI depends on `cli/` alone and
  the sandbox is a separate module; a CI check should keep it that way.
- **Revocability.** Xcelsior Compute Inc. holds all the copyright, so it can change the terms for future versions,
  for example moving the server to Apache-2.0. Versions already released keep their licenses for everyone who has
  them. Once outside contributors are accepted, relicensing their code needs their consent: adopt a CLA (or a
  DCO plus a copyright-assignment clause) before merging the first external contribution if that flexibility
  matters.
