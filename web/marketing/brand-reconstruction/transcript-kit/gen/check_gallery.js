import assert from "node:assert/strict";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { chromium } from "@playwright/test";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  await page.goto(pathToFileURL(path.join(root, "index.html")).href);
  assert.equal(await page.locator("section[data-family]:visible").count(), 13);
  assert.deepEqual(await page.locator("img").evaluateAll(images => images.filter(image => !image.complete || !image.naturalWidth).map(image => image.getAttribute("src"))), []);
  await page.getByRole("button", { name: "C · Weave", exact: true }).click();
  assert.equal(await page.locator("section[data-family]:visible").count(), 3);
  await page.getByRole("button", { name: "D · Arch", exact: true }).click();
  assert.equal(await page.locator("section[data-family]:visible").count(), 4);
  await page.getByRole("button", { name: "All 13", exact: true }).click();
  assert.equal(await page.locator("section[data-family]:visible").count(), 13);
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await writeFile(path.join(root, "gallery-validation.json"), JSON.stringify({ passed: true, loadedImages: await page.locator("img").count(), familyFilters: [13, 3, 4, 13], mobileOverflow: false }, null, 2) + "\n");
  console.log("Offline gallery: all images loaded, family filters passed, 390px viewport has no horizontal overflow.");
  const manifest = JSON.parse(await readFile(path.join(root, "png/manifest.json"), "utf8"));
  const extrasCount = manifest.entries.filter(entry => entry.group === "Extras").length;
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto(pathToFileURL(path.join(root, "png/index.html")).href);
  await page.locator("img").evaluateAll(async images => {
    images.forEach(image => { image.loading = "eager"; });
    await Promise.all(images.map(image => image.decode()));
  });
  assert.equal(await page.locator("article:visible").count(), manifest.count);
  assert.equal(await page.locator("img").count(), manifest.count * 2);
  await page.getByLabel("Show assets:").selectOption("D4");
  assert.equal(await page.locator("article:visible").count(), 26);
  await page.getByLabel("Show assets:").selectOption("Extras");
  assert.equal(await page.locator("article:visible").count(), extrasCount);
  await page.getByLabel("Show assets:").selectOption("all");
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await writeFile(path.join(root, "png/gallery-validation.json"), JSON.stringify({
    passed: true, pngs: manifest.count, loadedImages: manifest.count * 2,
    filters: [manifest.count, 26, extrasCount, manifest.count], mobileOverflow: false,
  }, null, 2) + "\n");
  console.log(`PNG catalog: ${manifest.count * 2} previews loaded, filters passed, 390px viewport has no horizontal overflow.`);
} finally {
  await browser.close();
}
