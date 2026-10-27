# Handoff: Headscale Sandbox Plugin, Xcelsior Plugin, and Plugin Marketplace

## Your role

You are picking up an infrastructure project for Aaryn Biro, founder of Xcelsior Compute (a GPU rental marketplace). Read this whole brief before acting. Ask before making destructive changes, exposing new network access, or deleting anything.

## Goal

Give cloud AI agents (Codex and Claude) a local-like environment where they work with Aaryn's real files, packages and private services, reached through his self-hosted Headscale network. Package this as a plugin. Then package Xcelsior as a second plugin, and publish both from Aaryn's own plugin marketplace.

## Background and constraints

- Aaryn tried the Tailscale VPN option in cloud agent settings. It failed because his tailnet uses a self-hosted Headscale control server, and the managed cloud VPN has no Headscale setting.
- A plugin cannot change the cloud agent's own built-in terminal or VPN. The design here is different: the agent drives a sandbox running on Aaryn's machine, and that sandbox is what joins Headscale.
- Codex and Claude both handle private access the same way: a process inside the private network makes an outbound connection to the cloud. Relevant options are OpenAI's Secure MCP Tunnel, `codex exec-server` (customer-owned execution environments), Claude Remote Control, and Claude self-hosted runners (Team/Enterprise).
- Codex's marketplace loader recognizes both `.agents/plugins/marketplace.json` and `.claude-plugin/marketplace.json`, so one repository can serve both ecosystems.

## Host machine (ASUS), already verified

- Docker is running.
- `/dev/net/tun` exists.
- The Headscale service is active.
- `codex exec-server` is available. Account access and cloud registration have not been verified.
- The main Xcelsior repo is on Aaryn's GitHub account `aabiro`. Xcelsior already has an OAuth-protected MCP service.

## Chosen architecture

A Docker container on the ASUS acts as a persistent sandbox. It runs a Tailscale client pointed at Aaryn's Headscale server. Cloud agents reach it through MCP Tunnel, and through the native Codex executor where that is supported.

Flow: cloud agent, then MCP Tunnel, then runtime on the ASUS, then sandbox container. The container has mounted project files, installed toolchains and a Headscale network connection to private services.

The sandbox should provide:

- Real networking: its own Headscale node, with routes and MagicDNS for private services.
- Real files: mounted host folders.
- Real dependencies: a base OS matching the host, plus project dependency files and persistent caches.
- Interactive execution: shell sessions, command output, stdin, background processes, file editing and artifacts.
- Persistent state: reconnect to the same workspace, and report whether processes survived a disconnect.

## Decisions already made

- Storage: use host-mounted folders (bind mounts) for everything that matters. They are plain folders on the ASUS that Aaryn can see and back up, and they are only deleted deliberately. Docker volumes can be wiped by commands like `docker volume prune` or `docker compose down -v`, so use them only for disposable package caches.
- Visibility: Aaryn wants the agent to see broadly. Default to broad read access, with write access only to project folders. Make the mount list and ACLs easy to widen or narrow.

## Persistence rules

- Mounted folders survive everything.
- Data inside the container survives a stop and restart, but is lost if the container is recreated.
- Running processes are always lost when the container stops.
- Bind-mount these from the host: `/var/lib/tailscale` (so the node keeps its Headscale identity and does not need new auth keys), the agent's home folder (shell history and config), and project and data directories.
- Package caches (pip, npm and so on) can go in Docker volumes.

## Known pitfalls to handle up front

1. TUN and permissions: grant `/dev/net/tun` and `NET_ADMIN`, or Tailscale falls back to slower userspace networking.
2. Key expiry: use a reusable Headscale pre-auth key and disable key expiry for the sandbox node, otherwise it silently drops off the network.
3. DNS: check that MagicDNS names resolve inside the container. A common symptom is pinging an IP working while names fail.
4. File ownership: run the container as Aaryn's user ID so files written to mounts are not owned by root.
5. Package compatibility: toolchains mounted from the host break if the container's OS or libc differs (for example Alpine vs Ubuntu). Match the host's base OS, or install dependencies inside the container from project files.
6. GPU access: install and configure the NVIDIA Container Toolkit early, because Xcelsior is GPU work.
7. Host uptime: disable sleep on the ASUS and set the container restart policy to `unless-stopped` or `always`.

## Security requirements

- A cloud agent will have a shell on Aaryn's machine, so treat this as high risk.
- Give the sandbox its own Headscale node, with ACLs that set exactly which machines and ports it can reach.
- Require authentication on the tunnel and MCP endpoints.
- Never mount secrets folders (SSH keys, cloud credentials, password stores) unless Aaryn explicitly asks.

## Build order

1. Headscale Sandbox plugin: sandbox lifecycle, Headscale enrollment, mounts, dependencies, interactive execution and connection diagnostics.
2. Cloud connection: MCP Tunnel integration first, then native `codex exec-server` attachment where supported.
3. Xcelsior plugin: package the existing OAuth-protected MCP service. This is the quickest win and can be done in parallel to set up the marketplace early.
4. Marketplace: one repository holding both plugins, with catalog metadata for Codex (`.agents/plugins/marketplace.json`) and Claude (`.claude-plugin/marketplace.json`).

## Acceptance test for step 1

The sandbox plugin is done when a cloud agent can:

1. Run a command in the sandbox.
2. Read a real project file from a mounted folder.
3. Use a project dependency.
4. Reach a private service over Headscale by its MagicDNS name.
5. Disconnect, reconnect and find the same workspace and Headscale identity.

After step 1, a bonus check: recreate the container from scratch and confirm the files, Tailscale identity and home config all persist.

## Claude Remote Control (Aaryn already has it)

If Aaryn just needs an agent working with his real environment quickly, Claude Remote Control runs on his own machine and gives local access with nothing to build. The custom plugin is worth building as a product, and for use across both Codex and Claude.

Aaryn has Claude Remote Control set up already. Use it as the immediate fallback for real work while the plugin is being built, and as a baseline to compare the sandbox experience against.

## Other execution options Aaryn has access to

Aaryn is on a Claude Team plan, so the two options below are both available and both interest him. Evaluate each against the custom sandbox before committing to the full plugin build.

| Option | How it works | What to check |
| --- | --- | --- |
| Claude self-hosted runners | Cloud Claude tasks are queued, then a runner inside Aaryn's network picks them up and executes them on his own hardware. Documented for Team and Enterprise plans. | Confirm it is enabled for his Team workspace. Test whether the runner can run inside the same Docker sandbox, with the same bind mounts and Headscale node, so Claude cloud tasks get the sandbox directly. |
| `codex exec-server` | Makes a customer-owned machine or container the Codex agent's execution environment, with shell, filesystem and local MCP servers. Already installed on the ASUS. | Verify account access and cloud registration. Test running it inside the sandbox container so Codex tasks execute there. |

Recommended approach: the Docker sandbox is the shared foundation, and each option is a different front door into it. Self-hosted runners serve Claude, `codex exec-server` serves Codex, and the MCP Tunnel plugin covers any agent that can't use either. If both native options work well, the MCP Tunnel layer may only be needed for packaging and the marketplace, so report back before building it in full.

Add to the acceptance test: run the five checks once through a Claude self-hosted runner and once through `codex exec-server`, in addition to the MCP Tunnel route.

## All-in-one setup (Aaryn's target)

Aaryn wants every option running together on one shared sandbox, not a choice between them. Build one Docker sandbox image with the bind mounts, toolchains and Headscale node, then run every entry point inside it.

| Entry point | Serves | Notes |
| --- | --- | --- |
| Claude self-hosted runner | Claude cloud tasks | Runs the whole Claude agent session inside the sandbox. Not an MCP server; works like a GitHub Actions self-hosted runner, polling for queued tasks. |
| `codex exec-server` | Codex tasks | Makes the sandbox the Codex execution environment. |
| MCP Tunnel plugin | Any other agent | Optional fallback for agents that can't use the first two. Also the piece packaged for the marketplace. |
| Claude Remote Control | Aaryn's interactive sessions | Keep it alongside the others. |

All entry points share the same files, packages, Headscale network and git setup, so configuration happens once.

Requirements for running them together:

- Concurrency: two agents editing the same folder at once will overwrite each other. By default agents work directly on main, and Aaryn sometimes runs two at once on the same repo. He tells one or both agents that another agent is working there. Don't add a hard lock that blocks this. Instead, when told another agent is active, an agent should stick to its own files, make small frequent commits, pull and rebase before every push, avoid reformatting or mass-editing files it isn't working on, and ask Aaryn rather than overwrite changes it didn't make. A lock can be an optional setting. Separate branches or worktrees are used only when Aaryn asks for them.
- Process supervision: run the entry points under a supervisor (s6, supervisord, or one Compose service per entry point on the shared image and mounts), so each restarts independently.
- Modes: every entry point respects the Regular and YOLO mode currently active (next section).
- Diagnostics: one status command showing which entry points are connected, the active mode, Headscale connectivity and running tasks.

## Two sandbox modes: Regular and YOLO

Aaryn wants two modes. Build them as separate Docker Compose profiles sharing one image, so switching modes is a deliberate act.

| Setting | Regular mode (default) | YOLO mode (opt-in) |
| --- | --- | --- |
| Files | Project and data folders mounted read-write | Whole drive mounted read-only, with home and project folders read-write |
| Git and GitHub | Sandbox-only deploy key or fine-grained token, scoped to chosen repos | Dedicated sandbox SSH key added to Aaryn's GitHub account, giving full repo access |
| Headscale | Sandbox node with tight ACLs | Sandbox node with broad ACLs across the tailnet |
| When to use | Any task, including unattended cloud tasks | Only in sessions Aaryn is actively watching |

Rules for YOLO mode:

- Never the default. Starting it requires explicitly selecting the YOLO profile, and the next start falls back to Regular.
- Use a dedicated SSH key created only for the sandbox, not Aaryn's personal key, so it can be revoked in one click without breaking his own logins. Aaryn has considered giving it his personal key, so flag the trade-off clearly if he asks again.
- Keep system folders read-only even in YOLO mode, so the agent can read anywhere but cannot break the OS.
- Make sure a backup or snapshot of the home folder exists before YOLO is first used, since git only protects repos.
- Avoid YOLO for tasks that ingest untrusted content (web pages, public issues), because of prompt-injection risk.
- Show the active mode clearly in diagnostics and at the start of each session.

Git workflow: by default, agents work directly on main and push to origin main. Use a separate branch, worktree or PR only when Aaryn asks for it on a specific task. Before pushing, pull and rebase on origin main, and stop to ask if there are conflicts rather than force-pushing. Never force-push main. Set the git name and email inside the container so commits are attributed properly.

## Headmesh: the product name and longer-term plan

The sandbox plugin and its networking layer are called Headmesh (working domain: headmesh.dev, not yet bought). Aaryn wants Headmesh to grow into its own Tailscale-style networking product, with Xcelsior as its first customer, and to build all of it in parallel.

Structure:

- Headmesh lives in its own repository and stays general-purpose. Xcelsior consumes it through Headmesh's API.
- Xcelsior-specific logic (GPU-owner onboarding flow, billing hooks, renter isolation rules) lives in the Xcelsior repo, not in Headmesh.
- Build on top of Headscale, the Tailscale clients and WireGuard (or the embedded Tailscale library, `tsnet`). Do not rewrite the networking or crypto from scratch.

Core features, generic so any user can use them:

- One-click onboarding: an enrollment API plus invite links or one-line install commands that join a machine to the right network with the right permissions. Xcelsior uses it as "connect your GPU"; anyone else uses it to add a device or teammate.
- Multi-tenancy: organisations or workspaces, each fully isolated, with their own members, devices and access rules. Headscale is built around a single organisation's network, so this is the main gap Headmesh fills. Xcelsior uses it to isolate renters from each other and from GPU owners' home networks.
- Agent sandboxes: the Docker sandbox, entry points and modes described above, as a built-in Headmesh feature.
- Fine-grained, temporary access: grant one machine or one service to one tenant for a set time, then revoke it automatically. This suits rentals and contractors.

Positioning compared with what exists:

- Tailscale: polished and already offers easy onboarding and multiple organisations, but its control server is closed and hosted by Tailscale.
- Headscale: open and self-hosted, but single-organisation and minimal on onboarding.
- Headmesh: open and self-hostable like Headscale, multi-tenant like Tailscale, and designed for sharing machines between parties who don't trust each other, such as renters, owners and AI agents.

Parallel build order, with each track shipping something small and usable:

1. Core: tenant model, enrollment API, and access rules on top of Headscale.
2. Xcelsior integration: GPU-owner onboarding and renter isolation, used as the real-world test of every core feature.
3. Agent sandbox: the plugin, entry points and modes described earlier.
4. Consumer slice: a CLI and a sign-up page first, to test demand before building desktop or mobile apps.

## Headmesh marketing site (headmesh.dev)

This is the public marketing site, separate from the Headmesh app or dashboard. It lives in the `web/marketing` folder of the Headmesh repo and is deployed on its own, so marketing changes never touch the product.

Go-to-market approach:

- Headmesh has its own identity and domain from day one, because it is general-purpose and anyone can use it.
- Xcelsior is the flagship customer and showcase ("Xcelsior runs on Headmesh").
- Keep marketing light until Xcelsior proves it in production: a landing page, an open GitHub repo and a waitlist. Use waitlist sign-ups to measure demand outside Xcelsior.

Site requirements:

- A static site (for example Astro or Next.js static export), fast, with no login.
- Pages: home or landing, docs (can link to the repo docs at first), GitHub link, and a waitlist form.
- The waitlist stores email and an optional "what would you use it for" field.
- Simple, privacy-friendly analytics.
- Clean, developer-focused look, dark mode supported.

Draft landing page copy:

Headline: Your machines, one private mesh. Open, self-hosted, multi-tenant.

Subheadline: Headmesh builds on Headscale and WireGuard to give teams, renters and AI agents secure access to exactly the machines they need, and nothing else.

Feature blocks:

- One-click onboarding: invite a device, teammate or GPU with a single link or command.
- True multi-tenancy: isolated workspaces with their own members, devices and rules.
- Temporary access: grant one machine or service for a set time, then it revokes itself.
- Agent sandboxes: let cloud AI agents work on your real files and private services safely, in a scoped or full-access mode.
- Yours to run: open source and self-hostable, with no closed control server.

Proof section: Xcelsior Compute runs on Headmesh, connecting GPU owners and renters without exposing anyone's home network.

Call to action: Join the waitlist, or star the project on GitHub.

Headline variations:

1. Plain and technical: "The open, multi-tenant mesh for machines you share."
2. Aimed at AI agents: "Give your AI agents a real machine, not a toy sandbox."
3. Punchy: "Share machines, not your network."
4. Self-hosting angle: "Tailscale-style networking you actually own."
5. Bold: "One mesh. Every machine. Zero blind trust."

Recommendation: use variation 3 as the main headline and variation 2 as a subheadline or the heading of the agent sandbox section.

## Repository layout

Headmesh repo:

- `core`: the control layer on top of Headscale (tenants, enrollment, access rules, temporary grants).
- `api`: the public API that Xcelsior and other customers call.
- `cli`: the command-line tool (join, invite, status).
- `sandbox`: Docker image, Compose file with the regular and YOLO profiles, and the entry points (Claude self-hosted runner, codex exec-server, MCP Tunnel).
- `web/marketing`: the headmesh.dev marketing site.
- `web/app`: the sign-up page and, later, the dashboard.
- `plugins`: the plugin marketplace, with Headmesh and Xcelsior as the first entries.
- `docs`: documentation, including this handoff brief.
- `deploy`: self-hosting and deployment scripts.

Xcelsior repo:

- One integration folder that talks to the Headmesh API, containing GPU-owner onboarding, billing hooks and renter isolation rules.

## Accounts and setup status

- GitHub: create the Headmesh organization on the Free plan, owned by "A business or institution" with Xcelsior Compute Inc. as the business. Upgrade to Team only when there are teammates or important private repos.
- npm: create a free "headmesh" organization to reserve the @headmesh scope (for example @headmesh/cli and @headmesh/sdk). Also claim the plain "headmesh" package name if it is free.
- Contact email: use hello@headmesh.dev once the domain is bought and email forwarding works (for example through Cloudflare Email Routing or the registrar). Until then, use aaryn.biro@xcelsior.ca.

## Open to-dos

- Buy headmesh.dev and check the renewal price.
- Set up email forwarding for hello@headmesh.dev.
- Confirm Claude self-hosted runners are enabled on the Team workspace.
- Verify codex exec-server account access and cloud registration.
- Find out how Claude and Codex cloud interfaces show diffs when work runs on Aaryn's machine.

## Tenant security model

Isolation between tenants is the core promise, so it must be designed and tested from the start:

- Every tenant gets its own isolated network, with deny-by-default access rules.
- Cross-tenant access only happens through explicit, time-limited grants that are logged.
- Revocation takes effect immediately and removes keys and routes.
- Automated tests try to reach other tenants' machines and must fail.
- Every enrollment, grant and revocation produces an audit log entry.

## License and business model

Placeholder, not yet decided. One common option is an open-source core with a paid hosted version. Aaryn will decide this later.

## Success metrics

- Waitlist sign-ups on headmesh.dev.
- GPU owners onboarded to Xcelsior through Headmesh.
- The agent sandbox passes the five-step acceptance test.

## Working style

Aaryn likes specific, strategic recommendations. Where there is a real choice, offer options and give a clear recommendation.
