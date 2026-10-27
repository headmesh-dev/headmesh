// Generates every plugin manifest, MCP config and marketplace catalog from the YAML
// sources, so the Claude Code and Codex views of a plugin can't drift apart.
//
//   pnpm -C plugins gen        write the outputs
//   pnpm -C plugins gen:check  exit 1 if any output differs from the file on disk
//
// Sources: plugins/marketplace.source.yaml and plugins/<name>/plugin.source.yaml.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { parse } from "yaml";
import {
  AGENT_PLUGINS_MCP_SCHEMA,
  AGENT_PLUGINS_PLUGIN_SCHEMA,
  PLUGINS_DIR,
  REPO_ROOT,
  compileSchema,
  describeErrors,
  repoPath,
} from "./common.ts";

// OpenAI's build docs put per-server auth under extensions["com.openai"].auth in mcp.json,
// but the Agent Plugins 1.0.0 MCP schema closes server objects, and Codex 0.162.1 and
// 0.164.0 silently drop any server that carries the key. The sources keep the intended
// auth so it is one switch away; flip this only after the Codex install smoke test shows
// the server still loads.
const EMIT_OPENAI_SERVER_AUTH = false;

type Headers = Record<string, string>;

interface Person {
  name: string;
  email?: string;
  url?: string;
}

interface McpServerSource {
  url: string;
  headers?: Headers;
  claude?: { url?: string; headers?: Headers };
  openaiAuth?: Record<string, unknown>;
}

interface PluginSource {
  name: string;
  displayName: string;
  version: string;
  description: string;
  author: Person;
  homepage: string;
  repository: string;
  license: string;
  keywords: string[];
  links: { website: string; privacyPolicy: string; termsOfService: string };
  userConfig?: Record<string, Record<string, unknown>>;
  mcpServers: Record<string, McpServerSource>;
  openai: { interface: Record<string, unknown> };
}

interface MarketplaceSource {
  name: string;
  displayName: string;
  description: string;
  owner: Person;
  plugins: {
    name: string;
    category: string;
    codex: { category: string; installation: string; authentication: string };
  }[];
}

function load<T>(path: string, schemaFile: string): T {
  const data: unknown = parse(readFileSync(path, "utf8"));
  const validate = compileSchema(schemaFile);
  if (!validate(data)) {
    throw new Error(`${repoPath(path)} does not match ${schemaFile}:\n${describeErrors(validate)}`);
  }
  return data as T;
}

// Agent Plugins clients must not expand placeholders in url or headers, so a ${...} there
// would be sent literally. Claude-only overrides belong under `claude:` instead.
function assertNoPlaceholders(plugin: string, server: string, s: McpServerSource): void {
  const values = [s.url, ...Object.values(s.headers ?? {})];
  if (values.some((v) => v.includes("${"))) {
    throw new Error(`${plugin}: mcpServers.${server} has a placeholder in a portable field; move it under claude:`);
  }
}

function openaiInterface(p: PluginSource): Record<string, unknown> {
  const { shortDescription, longDescription, developerName, category, capabilities, ...rest } = p.openai.interface;
  return {
    displayName: p.displayName,
    shortDescription,
    longDescription,
    developerName,
    category,
    capabilities,
    websiteURL: p.links.website,
    privacyPolicyURL: p.links.privacyPolicy,
    termsOfServiceURL: p.links.termsOfService,
    ...rest,
  };
}

function metadata(p: PluginSource) {
  return {
    name: p.name,
    version: p.version,
    description: p.description,
    author: p.author,
    homepage: p.homepage,
    repository: p.repository,
    license: p.license,
    keywords: p.keywords,
  };
}

// .claude-plugin/plugin.json
function claudeManifest(p: PluginSource) {
  const { name, ...rest } = metadata(p);
  return {
    name,
    displayName: p.displayName,
    ...rest,
    privacyPolicyUrl: p.links.privacyPolicy,
    termsOfServiceUrl: p.links.termsOfService,
    userConfig: p.userConfig,
  };
}

// .mcp.json (Claude Code)
function claudeMcp(p: PluginSource) {
  const servers: Record<string, unknown> = {};
  for (const [id, s] of Object.entries(p.mcpServers)) {
    const headers = { ...s.headers, ...s.claude?.headers };
    servers[id] = {
      type: "http",
      url: s.claude?.url ?? s.url,
      headers: Object.keys(headers).length > 0 ? headers : undefined,
    };
  }
  return { mcpServers: servers };
}

// plugin.json (Agent Plugins 1.0.0, read by Codex first)
function portableManifest(p: PluginSource) {
  return {
    $schema: AGENT_PLUGINS_PLUGIN_SCHEMA,
    ...metadata(p),
    extensions: { "com.openai": { interface: openaiInterface(p) } },
  };
}

// mcp.json (Agent Plugins 1.0.0)
function portableMcp(p: PluginSource) {
  const servers: Record<string, unknown> = {};
  for (const [id, s] of Object.entries(p.mcpServers)) {
    assertNoPlaceholders(p.name, id, s);
    servers[id] = {
      type: "streamable-http",
      url: s.url,
      headers: s.headers,
      extensions: EMIT_OPENAI_SERVER_AUTH && s.openaiAuth ? { "com.openai": { auth: s.openaiAuth } } : undefined,
    };
  }
  return { $schema: AGENT_PLUGINS_MCP_SCHEMA, mcpServers: servers };
}

// .codex-plugin/plugin.json: the compatibility layout for Codex builds that predate the
// portable manifest. It points at the portable mcp.json, because Claude's .mcp.json holds
// ${user_config.*} references that Codex would send literally.
function codexManifest(p: PluginSource) {
  return {
    ...metadata(p),
    skills: "./skills/",
    mcpServers: "./mcp.json",
    interface: openaiInterface(p),
  };
}

function claudeMarketplace(m: MarketplaceSource, plugins: Map<string, PluginSource>) {
  return {
    name: m.name,
    owner: m.owner,
    description: m.description,
    plugins: m.plugins.map((entry) => ({
      name: entry.name,
      source: `./plugins/${entry.name}`,
      description: plugins.get(entry.name)?.description,
      category: entry.category,
    })),
  };
}

function codexMarketplace(m: MarketplaceSource) {
  return {
    name: m.name,
    interface: { displayName: m.displayName },
    plugins: m.plugins.map((entry) => ({
      name: entry.name,
      source: { source: "local", path: `./plugins/${entry.name}` },
      policy: { installation: entry.codex.installation, authentication: entry.codex.authentication },
      category: entry.codex.category,
    })),
  };
}

function render(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function outputs(): Map<string, string> {
  const marketplace = load<MarketplaceSource>(join(PLUGINS_DIR, "marketplace.source.yaml"), "marketplace-source.schema.json");
  const plugins = new Map<string, PluginSource>();
  for (const { name } of marketplace.plugins) {
    const source = join(PLUGINS_DIR, name, "plugin.source.yaml");
    if (!existsSync(source)) throw new Error(`marketplace lists ${name}, but ${repoPath(source)} is missing`);
    const plugin = load<PluginSource>(source, "plugin-source.schema.json");
    if (plugin.name !== name) throw new Error(`${repoPath(source)} names itself ${plugin.name}, expected ${name}`);
    plugins.set(name, plugin);
  }

  const files = new Map<string, string>();
  files.set(join(REPO_ROOT, ".claude-plugin", "marketplace.json"), render(claudeMarketplace(marketplace, plugins)));
  files.set(join(REPO_ROOT, ".agents", "plugins", "marketplace.json"), render(codexMarketplace(marketplace)));
  for (const plugin of plugins.values()) {
    const dir = join(PLUGINS_DIR, plugin.name);
    files.set(join(dir, ".claude-plugin", "plugin.json"), render(claudeManifest(plugin)));
    files.set(join(dir, ".mcp.json"), render(claudeMcp(plugin)));
    files.set(join(dir, "plugin.json"), render(portableManifest(plugin)));
    files.set(join(dir, "mcp.json"), render(portableMcp(plugin)));
    files.set(join(dir, ".codex-plugin", "plugin.json"), render(codexManifest(plugin)));
  }
  return files;
}

function main(): number {
  const check = process.argv.includes("--check");
  const stale: string[] = [];
  for (const [path, content] of outputs()) {
    const current = existsSync(path) ? readFileSync(path, "utf8") : undefined;
    if (current === content) continue;
    stale.push(repoPath(path));
    if (!check) {
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, content);
    }
  }
  if (check && stale.length > 0) {
    console.error(`Generated files are out of date:\n${stale.map((p) => `  ${p}`).join("\n")}\nRun: pnpm -C plugins gen`);
    return 1;
  }
  console.log(stale.length > 0 ? `Updated:\n${stale.map((p) => `  ${p}`).join("\n")}` : "Generated files are up to date.");
  return 0;
}

process.exitCode = main();
