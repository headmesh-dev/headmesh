// Build explicit-OAuth OpenAI draft ZIPs without changing working Codex manifests.
// Usage: pnpm --dir plugins package:submissions --out /absolute/new/output
import { createHash } from "node:crypto";
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { isAbsolute, join, relative, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { parse } from "yaml";
import { PLUGINS_DIR, REPO_ROOT, compileSchema, describeErrors } from "./common.ts";

const args = process.argv.slice(2);
const outIndex = args.indexOf("--out");
if (outIndex < 0 || !args[outIndex + 1] || !isAbsolute(args[outIndex + 1]!)) {
  throw new Error("Supply --out with a new absolute output directory");
}
const output = resolve(args[outIndex + 1]!);
if (existsSync(output)) throw new Error("Output already exists; choose a new directory to preserve previous artifacts");
const review = JSON.parse(readFileSync(join(PLUGINS_DIR, "submission.source.json"), "utf8"));
const hash = (value: Buffer | string) => createHash("sha256").update(value).digest("hex");
const json = (value: unknown) => JSON.stringify(value, null, 2) + "\n";
const receipts: unknown[] = [];
mkdirSync(output, { recursive: true, mode: 0o700 });

for (const name of ["headmesh", "xcelsior"]) {
  const sourceDir = join(PLUGINS_DIR, name);
  const source = parse(readFileSync(join(sourceDir, "plugin.source.yaml"), "utf8"));
  const validateSource = compileSchema("plugin-source.schema.json");
  if (!validateSource(source)) throw new Error(describeErrors(validateSource));
  const original = JSON.parse(readFileSync(join(sourceDir, "plugin.json"), "utf8"));
  const manifest = structuredClone(original);
  const mcp = JSON.parse(readFileSync(join(sourceDir, "mcp.json"), "utf8"));
  const config = review[name];
  if (!config || config.positive.length !== 5 || config.negative.length !== 3 || config.shortDescription.length > 30) {
    throw new Error(`${name}: incomplete review cases or invalid subtitle`);
  }
  const extension = manifest.extensions["com.openai"];
  extension.interface.shortDescription = config.shortDescription;
  extension.interface.supportURL = config.supportURL;
  extension.interface.composerIcon = extension.interface.logo;
  // Existing mint is a dark-theme color. Leave the light-theme color to the
  // client rather than submitting a color without enough contrast on white.
  if (extension.interface.brandColor) {
    extension.interface.brandColorDark = extension.interface.brandColor;
    delete extension.interface.brandColor;
  }
  extension.review = { test_cases: { positive: config.positive, negative: config.negative }, commerce: config.commerce, commerce_description: config.commerce_description };
  if (JSON.stringify(extension.interface.defaultPrompt) !== JSON.stringify(original.extensions["com.openai"].interface.defaultPrompt)) {
    throw new Error("An existing starter prompt changed");
  }
  const validateManifest = compileSchema("agent-plugins-1.0.0/plugin.schema.json");
  if (!validateManifest(manifest)) throw new Error(describeErrors(validateManifest));
  const validateMcp = compileSchema("agent-plugins-1.0.0/mcp.schema.json");
  if (!validateMcp(mcp)) throw new Error(describeErrors(validateMcp));
  for (const [id, entry] of Object.entries(mcp.mcpServers) as [string, any][]) {
    const auth = source.mcpServers[id]?.openaiAuth;
    if (auth?.type !== "oauth" || auth.client?.mode !== "cimd" || auth.client.clientId || auth.client.clientSecret || entry.headers && Object.keys(entry.headers).length) {
      throw new Error(`${name}/${id}: expected discovered CIMD OAuth without embedded credentials`);
    }
    const url = new URL(entry.url);
    if (url.protocol !== "https:" || url.username || url.password || url.hash || entry.url.includes("{")) throw new Error("Invalid submission endpoint");
    // Deliberately portal-specific: current portable clients reject this
    // extension even though OpenAI's upload contract requires it.
    entry.extensions = { "com.openai": { auth: structuredClone(auth) } };
  }
  const staging = join(output, name);
  mkdirSync(staging);
  writeFileSync(join(staging, "plugin.json"), json(manifest));
  writeFileSync(join(staging, "mcp.json"), json(mcp));
  const tracked = spawnSync("git", ["ls-files", "-z", "--", `plugins/${name}/assets/`, `plugins/${name}/skills/`], { cwd: REPO_ROOT, encoding: "utf8" });
  if (tracked.status !== 0) throw new Error("Cannot enumerate tracked plugin components");
  const hashes: Record<string, string> = {};
  for (const file of tracked.stdout.split("\0").filter(Boolean).sort()) {
    const path = resolve(REPO_ROOT, file);
    const target = relative(sourceDir, path);
    if (target.startsWith("..") || isAbsolute(target) || lstatSync(path).isSymbolicLink() || /(?:^|\/)\.env(?:\.|$)/.test(target)) throw new Error("Unsafe package path");
    const bytes = readFileSync(path);
    const dest = join(staging, target);
    mkdirSync(resolve(dest, ".."), { recursive: true });
    writeFileSync(dest, bytes);
    hashes[target] = hash(bytes);
    if (hash(readFileSync(dest)) !== hash(bytes)) throw new Error("Component changed during copying");
  }
  for (const field of ["logo", "composerIcon"]) {
    const path = extension.interface[field];
    if (typeof path !== "string" || !path.startsWith("./assets/") || !hashes[path.slice(2)]) throw new Error(`Missing ${field}`);
  }
  const zipPath = join(output, `${name}-openai-draft.zip`);
  const archived = spawnSync("python3", ["-m", "zipfile", "-c", zipPath, "plugin.json", "mcp.json", "assets", "skills"], { cwd: staging, encoding: "utf8" });
  if (archived.status !== 0) throw new Error("ZIP creation failed");
  const integrity = spawnSync("python3", ["-m", "zipfile", "-t", zipPath], { encoding: "utf8" });
  if (integrity.status !== 0) throw new Error("ZIP integrity failed");
  receipts.push({ name, zip: zipPath, sha256: hash(readFileSync(zipPath)), state: "draft-not-submitted", existing_prompts_preserved: true, component_hashes: hashes, remaining: config.remaining });
}
writeFileSync(join(output, "receipt.json"), json(receipts), { mode: 0o600 });
console.log(json(receipts.map(({name, zip, sha256, state}: any) => ({name, zip, sha256, state}))));
