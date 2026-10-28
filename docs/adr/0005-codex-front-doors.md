# ADR 0005: Codex front doors

- Status: accepted (2026-10-26)
- Plan: §0, §7.3, §7.4, §7.6

## Decision

All three Codex routes ship in M1, on the same sandbox:

- **SSH remote connections** (primary, interactive): Codex desktop connects to `agent@asus-sandbox` over Tailscale
  SSH, which terminates inside the container. Only Aaryn's own devices are allowed.
- **OpenAI Secure MCP Tunnel**: `tunnel-client` dials out to OpenAI and forwards to `headmesh-sandboxd`, for
  ChatGPT and Codex through the Platform.
- **`codex exec-server`** through the Agents API, started per session by a host-side orchestrator that keeps the
  application key outside the sandbox.

The acceptance test runs through SSH **and** exec-server.

## Consequences

Exec-server adds no Hetzner cost (it runs on the ASUS; OpenAI bills Agents API usage). The sandbox track found that
the ChatGPT desktop app also runs `codex exec-server` against Codex cloud environments, which §1 said it didn't; see
`sandbox/docs/adr/spike-t16-codex-exec-server-non-root.md`.
