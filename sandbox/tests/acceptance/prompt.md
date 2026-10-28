# Headmesh sandbox acceptance (plan §6.15)

You are running the Headmesh sandbox acceptance test. Do only what this file
says. Don't change any project files.

Set ROUTE to the front door you arrived through:

| You are… | ROUTE |
|---|---|
| a Claude cloud task on the self-hosted runner | `runner` |
| Codex over an SSH remote connection | `ssh` |
| Codex driven by `headmesh-sandbox agent run` (exec-server) | `exec-server` |
| ChatGPT or Codex over the OpenAI Secure MCP Tunnel | `tunnel` |

## If you have a shell (runner, ssh, exec-server)

1. Register: `hm agents register --agent <your entry point> --task "acceptance" --repo xcelsior`.
2. Run phase 1:

   ```
   headmesh-sandbox acceptance run --route ROUTE --phase 1
   ```

   It runs checks 1–4 and starts a marker session for check 5.
3. Check the same things yourself and say whether they agree:
   - `id -u` prints 1000, and `/etc/os-release` says `VERSION_ID="26.04"`.
   - `sha256sum /mnt/storage/projects/xcelsior/README.md`.
   - `cd /mnt/storage/projects/xcelsior && uv run --frozen python -c "import fastapi, psycopg; print(fastapi.__version__)"`.
   - `getent hosts prod-hetzner.mesh.xcelsior.ca`.
4. **Disconnect.** End this turn or session (runner: finish the task; SSH: close the
   connection; exec-server: let the run end). Aaryn or the harness starts a new one.
5. In the new session, run phase 2:

   ```
   headmesh-sandbox acceptance run --route ROUTE --phase 2
   ```

6. Report the two summaries and the result file paths it prints
   (`/var/lib/headmesh/artifacts/acceptance/ROUTE-*.json`).
7. Unregister: `hm agents unregister`.

## If you only have MCP tools (tunnel)

Use the `headmesh-sandboxd` tools and write the result yourself.

1. `agents_register {agent: "chatgpt-tunnel", task: "acceptance", repos: ["xcelsior"]}`.
2. Check 1: `exec {command: "id -u; uname -a; cat /etc/os-release"}`. Pass when the uid is
   1000 and `VERSION_ID="26.04"`.
3. Check 2: `exec {command: "sha256sum /mnt/storage/projects/xcelsior/README.md"}`.
4. Check 3: `exec {command: "cd /mnt/storage/projects/xcelsior && uv run --frozen python -c \"import fastapi, psycopg; print(fastapi.__version__)\"", timeout_s: 110}`.
   If it times out on the tunnel's 110 s limit, run it with `process_start` and
   read `process_logs` until it exits.
5. Check 4: `exec {command: "getent hosts prod-hetzner.mesh.xcelsior.ca"}`, then
   `net_check {target: "tower-server.mesh.xcelsior.ca:11434"}` (must connect) and
   `net_check {target: "100.64.0.1:5432"}` (must not).
6. Check 5, part 1: `sandbox_status` (note `tailnet.node_id` and `tailnet.ips`), then
   `session_create {name: "acceptance-tunnel", shell: ["bash", "-c", "echo $$ > ~/.hm-acceptance-tunnel && exec sleep 86400"]}`.
   Note the session id. **End the conversation turn** and continue in a new chat.
7. Check 5, part 2 (new chat): `session_list` shows that session `running`;
   `exec {command: "cat ~/.hm-acceptance-tunnel"}` works; `sandbox_status` shows the
   same node id and IPs. Then `session_kill {id, signal: "KILL", remove: true}`.
8. Write the result with `fs_write` to
   `/var/lib/headmesh/artifacts/acceptance/tunnel-all-<UTC timestamp YYYYMMDDTHHMMSSZ>.json`
   in this shape (one entry per check, `pass` true or false, `details` as text,
   and for check 2 `data: {"sha256": "<hash>"}`):

   ```json
   {"route": "tunnel", "phase": "all", "started_at": "...", "finished_at": "...",
    "pass": true, "passed": 5, "total": 5,
    "checks": [{"id": 1, "name": "run a command", "pass": true, "details": "..."}]}
   ```

9. `agents_unregister`.

Aaryn verifies every route on the host with `headmesh-sandbox acceptance verify`.
