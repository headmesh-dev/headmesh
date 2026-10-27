---
description: Run the five Headmesh sandbox acceptance checks over MCP and save the result as an artifact
argument-hint: "[resume <session-id>]"
---

Run the Headmesh sandbox acceptance checks through the `headmesh-sandbox` MCP server. The route name for this run is `claude-code-plugin`.

Arguments: `$ARGUMENTS`

## If the arguments start with `resume`

Go straight to **Check 5, part 2** with the session ID given, then write the result (see "Result").

## Otherwise, run checks 1 to 5

Start with `sandbox_status`. Record `boot_id`, the tailnet node name and IP, and the mode. Then:

1. **Run a command.** Use `exec` to run `id -u; uname -a; cat /etc/os-release`. It passes when `id -u` prints `1000` and `os-release` reports Ubuntu 26.04.
2. **Read a real project file.** Use `exec` to run `sha256sum /mnt/storage/projects/xcelsior/README.md`. Record the hash. The host verifier compares it with the host's copy, so this check can only report the hash, not pass or fail on its own.
3. **Use a project dependency.** Use `exec` with `cwd` `/mnt/storage/projects/xcelsior` to run `uv run --frozen python -c "import fastapi, psycopg; print(fastapi.__version__)"`, with `timeout_s` 600. It passes on exit 0 with a version printed.
4. **Reach a private service by MagicDNS name.** Use `exec` to run `getent hosts prod-hetzner.mesh.xcelsior.ca`. Then use `net_check` on each allowed target that `sandbox_status` lists (for example `tower-server.mesh.xcelsior.ca`, port 11434) and on one denied target. It passes when the name resolves, the allowed targets connect and the denied target is refused.
5. **Disconnect and reconnect, part 1.** Use `session_create` with name `acceptance-<timestamp>` and `cwd` `/tmp`. Use `session_send` to run `echo $$ > /tmp/headmesh-acceptance.pid; touch /tmp/headmesh-acceptance.marker; sleep 86400`. Record the session ID, then stop and tell the user:

   > Check 5 needs a real reconnect. Run `/mcp`, choose **headmesh-sandbox → Reconnect** (or start a new Claude Code session), then run `/headmesh:sandbox-acceptance resume <session-id>`.

   Before stopping, save the partial result for checks 1 to 4 (see "Result") with check 5 marked `pending`.

## Check 5, part 2 (after `resume <session-id>`)

Call `sandbox_status`. It passes when all of these hold:

- the session is `running`;
- `/tmp/headmesh-acceptance.marker` exists (`fs_stat`);
- the tailnet node and IP are the same as in part 1 (read them from the saved partial result through `artifact_list` and `artifact_get`).

Then clean up: `session_kill` the session, and use `fs_delete` on the marker and pid files.

If the session is `lost` instead, the container restarted between the two parts. Check 5 fails. Report it as the `headmesh-sandbox` skill describes.

## Result

Write the result with `artifact_put` under the name `acceptance/claude-code-plugin-<UTC timestamp>.json`, using this shape:

```json
{
  "route": "claude-code-plugin",
  "started_at": "<ISO 8601>",
  "boot_id": "<from sandbox_status>",
  "node": { "name": "<node>", "ip": "<ip>" },
  "checks": [
    { "id": 1, "name": "run-command", "status": "pass|fail", "evidence": "<short output>" },
    { "id": 2, "name": "read-project-file", "status": "reported", "sha256": "<hash>" },
    { "id": 3, "name": "project-dependency", "status": "pass|fail", "evidence": "<fastapi version or error>" },
    { "id": 4, "name": "magicdns-and-policy", "status": "pass|fail", "evidence": "<resolution, allowed and denied results>" },
    { "id": 5, "name": "disconnect-reconnect", "status": "pass|fail|pending", "session_id": "<id>" }
  ]
}
```

Finish with a table of the checks and their status, plus the artifact name. Don't mark a check as passing unless you saw the evidence.
