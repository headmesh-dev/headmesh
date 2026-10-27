import { readFileSync } from "node:fs";
import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page } from "@playwright/test";

/**
 * The response headers Cloudflare Pages would send for every page, read from the built dist/_headers. `astro
 * preview` doesn't apply _headers, so the fixture adds them to every same-origin response; that way the tests run
 * under the real CSP and catch anything it would block. upgrade-insecure-requests is dropped because the preview
 * server is plain HTTP.
 */
function productionHeaders(): Record<string, string> {
  const file = readFileSync(new URL("../../dist/_headers", import.meta.url), "utf8");
  const block = file.split(/\n(?=\S)/).find((section) => section.startsWith("/*\n"));
  if (!block) throw new Error("dist/_headers has no /* rule; run `pnpm build` first");
  const headers: Record<string, string> = {};
  for (const line of block.split("\n").slice(1)) {
    const match = /^\s+([\w-]+):\s*(.+)$/.exec(line);
    if (match?.[1] && match[2]) headers[match[1].toLowerCase()] = match[2];
  }
  headers["content-security-policy"] = (headers["content-security-policy"] ?? "").replace(
    /;\s*upgrade-insecure-requests/,
    "",
  );
  return headers;
}

const HEADERS = productionHeaders();

/** Minimal stand-in for Turnstile's implicit rendering, so tests never depend on Cloudflare's network. */
const FAKE_TURNSTILE = `
  window.turnstile = { reset() { window.__turnstileResets = (window.__turnstileResets || 0) + 1; } };
  for (const el of document.querySelectorAll(".cf-turnstile")) {
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = "cf-turnstile-response";
    input.value = "test-turnstile-token";
    el.append(input);
  }
`;

export interface PageProblems {
  /** Console errors and uncaught exceptions. */
  console: string[];
}

export const test = base.extend<{ problems: PageProblems }>({
  // Automatic: every test gets the production headers, the Turnstile stub and no third-party traffic, whether or
  // not it inspects the problems it collects.
  problems: [
    async ({ page, baseURL }, use) => {
      const origin = new URL(baseURL ?? "http://localhost").origin;
      const problems: PageProblems = { console: [] };

      await page.route("**/*", async (route) => {
        const url = new URL(route.request().url());
        if (url.href === "https://challenges.cloudflare.com/turnstile/v0/api.js") {
          return route.fulfill({ contentType: "text/javascript", body: FAKE_TURNSTILE });
        }
        if (url.origin !== origin) return route.abort();
        const response = await route.fetch();
        return route.fulfill({ response, headers: { ...response.headers(), ...HEADERS } });
      });
      await page.addInitScript(() => {
        document.addEventListener("securitypolicyviolation", (event) => {
          (window as unknown as { __csp: string[] }).__csp ??= [];
          (window as unknown as { __csp: string[] }).__csp.push(`${event.violatedDirective} ${event.blockedURI}`);
        });
      });
      page.on("console", (message) => {
        if (message.type() === "error") problems.console.push(message.text());
      });
      page.on("pageerror", (error) => problems.console.push(error.message));

      await use(problems);
    },
    { auto: true },
  ],
});

/** Wait until the (fake) Turnstile widget has issued its token, as a person would before submitting. */
export async function waitForTurnstile(page: Page): Promise<void> {
  await expect(page.locator('input[name="cf-turnstile-response"]')).toHaveCount(1);
}

export async function cspViolations(page: Page): Promise<string[]> {
  return page.evaluate(() => (window as unknown as { __csp?: string[] }).__csp ?? []);
}

export async function expectAccessible(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"])
    .analyze();
  const summary = results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`);
  expect(summary).toEqual([]);
}

export { expect };
