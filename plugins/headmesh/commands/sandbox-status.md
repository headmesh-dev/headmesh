---
description: Show the Headmesh sandbox's health, mode, tailnet path, sessions and other active agents
allowed-tools: mcp__plugin_headmesh_headmesh-sandbox__sandbox_status, mcp__plugin_headmesh_headmesh-sandbox__agents_list
---

Call the `sandbox_status` tool on the `headmesh-sandbox` MCP server, then report:

1. **Overall**: one line that says healthy, degraded or down, followed by the mode. Write **YOLO** in bold if the sandbox is in YOLO mode, with the time left on its TTL.
2. **Tailnet**: the node name, IP and MagicDNS name, whether it is online, and for each configured target whether it is reached directly or through DERP.
3. **Entry points**: one line each for the Claude runner, Remote Control, the OpenAI tunnel and exec-server, with any that aren't healthy listed first.
4. **Sessions and processes**: a table of name, ID, state (`running`, `exited(code)` or `lost`) and command. If any are `lost`, follow the `headmesh-sandbox` skill's "Reporting lost processes" section.
5. **Other agents**: who else is active in which repositories, from `agents_list` if `sandbox_status` doesn't include it.
6. **Resources**: GPU mode and usage, cache disk usage, and the age of the last backup if it is reported.

Leave out empty sections. If the MCP server can't be reached, say so, show the error and suggest checking the sandbox URL and token in `/plugin` → headmesh → Configure. Don't retry in a loop.

$ARGUMENTS
