// Post-build step: fill the CSP in nginx/security-headers.conf with the SHA-256 hashes of every inline <script> and
// <style> in the built HTML, so the policy allows exactly the inline code we ship and nothing else. The result goes
// to build/nginx/security-headers.conf, which deploy/deploy.sh ships with the release for the vhost to include.
//
// Usage: node scripts/csp.mjs [distDir] [outFile]   (run by `pnpm build` after `astro build`)
import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

/** Keep each header well under the 4–8 KiB response-header limits of common proxies and browsers. */
export const MAX_HEADER_LINE = 4000;

const TEMPLATE = fileURLToPath(new URL("../nginx/security-headers.conf", import.meta.url));

const SCRIPT_RE = /<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi;
const STYLE_RE = /<style\b[^>]*>([\s\S]*?)<\/style\s*>/gi;
// Inline event handlers (onclick=…) can't be allowed by a hash-based policy without 'unsafe-hashes'.
const EVENT_HANDLER_RE = /<[a-z][^>]*\son[a-z]+\s*=/i;

/** CSP source expression for some inline content: 'sha256-<base64>'. */
export function cspHash(content) {
  return `'sha256-${createHash("sha256").update(content, "utf8").digest("base64")}'`;
}

/** Inline (no src) script bodies and style bodies in one HTML document, exactly as the browser hashes them. */
export function extractInline(html) {
  const scripts = [];
  for (const [, attrs, body] of html.matchAll(SCRIPT_RE)) {
    if (!/\ssrc\s*=/i.test(` ${attrs}`)) scripts.push(body);
  }
  const styles = [...html.matchAll(STYLE_RE)].map(([, body]) => body);
  return { scripts, styles, hasEventHandlers: EVENT_HANDLER_RE.test(html) };
}

/** Replace the hash placeholders in the headers template and check no header grows unreasonably long. */
export function fillHeaders(template, { scriptHashes, styleHashes }) {
  for (const placeholder of ["{{SCRIPT_HASHES}}", "{{STYLE_HASHES}}"]) {
    if (!template.includes(placeholder)) throw new Error(`headers template is missing ${placeholder}`);
  }
  const join = (hashes) => [...new Set(hashes)].sort().join(" ");
  const filled = template
    .replaceAll("{{SCRIPT_HASHES}}", join(scriptHashes))
    .replaceAll("{{STYLE_HASHES}}", join(styleHashes))
    .replace(/ {2,}/g, " ")
    .replaceAll(" ;", ";");
  const tooLong = filled.split("\n").filter((line) => line.length > MAX_HEADER_LINE);
  if (tooLong.length > 0) {
    throw new Error(`headers have ${tooLong.length} line(s) over ${MAX_HEADER_LINE} characters; reduce inline code`);
  }
  return filled;
}

async function* htmlFiles(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) yield* htmlFiles(path);
    else if (entry.name.endsWith(".html")) yield path;
  }
}

export async function run(distDir, outFile) {
  const scriptHashes = [];
  const styleHashes = [];
  const offenders = [];
  for await (const file of htmlFiles(distDir)) {
    const { scripts, styles, hasEventHandlers } = extractInline(await readFile(file, "utf8"));
    scriptHashes.push(...scripts.map(cspHash));
    styleHashes.push(...styles.map(cspHash));
    if (hasEventHandlers) offenders.push(file);
  }
  if (offenders.length > 0) {
    throw new Error(`Inline event handlers would be blocked by the CSP:\n- ${offenders.join("\n- ")}`);
  }
  const filled = fillHeaders(await readFile(TEMPLATE, "utf8"), { scriptHashes, styleHashes });
  await mkdir(dirname(outFile), { recursive: true });
  await writeFile(outFile, filled);
  return { scripts: new Set(scriptHashes).size, styles: new Set(styleHashes).size };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const distDir = process.argv[2] ?? fileURLToPath(new URL("../dist", import.meta.url));
  const outFile = process.argv[3] ?? fileURLToPath(new URL("../build/nginx/security-headers.conf", import.meta.url));
  const { scripts, styles } = await run(distDir, outFile);
  console.log(`csp: hashed ${scripts} inline script(s) and ${styles} inline style(s) into ${outFile}`);
}
