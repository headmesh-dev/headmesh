import assert from "node:assert/strict";
import { access, readFile, writeFile } from "node:fs/promises";
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
  const logoManifest = JSON.parse(await readFile(path.join(root, "source-extras/selected-brand/logos/manifest.json"), "utf8"));
  assert.equal(logoManifest.count, 40);
  const logoGalleries = [
    path.join(root, "png/selected-brand/logos/index.html"),
  ];
  if (process.argv.includes("--site")) {
    logoGalleries.push(path.resolve(root, "../../public/brand/headmesh-v1/logos/index.html"));
  }
  const logoChecks = [];
  for (const filename of logoGalleries) {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(pathToFileURL(filename).href);
    await page.locator("img").evaluateAll(images => Promise.all(images.map(image => image.decode())));
    assert.equal(await page.locator("article:visible").count(), logoManifest.count);
    assert.equal(await page.locator("article a[download]").count(), logoManifest.count * 2);
    for (const href of await page.locator("article a[download]").evaluateAll(links => links.map(link => link.getAttribute("href")))) {
      await access(path.resolve(path.dirname(filename), href));
    }
    for (const layout of ["mark", "wordmark", "horizontal", "vertical"]) {
      await page.getByLabel("Layout", { exact: true }).selectOption(layout);
      assert.equal(await page.locator("article:visible").count(), 10);
    }
    await page.getByLabel("Logo colour", { exact: true }).selectOption("gradient");
    assert.equal(await page.locator("article:visible").count(), 3);
    await page.getByLabel("Background", { exact: true }).selectOption("white");
    assert.equal(await page.locator("article:visible").count(), 1);
    assert.equal(await page.locator("#count").textContent(), "1 variant");
    for (const label of ["Layout", "Logo colour", "Background"]) {
      await page.getByLabel(label, { exact: true }).selectOption("all");
    }
    assert.equal(await page.locator("article:visible").count(), logoManifest.count);
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    logoChecks.push({ gallery: path.relative(root, filename), images: logoManifest.count,
      downloadLinks: logoManifest.count * 2, layoutCounts: [10, 10, 10, 10], combinedFilters: [3, 1], mobileOverflow: false });
  }
  await page.goto(pathToFileURL(path.join(root, "png/selected-brand/icons/index.html")).href);
  await page.locator("img").evaluateAll(images => Promise.all(images.map(image => image.decode())));
  assert.equal(await page.locator("article").count(), 9);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  for (const href of await page.locator("article a[download]").evaluateAll(links => links.map(link => link.getAttribute("href")))) {
    await access(path.resolve(root, "png/selected-brand/icons", href));
  }
  await writeFile(path.join(root, "png/gallery-validation.json"), JSON.stringify({
    passed: true, pngs: manifest.count, loadedImages: manifest.count * 2,
    filters: [manifest.count, 26, extrasCount, manifest.count], mobileOverflow: false,
    logoGalleries: logoChecks, iconShapes: { images: 9, downloadLinks: 18, mobileOverflow: false },
  }, null, 2) + "\n");
  console.log(`PNG catalog: ${manifest.count * 2} previews loaded, filters passed, 390px viewport has no horizontal overflow.`);
  console.log(`Logo galleries: ${logoChecks.length} sets of 40 previews and 80 downloads passed; layout/colour/background filters and mobile checks passed. All 9 icon previews loaded.`);
} finally {
  await browser.close();
}
