// Install smoke-test assertions for .github/workflows/plugins.yml. Runs in a bare container
// with only Node, so it must not import anything outside node:*.
//
//   node plugins/gen/assert-install.ts claude <output of `claude plugin list --json`>
//   node plugins/gen/assert-install.ts codex  <output of `codex mcp get xcelsior --json`>
//
// It checks the installed server definition, not mcp__plugin_xcelsior_xcelsior__* tool names:
// listing tools needs a live, signed-in connection to Xcelsior's production MCP server.

import { readFileSync } from "node:fs";

const XCELSIOR_URL = "https://mcp.xcelsior.ca/mcp";

interface ClaudePlugin {
  id: string;
  enabled: boolean;
  errors?: unknown[];
  mcpServers?: Record<string, { type?: string; url?: string }>;
}

interface CodexServer {
  enabled: boolean;
  disabled_reason: string | null;
  transport?: { type?: string; url?: string };
}

function claude(plugins: ClaudePlugin[]): string {
  const plugin = plugins.find((p) => p.id === "xcelsior@headmesh");
  if (!plugin?.enabled) throw new Error("xcelsior@headmesh is not installed and enabled");
  if (plugin.errors?.length) throw new Error(`xcelsior@headmesh has load errors: ${JSON.stringify(plugin.errors)}`);
  const server = plugin.mcpServers?.xcelsior;
  if (server?.type !== "http" || server.url !== XCELSIOR_URL) {
    throw new Error(`unexpected MCP servers: ${JSON.stringify(plugin.mcpServers)}`);
  }
  return "Claude Code: xcelsior@headmesh is enabled with MCP server plugin:xcelsior:xcelsior";
}

function codex(server: CodexServer): string {
  if (!server.enabled) throw new Error(`the xcelsior MCP server is disabled: ${server.disabled_reason}`);
  if (server.transport?.type !== "streamable_http" || server.transport.url !== XCELSIOR_URL) {
    throw new Error(`unexpected MCP transport: ${JSON.stringify(server.transport)}`);
  }
  return "Codex: xcelsior@headmesh is installed with MCP server xcelsior";
}

const [client, file] = process.argv.slice(2);
if (!file || (client !== "claude" && client !== "codex")) {
  throw new Error("usage: assert-install.ts claude|codex <json file>");
}
const data: unknown = JSON.parse(readFileSync(file, "utf8"));
console.log(client === "claude" ? claude(data as ClaudePlugin[]) : codex(data as CodexServer));
