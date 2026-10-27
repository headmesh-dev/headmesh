# Headmesh plugins for Claude Code and Codex

This repository is a plugin marketplace named `headmesh` that serves both Claude Code and Codex. It has two plugins:

| Plugin | What it does | MCP server |
|---|---|---|
| `headmesh` | Work in a Headmesh agent sandbox on your own machine: run commands, keep sessions alive across disconnects, edit project files, and share a repository safely with other agents. | `headmesh-sandbox`, at your sandbox's URL, with a bearer token |
| `xcelsior` | Rent and manage GPUs on Xcelsior Compute, with a confirmation step before anything is billed or destroyed. | `xcelsior` at `https://mcp.xcelsior.ca/mcp`, signed in with OAuth |

Each plugin also ships skills, which both tools load, and slash commands, which only Claude Code loads.

| Plugin | Skills | Commands (Claude Code) |
|---|---|---|
| `headmesh` | `headmesh-sandbox`, `multi-agent-git`, `headmesh-network` | `/sandbox-status`, `/sandbox-acceptance` |
| `xcelsior` | `xcelsior-gpu-rental`, `xcelsior-provider` | `/gpu-prices`, `/my-instances` |

## Install in Claude Code

Add the marketplace, then install the plugins you want:

```bash
claude plugin marketplace add headmesh-dev/headmesh --sparse .claude-plugin --sparse plugins
```

```bash
claude plugin install xcelsior@headmesh
```

```bash
claude plugin install headmesh@headmesh
```

`--sparse` checks out only the marketplace file and the plugins, not the rest of the monorepo.

**xcelsior:** the first time a session uses the server, Claude Code opens your browser so you can sign in to Xcelsior. You can also start the sign-in from `/mcp`.

**headmesh:** the plugin asks for two values. Set them in `/plugin` → **headmesh** → **Configure**. You can also pass the URL at install time with `--config sandbox_url=…`, but enter the token in the dialog so it doesn't end up in your shell history.

- **Sandbox MCP URL**: defaults to `https://asus.sbx.headmesh.dev/mcp`. Use your sandbox's MagicDNS URL on your tailnet, or its Headmesh ingress URL.
- **Sandbox token**: the `hms_…` bearer token your sandbox issued for this client. Claude Code keeps it in your system keychain, not in `settings.json`.

### Make the marketplace available to a whole team

Add this to the project's `.claude/settings.json`. Teammates who trust the project are offered the marketplace and the enabled plugins:

```json
{
  "extraKnownMarketplaces": {
    "headmesh": {
      "source": {
        "source": "github",
        "repo": "headmesh-dev/headmesh",
        "sparsePaths": [".claude-plugin", "plugins"]
      }
    }
  },
  "enabledPlugins": {
    "xcelsior@headmesh": true,
    "headmesh@headmesh": true
  }
}
```

## Install in Codex

```bash
codex plugin marketplace add headmesh-dev/headmesh --sparse .agents/plugins --sparse plugins
```

```bash
codex plugin add xcelsior@headmesh
```

```bash
codex plugin add headmesh@headmesh
```

Codex reads `.agents/plugins/marketplace.json`. The ChatGPT desktop app also lists it on its own when you open a clone of this repository; the Codex CLI needs the `marketplace add` above.

**xcelsior:** sign in with `codex mcp login xcelsior`. Codex finds Xcelsior's OAuth server from the MCP server's protected-resource metadata, so there is nothing to configure.

**headmesh:** Codex can't take the URL and token from the plugin, because the portable `mcp.json` format allows neither placeholders nor secrets. Register the server yourself under the same name, and your entry replaces the plugin's:

```bash
codex mcp add headmesh-sandbox --url https://asus.sbx.headmesh.dev/mcp --bearer-token-env-var HEADMESH_SANDBOX_TOKEN
```

Then export `HEADMESH_SANDBOX_TOKEN` in the shell that starts Codex. From milestone M3 the sandbox signs you in with OAuth, and this step goes away.

To turn a plugin on for everyone working in a repository, add it to that repository's `.codex/config.toml`:

```toml
[plugins."xcelsior@headmesh"]
enabled = true
```

## How the files fit together

Each plugin is generated from one source file, `plugins/<name>/plugin.source.yaml`. The two marketplace files are generated from `plugins/marketplace.source.yaml`. Don't edit the generated files by hand: CI fails if they differ from what the generator produces.

| File | Read by | Generated |
|---|---|---|
| `.claude-plugin/marketplace.json` | Claude Code. Codex too, if `.agents/plugins/marketplace.json` is missing. | yes |
| `.agents/plugins/marketplace.json` | Codex | yes |
| `plugins/<name>/.claude-plugin/plugin.json` | Claude Code | yes |
| `plugins/<name>/.mcp.json` | Claude Code. It uses `${user_config.*}`. | yes |
| `plugins/<name>/plugin.json` | Codex, and any [Agent Plugins](https://agent-plugins.org) 1.0.0 client. Codex settings go under `extensions["com.openai"]`. | yes |
| `plugins/<name>/mcp.json` | Codex, and any Agent Plugins client | yes |
| `plugins/<name>/.codex-plugin/plugin.json` | Older Codex builds that predate the portable manifest. It points at the portable `mcp.json`. | yes |
| `plugins/<name>/skills/*/SKILL.md` | Both tools | no |
| `plugins/<name>/commands/*.md` | Claude Code only | no |

The plugins deliberately have no `bin/` directory, because claude.ai refuses to install plugins that have one, and no hooks, because hooks block Codex directory listing.

### Where Codex differs from OpenAI's docs

OpenAI's build docs put per-server authentication under `extensions["com.openai"].auth` in `mcp.json`. The published Agent Plugins 1.0.0 MCP schema doesn't allow extra keys on a server, and Codex 0.162.1 and 0.164.0 silently drop any server that has that key. So the generated `mcp.json` leaves it out, and both servers still work: Codex discovers Xcelsior's OAuth through the server, and Headmesh users register the token as shown above. The intended auth settings stay in each `plugin.source.yaml` under `openaiAuth`. To emit them, flip `EMIT_OPENAI_SERVER_AUTH` in `gen/gen.ts`, but only once the Codex install smoke test shows the server still loads.

## Develop

You need Node 24.16 and pnpm 10, both pinned in `mise.toml`.

```bash
pnpm --dir plugins install
```

Regenerate after editing a source file:

```bash
pnpm --dir plugins gen
```

Run what CI checks: generator drift, the Agent Plugins schemas, and skill frontmatter:

```bash
pnpm --dir plugins check
```

Run Claude Code's own validation:

```bash
claude plugin validate --strict .
```

To try your working copy, add it as a local marketplace: `claude plugin marketplace add ./` or `codex plugin marketplace add ./`. Claude Code reads a local marketplace in place. Codex copies each plugin into `~/.codex/plugins/cache/`, so run `codex plugin add` again after each change.

`gen/validate.ts` checks the generated files against copies of the official schemas in `schemas/agent-plugins-1.0.0/`, so CI doesn't need the network. `pnpm --dir plugins validate --upstream` also checks that those copies still match agent-plugins.org. The other two schemas in `schemas/` describe the YAML sources and give editors completion through the `yaml-language-server` comment at the top of each source file.

CI is `.github/workflows/plugins.yml`. Besides the checks above, it installs `xcelsior@headmesh` into both Claude Code and Codex in a clean Ubuntu 26.04 container and checks the resulting MCP server definition.

### Release

Bump `version` in the plugin's `plugin.source.yaml`, run `pnpm --dir plugins gen`, commit, then tag:

```bash
claude plugin tag plugins/xcelsior --push
```

This creates and pushes a `xcelsior--v<version>` tag after checking that the manifest and the marketplace entry agree.
