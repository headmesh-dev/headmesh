# ADR 0011: tenancy on Headscale tagged nodes, without a fork

- Status: accepted (2026-10-27)
- Plan: §9.2, §9.3

## Decision

- Headmesh never forks Headscale. Multi-tenancy is a policy Headmesh compiles from its own database.
- Every Headmesh-enrolled node is a **tagged** node (pre-auth keys carry `acl_tags`, no user), so it never expires:
  `tag:hm-o-<org>` for org membership, `tag:hm-r-<org>-<role>` for roles, `tag:hm-n-<device>` per device.
- The compiler emits `grants` only (no legacy `acls`), `tagOwners` for every tag, and Headscale policy `tests` as
  isolation canaries, so Headscale itself refuses a policy that breaks isolation.
- A separate verifier re-derives every cross-org edge from the snapshot, and property tests compare the compiled
  policy with an independent model.

## Consequences

Isolation is enforced in five layers (§14). Headscale's peer filtering keeps other tenants out of a node's netmap,
proven on real Headscale and Tailscale by `test/e2e`.
