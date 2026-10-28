import { expect, test } from "@playwright/test";

// What search engines and AI agents read: llms.txt, the Markdown copies, robots.txt and the JSON-LD.

test("llms.txt and llms-full.txt are served as text and link the Markdown docs", async ({ request }) => {
  const index = await request.get("/llms.txt");
  expect(index.ok()).toBe(true);
  const text = await index.text();
  expect(text.startsWith("# Headmesh\n")).toBe(true);
  expect(text).toContain("https://headmesh.dev/docs/quickstart.md");

  const full = await request.get("/llms-full.txt");
  expect(full.ok()).toBe(true);
  expect(await full.text()).toContain("\n## Quickstart\n");
});

test("every docs page has a Markdown copy it links to", async ({ page, request }) => {
  await page.goto("/docs/quickstart/");
  const href = await page.locator('link[rel="alternate"][type="text/markdown"]').getAttribute("href");
  expect(href).toBe("/docs/quickstart.md");
  const markdown = await request.get(href ?? "");
  expect(markdown.ok()).toBe(true);
  expect(await markdown.text()).toMatch(/^# Quickstart\n/);
});

test("robots.txt welcomes AI agents and points at the sitemap", async ({ request }) => {
  const robots = await (await request.get("/robots.txt")).text();
  expect(robots).toContain("User-agent: ClaudeBot");
  expect(robots).toContain("Sitemap: https://headmesh.dev/sitemap-index.xml");
});

test("the home page describes the software in valid JSON-LD", async ({ page }) => {
  await page.goto("/");
  const blocks = await page.locator('script[type="application/ld+json"]').allTextContents();
  expect(blocks).toHaveLength(1);
  const types = JSON.parse(blocks[0] ?? "{}")["@graph"].map((node: { "@type": string }) => node["@type"]);
  expect(types).toEqual(["Organization", "WebSite", "SoftwareApplication", "SoftwareSourceCode"]);
});
