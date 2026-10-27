// Validates what gen.ts produced against the published Agent Plugins 1.0.0 schemas, which
// are vendored in plugins/schemas/agent-plugins-1.0.0 so CI never depends on the network,
// and checks every SKILL.md against the Agent Skills frontmatter rules.
//
//   pnpm -C plugins validate             validate plugins/*/plugin.json, mcp.json and skills
//   pnpm -C plugins validate --upstream  also check the vendored schemas still match agent-plugins.org

import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { parse } from "yaml";
import {
  AGENT_PLUGINS_MCP_SCHEMA,
  AGENT_PLUGINS_PLUGIN_SCHEMA,
  PLUGINS_DIR,
  SCHEMAS_DIR,
  compileSchema,
  describeErrors,
  readJson,
  repoPath,
} from "./common.ts";

const VENDORED = [
  { url: AGENT_PLUGINS_PLUGIN_SCHEMA, file: "agent-plugins-1.0.0/plugin.schema.json", target: "plugin.json" },
  { url: AGENT_PLUGINS_MCP_SCHEMA, file: "agent-plugins-1.0.0/mcp.schema.json", target: "mcp.json" },
];

// Agent Skills: lowercase letters, digits and single hyphens, 1-64 characters, equal to
// the skill's directory name; description 1-1024 characters.
const SKILL_NAME = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

function pluginDirs(): string[] {
  return readdirSync(PLUGINS_DIR, { withFileTypes: true })
    .filter((d) => d.isDirectory() && existsSync(join(PLUGINS_DIR, d.name, "plugin.source.yaml")))
    .map((d) => join(PLUGINS_DIR, d.name));
}

function validateManifests(dirs: string[]): string[] {
  const failures: string[] = [];
  for (const { url, file, target } of VENDORED) {
    const id = (readJson(join(SCHEMAS_DIR, file)) as { $id?: string }).$id;
    if (id !== url) failures.push(`${file} has $id ${id}, expected ${url}`);
    const validate = compileSchema(file);
    for (const dir of dirs) {
      const path = join(dir, target);
      if (!validate(readJson(path))) failures.push(`${repoPath(path)} does not match ${url}:\n${describeErrors(validate)}`);
    }
  }
  return failures;
}

function validateSkills(dirs: string[]): string[] {
  const failures: string[] = [];
  for (const dir of dirs) {
    const skillsDir = join(dir, "skills");
    if (!existsSync(skillsDir)) continue;
    for (const entry of readdirSync(skillsDir, { withFileTypes: true }).filter((d) => d.isDirectory())) {
      const path = join(skillsDir, entry.name, "SKILL.md");
      const where = repoPath(path);
      if (!existsSync(path)) {
        failures.push(`${where} is missing`);
        continue;
      }
      const match = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(path, "utf8"));
      const front = match ? (parse(match[1] ?? "") as Record<string, unknown> | null) : null;
      const name = front?.name;
      const description = front?.description;
      if (typeof name !== "string" || !SKILL_NAME.test(name) || name.length > 64 || name !== entry.name) {
        failures.push(`${where}: frontmatter name must be ${entry.name} (lowercase, digits, single hyphens, at most 64 characters)`);
      }
      if (typeof description !== "string" || description.length === 0 || description.length > 1024) {
        failures.push(`${where}: frontmatter description must be 1-1024 characters`);
      }
    }
  }
  return failures;
}

async function compareUpstream(): Promise<string[]> {
  const failures: string[] = [];
  for (const { url, file } of VENDORED) {
    const response = await fetch(url, { signal: AbortSignal.timeout(15_000) });
    if (!response.ok) {
      failures.push(`GET ${url} returned ${response.status}`);
      continue;
    }
    const upstream = JSON.stringify(await response.json());
    if (upstream !== JSON.stringify(readJson(join(SCHEMAS_DIR, file)))) {
      failures.push(`plugins/schemas/${file} differs from ${url}; re-vendor it and re-run validation`);
    }
  }
  return failures;
}

const dirs = pluginDirs();
const failures = [...validateManifests(dirs), ...validateSkills(dirs)];
if (process.argv.includes("--upstream")) failures.push(...(await compareUpstream()));
if (failures.length > 0) {
  console.error(failures.join("\n"));
  process.exitCode = 1;
} else {
  console.log(`Validated ${dirs.length} plugins against Agent Plugins 1.0.0 and Agent Skills.`);
}
