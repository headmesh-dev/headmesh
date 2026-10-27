import { readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";

export const PLUGINS_DIR = join(import.meta.dirname, "..");
export const REPO_ROOT = join(PLUGINS_DIR, "..");
export const SCHEMAS_DIR = join(PLUGINS_DIR, "schemas");

export const AGENT_PLUGINS_PLUGIN_SCHEMA = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json";
export const AGENT_PLUGINS_MCP_SCHEMA = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json";

export function repoPath(path: string): string {
  return relative(REPO_ROOT, path);
}

export function readJson(path: string): unknown {
  return JSON.parse(readFileSync(path, "utf8"));
}

// One validator per schema file under plugins/schemas, compiled with Ajv's strict mode
// so a schema typo fails loudly instead of silently accepting everything.
export function compileSchema(file: string): ValidateFunction {
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  return ajv.compile(readJson(join(SCHEMAS_DIR, file)) as object);
}

export function describeErrors(validate: ValidateFunction): string {
  return (validate.errors ?? [])
    .map((e) => `  ${e.instancePath || "/"} ${e.message ?? "is invalid"}${e.params ? ` ${JSON.stringify(e.params)}` : ""}`)
    .join("\n");
}
