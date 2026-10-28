// Reconstructed CLI for the recorded browser renderer. Exported SVG type is outlined.
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { chromium } from "@playwright/test";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
// Public SVG social cards contain live type. Embed the prepared brand fonts so PNG exports do not depend on
// the workstation's font installation. The 338 candidate SVGs already have outlined type.
const fontFaces = [
  ["Geist", "Geist-400.ttf", 400],
  ["Geist", "Geist-600.ttf", 600],
  ["Geist", "Geist-700.ttf", 700],
  ["Geist Mono", "GeistMono-500.ttf", 500],
  ["Bricolage Grotesque", "BricolageGrotesque-700.ttf", 700],
  ["Bricolage Grotesque", "BricolageGrotesque-800.ttf", 800],
];
const fontCss = (await Promise.all(fontFaces.map(async ([family, filename, weight]) => {
  const data = await readFile(path.join(root, "fonts", filename));
  return `@font-face{font-family:'${family}';font-weight:${weight};font-style:normal;src:url(data:font/ttf;base64,${data.toString("base64")}) format('truetype')}`;
}))).join("\n");
const args = process.argv.slice(2);
let jobs;
if (args[0] === "--batch" && args[1]) {
  jobs = JSON.parse(await readFile(path.resolve(root, args[1]), "utf8"));
} else if (args.length >= 2) {
  jobs = [{ in: args[0], out: args[1], w: Number(args[2] ?? 1500), h: Number(args[3] ?? 900), scale: Number(args[4] ?? 1) }];
} else {
  throw new Error("Usage: node gen/render.js --batch jobs.json | input output [width height scale]");
}

const browser = await chromium.launch({ headless: true });
try {
  let cursor = 0;
  await Promise.all(Array.from({ length: Math.min(4, jobs.length) }, async () => {
    let context;
    let page;
    let scale;
    try {
      while (cursor < jobs.length) {
        const job = jobs[cursor++];
        const requestedScale = job.scale ?? 1;
        if (!context || scale !== requestedScale) {
          if (context) await context.close();
          scale = requestedScale;
          context = await browser.newContext({ deviceScaleFactor: scale });
          page = await context.newPage();
          await page.route("**/*", route => {
            const protocol = new URL(route.request().url()).protocol;
            return ["file:", "data:", "about:"].includes(protocol) ? route.continue() : route.abort();
          });
        }
        const input = path.resolve(root, job.in);
        const output = path.resolve(root, job.out);
        await mkdir(path.dirname(output), { recursive: true });
        await page.setViewportSize({ width: job.w, height: job.h });
        if (input.endsWith(".svg")) {
          const svg = await readFile(input, "utf8");
          await page.setContent(`<html><head><style>${fontCss}\nhtml,body{margin:0;padding:0;background:transparent;overflow:hidden}svg{display:block;width:${job.w}px!important;height:${job.h}px!important}</style></head><body>${svg}</body></html>`, { waitUntil: "load" });
          await page.evaluate(() => document.fonts.ready);
          await page.locator("body > svg").screenshot({ path: output, omitBackground: true, timeout: 45000 });
        } else {
          await page.goto(pathToFileURL(input).href, { waitUntil: "load", timeout: 45000 });
          await page.evaluate(() => document.fonts.ready);
          await page.screenshot({ path: output, fullPage: true, omitBackground: true, timeout: 45000 });
        }
      }
    } finally {
      if (context) await context.close();
    }
  }));
  await mkdir(path.join(root, "tmp"), { recursive: true });
  await writeFile(path.join(root, "tmp/renderer.json"), JSON.stringify({ chromium: browser.version(), jobs: jobs.length }, null, 2));
  console.log(`rendered ${jobs.length}`);
} finally {
  await browser.close();
}
