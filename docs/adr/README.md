# Architecture decision records

One record per decision locked in plan §0 and per later change to the plan (plan §3.3). Sandbox-specific decisions
and spike notes are in [`sandbox/docs/adr/`](../../sandbox/docs/adr/).

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](0001-names-and-domain.md) | GitHub org `headmesh-dev`, domain headmesh.dev, npm org `headmesh` | accepted 2026-10-26 |
| [0002](0002-go-for-the-core.md) | Go for core, API, CLI, sandbox daemon and cell agent | accepted 2026-10-26 |
| [0003](0003-two-headscale-cells.md) | Two Headscale cells: adopt `xcelsior` in place, add `hs1` | accepted 2026-10-26 |
| [0004](0004-no-headscale-api-exposure.md) | Headscale's API stays on its host; an outbound cell agent bridges it | accepted 2026-10-26 |
| [0005](0005-codex-front-doors.md) | Codex: SSH primary, plus Secure MCP Tunnel and exec-server, all in M1 | accepted 2026-10-26 |
| [0006](0006-one-vm-with-on-demand-standby.md) | One VM in nbg1, replication to R2, scripted restore elsewhere | accepted 2026-10-26 |
| [0007](0007-cx23-rescaled-cpu-only.md) | Create hm-prod-1 as `cx23`; rescale CPU/RAM only | accepted 2026-10-26 |
| [0008](0008-prod-hetzner-disk-set-and-forget.md) | prod-hetzner disk: reclaim once, then self-maintaining | accepted 2026-10-26 |
| [0009](0009-ubuntu-26-04-everywhere.md) | Ubuntu 26.04 LTS on every machine and image | accepted 2026-10-27 |
| [0010](0010-package-discipline.md) | Start from the pinned packages; switch only to better ones | accepted 2026-10-27 |
| [0011](0011-tenancy-on-tagged-nodes.md) | Tenancy on Headscale tagged nodes and `grants`, no fork | accepted 2026-10-27 |
| [0012](0012-shared-checkout-on-main.md) | Parallel tracks share one checkout on `main` | accepted 2026-10-27 |
| [0013](0013-nginx-and-systemd-on-hm-prod-1.md) | nginx + systemd on hm-prod-1; headmesh.dev self-hosted there | accepted 2026-10-28 |
| [0014](0014-core-spike-outcomes.md) | Spikes T-4, T-5, T-12, T-13, T-14: what the core and plugins found | accepted 2026-10-28 |
