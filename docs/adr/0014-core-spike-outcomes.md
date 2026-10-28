# ADR 0014: spike outcomes for the core and plugins

- Status: accepted (2026-10-28)
- Plan: §17. The sandbox spikes (T-1, T-2, T-3, T-7, T-8, T-16) are in `sandbox/docs/adr/`.

| Spike | Question | Outcome | Evidence |
| --- | --- | --- | --- |
| T-4 | Headscale 0.29.6 tag limits; does `tagOwners: []` work for API-assigned tags? | **Yes for `tagOwners: []`.** Headscale v0.29.6 accepts `tagOwners` entries with an empty owner list, and nodes registered with tagged pre-auth keys get exactly those tags, so no internal `headmesh@` user is needed. The compiler's tag shapes (`tag:hm-o-…`, `tag:hm-r-…-<role>`, `tag:hm-n-…`) are accepted; no explicit length or charset limit was probed beyond them. | `test/e2e` (6fc1e9b) applies the compiled policy to a real Headscale |
| T-5 | Can Headscale's `hscontrol/policy/v2` be imported as a library? | **Not used.** The fallback was taken: Headscale itself checks every policy (its `tests` block runs on `SetPolicy`), and Headmesh has its own independent verifier (`core/policy/verify.go`) plus property tests against a reference model. | `core/policy`, `test/e2e` |
| T-12 | Which plugin manifest does Codex read? | The root portable `plugin.json` when it has `extensions["com.openai"]`; `.codex-plugin/plugin.json` only otherwise, never merged. With a root manifest, Codex reads the portable `mcp.json` and ignores `.mcp.json`. It prefers `.agents/plugins/marketplace.json` over `.claude-plugin/marketplace.json`. | Tested with Codex 0.162.1 and 0.164.0; `plugins/README.md` |
| T-14 | How do Headscale policy `tests` treat tags with no nodes? | A test whose principals resolve to no addresses **fails**, which would block every policy change until a node registered. Canaries now only use orgs with registered nodes. | `test/e2e` (6fc1e9b) |

## Still open

- **T-13, established flows after revocation:** the e2e suite measures that new connections are refused within
  0.4–3.4 s of a revoke (SLO p99 < 5 s), but doesn't yet hold a TCP connection open across the revoke.
- **T-9, T-10, T-11, T-15, T-17** depend on infrastructure that doesn't exist yet (hs1's Litestream replica, the
  Hetzner API token, the CUDA image, R2 object lock).
