---
name: headmesh-network
description: Enroll devices into a Headmesh network and grant temporary, port-scoped access between organizations with the headmesh CLI. Use when the user wants to add a machine to their mesh, invite a device or teammate, or give someone time-limited access to a service.
---

# Devices and temporary access with the `headmesh` CLI

> **Status:** these commands arrive with Headmesh core (milestone M2). Before using any of them, run `headmesh version` and `headmesh --help`. If the CLI is missing or a command isn't listed, tell the user it isn't available yet rather than guessing at flags.

Every command accepts `--json`. Use it and read the structured result, rather than parsing human-readable output.

## Check where you are

- `headmesh whoami`: the signed-in account and organization. If it fails, ask the user to run `headmesh login`. It opens a browser device flow, so you can't complete it for them.
- `headmesh status --json`: this machine's node, IP, MagicDNS name and connection state.
- `headmesh devices ls --json`: devices in the organization.

## Enroll a device

- On the machine itself: `headmesh join <token-or-url> [--name <name>]`.
  - If the machine is already on another tailnet, `join` creates an isolated instance so it doesn't disturb the existing one. You can force this with `--isolated` and pick a name with `--instance <name>`.
  - `join` asks for sudo only for the steps that need it, and asks before installing Tailscale. Let the user answer those prompts.
- To enroll another machine, create an invite and give the user the one-liner: `headmesh invite device [--ttl 24h] [--uses 1] --json`.
- To invite a person: `headmesh invite member --role <role> --json`.

Treat join tokens and invite links as secrets. Show them only to the user who asked, and don't write them into files, commits or logs.

## Grant temporary access

Grants are how one organization reaches a specific port on another organization's device for a limited time:

```bash
headmesh grant create --to <org> --device <device> --ports tcp:22 --ttl 4h --json
headmesh grant ls --json
headmesh grant extend <grant-id> --ttl 2h --json
headmesh grant revoke <grant-id> --json
```

- Before creating a grant, confirm the recipient organization, the device, the exact ports and the TTL with the user. Never widen ports or TTL beyond what they asked for.
- Prefer the shortest TTL that does the job, and offer to revoke the grant when the work is done.
- `headmesh rules preview` shows what a rules change would allow before you `rules apply`. Run the preview and show it to the user first.

## Audit

`headmesh audit tail --json` shows recent enrollments, grants and revocations, and `headmesh audit verify` checks the log's integrity. Use them to confirm that a change you made took effect.
