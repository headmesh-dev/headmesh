import { WAITLIST_PROMPT } from "../../src/agents/markdown.ts";
import { cspViolations, expect, expectAccessible, test, waitForTurnstile } from "./fixtures";

const PAGES = [
  { path: "/", heading: "Share machines, not your network." },
  { path: "/waitlist/", heading: "Join the waitlist" },
  { path: "/waitlist/confirm/", heading: "Confirm your email" },
  { path: "/waitlist/unsubscribe/", heading: "Unsubscribe" },
  { path: "/privacy/", heading: "Privacy policy" },
  { path: "/terms/", heading: "Terms of use" },
  { path: "/docs/", heading: "Headmesh documentation" },
  { path: "/docs/quickstart/", heading: "Quickstart" },
  { path: "/docs/concepts/", heading: "Concepts" },
  { path: "/docs/self-hosting/", heading: "Self-hosting" },
  { path: "/this-page-does-not-exist/", heading: "This page isn't on the mesh." },
];

for (const { path, heading } of PAGES) {
  for (const colorScheme of ["light", "dark"] as const) {
    test(`${path} renders cleanly under the production CSP (${colorScheme})`, async ({ page, problems }) => {
      await page.emulateMedia({ colorScheme });
      const response = await page.goto(path);
      expect(response?.status()).toBe(path.startsWith("/this-page") ? 404 : 200);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(heading);
      await expect(page.locator("html")).toHaveAttribute("data-theme", colorScheme);

      // No horizontal scrolling at any viewport.
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      expect(overflow).toBeLessThanOrEqual(0);

      await expectAccessible(page);
      expect(await cspViolations(page)).toEqual([]);
      // The 404 page's own document request is the one expected console error.
      expect(problems.console.filter((line) => !line.includes("status of 404"))).toEqual([]);
    });
  }
}

test("home page carries the launch copy and both calls to action", async ({ page, problems }) => {
  await page.goto("/");
  await expect(page).toHaveTitle("Headmesh");
  await expect(page.getByText("Give teams, renters and AI agents exactly the machines they need")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Give your AI agents a real machine, not a toy sandbox." }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "Built for Xcelsior Compute" })).toBeVisible();
  await expect(page.getByRole("contentinfo")).toContainText("and WireGuard. An Xcelsior Compute project.");
  for (const feature of [
    "Invite people and devices",
    "Keep teams separate",
    "Temporary access",
    "Agent sandboxes",
    "Yours to run",
  ]) {
    await expect(page.getByRole("heading", { level: 3, name: feature })).toBeVisible();
  }
  const main = page.getByRole("main");
  await expect(main.getByRole("link", { name: "Join the waitlist" }).first()).toHaveAttribute("href", "/waitlist/");
  await expect(main.getByRole("link", { name: "Star on GitHub" }).first()).toHaveAttribute(
    "href",
    "https://github.com/headmesh-dev/headmesh",
  );
  await expect(page.locator('meta[property="og:image"]')).toHaveAttribute(
    "content",
    "https://headmesh.dev/og/home.png",
  );
  await expect(page.locator('link[rel="canonical"]')).toHaveAttribute("href", "https://headmesh.dev/");
  expect(problems.console).toEqual([]);
});

test("hero diagram explains each connection on hover and tap, without a native tooltip", async ({
  page,
  problems,
  isMobile,
}) => {
  await page.goto("/");
  const diagram = page.locator("[data-mesh]");
  const caption = diagram.locator(".mesh-caption.is-shown");
  await expect(diagram.getByRole("img", { name: /four-hour grant to one GPU/ })).toBeVisible();
  // An SVG <title> is what browsers show as a plain tooltip box.
  await expect(diagram.locator("svg title")).toHaveCount(0);
  await expect(caption).toHaveText("Hover or tap a connection to see who can reach what.");

  if (!isMobile) {
    await diagram.locator('.mesh-lane[data-lane="renter"]').hover();
    await expect(diagram).toHaveAttribute("data-active", "renter");
    await expect(caption).toContainText("reaches gpu-01 for four hours");
    await page.mouse.move(0, 0);
    await expect(diagram).not.toHaveAttribute("data-active");
  }

  // A tap or click pins the lane, so touch screens get the captions too, and a second one lets go.
  const agent = diagram.getByRole("button", { name: /AI agent/ });
  await agent.click();
  await expect(agent).toHaveAttribute("aria-pressed", "true");
  await expect(diagram).toHaveAttribute("data-active", "agent");
  await expect(caption).toContainText("reaches your NAS and nothing else");
  await agent.click();
  await expect(agent).toHaveAttribute("aria-pressed", "false");

  await diagram.locator('.mesh-lane[data-lane="desktop"] text').first().click();
  await expect(diagram).toHaveAttribute("data-active", "desktop");
  await expect(caption).toContainText("isn't shared with anyone");
  await expect(diagram.getByRole("button", { name: /Desktop/ })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("heading", { level: 1 }).click();
  await expect(diagram).not.toHaveAttribute("data-active");
  expect(problems.console).toEqual([]);
});

test("theme toggle switches theme and carries over to the docs", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");
  const html = page.locator("html");
  await expect(html).toHaveAttribute("data-theme", "light");

  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(html).toHaveAttribute("data-theme", "dark");
  await expect(page.getByRole("button", { name: "Switch to light theme" })).toBeVisible();

  await page.reload();
  await expect(html).toHaveAttribute("data-theme", "dark");
  await page.goto("/docs/");
  await expect(html).toHaveAttribute("data-theme", "dark");
});

test.describe("copy for LLM", () => {
  test.beforeEach(async ({ context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  });

  const clipboard = (page: import("@playwright/test").Page) => page.evaluate(() => navigator.clipboard.readText());

  test("copies the waitlist directions, with Markdown and Claude in the menu", async ({ page, problems }) => {
    await page.goto("/");
    const main = page.getByRole("main");
    await main.getByRole("button", { name: "Copy for LLM" }).click();
    await expect(main.getByRole("button", { name: "Copied" })).toBeVisible();
    expect(await clipboard(page)).toBe(WAITLIST_PROMPT);
    await expect(main.getByRole("button", { name: "Copy for LLM" })).toBeVisible();

    const toggle = main.getByRole("button", { name: "More ways to copy" });
    const menu = page.getByRole("menu", { name: "Copy for LLM" });
    await expect(menu).toBeHidden();
    await toggle.focus();
    await page.keyboard.press("ArrowDown");
    await expect(menu).toBeVisible();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect(menu.getByRole("menuitem")).toHaveText([/^Copy for LLM/, /^Copy as Markdown/, /^Open in Claude/]);
    await expect(menu.getByRole("menuitem").first()).toBeFocused();
    await expect(menu.getByRole("menuitem", { name: /Open in Claude/ })).toHaveAttribute(
      "href",
      `https://claude.ai/new?q=${encodeURIComponent(WAITLIST_PROMPT)}`,
    );
    await expectAccessible(page);

    await page.keyboard.press("ArrowDown");
    await expect(menu.getByRole("menuitem", { name: /Copy as Markdown/ })).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(menu).toBeHidden();
    await expect(toggle).toBeFocused();
    expect(await clipboard(page)).toBe(await (await page.request.get("/llms.txt")).text());

    await toggle.click();
    await expect(menu.getByRole("menuitem").first()).toBeFocused();
    await page.keyboard.press("ArrowUp");
    await expect(menu.getByRole("menuitem", { name: /Open in Claude/ })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(menu).toBeHidden();
    await toggle.click();
    await page.getByRole("heading", { level: 1 }).click();
    await expect(menu).toBeHidden();
    expect(await cspViolations(page)).toEqual([]);
    expect(problems.console).toEqual([]);
  });

  test("docs pages copy their own Markdown", async ({ page, problems }) => {
    await page.goto("/docs/quickstart/");
    await page.getByRole("button", { name: "Copy page" }).click();
    expect(await clipboard(page)).toBe(await (await page.request.get("/docs/quickstart.md")).text());
    await page.getByRole("button", { name: "More ways to copy" }).click();
    await expect(page.getByRole("menuitem", { name: /Open in Claude/ })).toHaveAttribute(
      "href",
      `https://claude.ai/new?q=${encodeURIComponent("Read https://headmesh.dev/docs/quickstart.md so I can ask you questions about it.")}`,
    );
    await page.getByRole("menuitem", { name: /Copy for LLM/ }).click();
    expect(await clipboard(page)).toBe(WAITLIST_PROMPT);
    expect(problems.console).toEqual([]);
  });

  test("an agent's link fills in the waitlist form and leaves the address bar clean", async ({ page, problems }) => {
    let sent: Record<string, unknown> | undefined;
    await page.route("**/api/waitlist", async (route) => {
      sent = route.request().postDataJSON();
      await route.fulfill({ status: 202, json: { ok: true } });
    });

    const email = encodeURIComponent("ada+hm@example.com");
    await page.goto(`/waitlist/?ref=agent#email=${email}&use_case=${encodeURIComponent("GPU sharing")}`);
    await expect(page.getByLabel("Email", { exact: true })).toHaveValue("ada+hm@example.com");
    await expect(page.getByLabel(/What would you use Headmesh for/)).toHaveValue("GPU sharing");
    await expect(page.getByRole("checkbox")).not.toBeChecked();
    await expect(page).toHaveURL(/\/waitlist\/\?ref=agent$/);

    await waitForTurnstile(page);
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Join the waitlist" }).click();
    await expect(page.getByRole("heading", { name: "Check your inbox" })).toBeVisible();
    expect(sent).toMatchObject({ email: "ada+hm@example.com", use_case: "GPU sharing", source: "agent" });
    expect(problems.console).toEqual([]);
  });
});

test.describe("waitlist form", () => {
  test("sends a well-formed sign-up and shows the next step", async ({ page, problems }) => {
    let sent: Record<string, unknown> | undefined;
    await page.route("**/api/waitlist", async (route) => {
      expect(await route.request().headerValue("content-type")).toBe("application/json");
      sent = route.request().postDataJSON();
      await route.fulfill({ status: 202, json: { ok: true } });
    });

    await page.goto("/waitlist/?utm_source=newsletter&utm_campaign=launch&ignored=1");
    await waitForTurnstile(page);
    await page.getByLabel("Email", { exact: true }).fill("  ada@example.com ");
    await page.getByLabel(/What would you use Headmesh for/).fill("GPU sharing");
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Join the waitlist" }).click();

    const success = page.getByRole("heading", { name: "Check your inbox" });
    await expect(success).toBeVisible();
    await expect(page.getByText("ada@example.com", { exact: true })).toBeVisible();
    expect(sent).toEqual({
      email: "ada@example.com",
      use_case: "GPU sharing",
      consent: true,
      consent_version: expect.any(String),
      turnstile_token: "test-turnstile-token",
      source: "newsletter",
      utm: { utm_source: "newsletter", utm_campaign: "launch" },
    });
    expect(await cspViolations(page)).toEqual([]);
    expect(problems.console).toEqual([]);
  });

  test("won't submit without consent", async ({ page }) => {
    let requests = 0;
    await page.route("**/api/waitlist", (route) => {
      requests++;
      return route.fulfill({ status: 202, json: { ok: true } });
    });
    await page.goto("/waitlist/");
    await page.getByLabel("Email", { exact: true }).fill("ada@example.com");
    await page.getByRole("button", { name: "Join the waitlist" }).click();
    await expect(page.getByRole("checkbox")).toHaveJSProperty("validity.valid", false);
    expect(requests).toBe(0);
  });

  test("waits for the spam check instead of sending an incomplete request", async ({ page }) => {
    let requests = 0;
    await page.route("https://challenges.cloudflare.com/**", (route) => route.abort());
    await page.route("**/api/waitlist", (route) => {
      requests++;
      return route.fulfill({ status: 202, json: { ok: true } });
    });
    await page.goto("/waitlist/");
    await page.getByLabel("Email", { exact: true }).fill("ada@example.com");
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Join the waitlist" }).click();
    await expect(page.locator("#waitlist-form").getByRole("status")).toHaveText(/spam check is still loading/);
    expect(requests).toBe(0);
  });

  test("shows server-side field errors and a fresh spam check", async ({ page }) => {
    await page.route("**/api/waitlist", (route) =>
      route.fulfill({
        status: 400,
        json: { ok: false, error: "invalid_request", fields: { email: "Enter a valid email address." } },
      }),
    );
    await page.goto("/waitlist/");
    await waitForTurnstile(page);
    await page.getByLabel("Email", { exact: true }).fill("ada@example.com");
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Join the waitlist" }).click();

    await expect(page.getByText("Enter a valid email address.")).toBeVisible();
    await expect(page.getByLabel("Email", { exact: true })).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#waitlist-form").getByRole("status")).toHaveText("Please fix the highlighted fields.");
    expect(await page.evaluate(() => (window as unknown as { __turnstileResets?: number }).__turnstileResets)).toBe(1);
  });
});

test.describe("email link pages", () => {
  test("confirm page posts the token from the fragment only when clicked", async ({ page }) => {
    let body: unknown;
    await page.route("**/api/waitlist/confirm", async (route) => {
      body = route.request().postDataJSON();
      await route.fulfill({ json: { ok: true } });
    });
    await page.goto("/waitlist/confirm/#token=v1.abc.123.sig");
    // The token leaves the address bar straight away.
    await expect(page).toHaveURL(/\/waitlist\/confirm\/$/);
    expect(body).toBeUndefined();

    await page.getByRole("button", { name: "Confirm my email" }).click();
    await expect(page.getByRole("status")).toContainText("You're on the list.");
    expect(body).toEqual({ token: "v1.abc.123.sig" });
  });

  test("confirm page explains an expired link", async ({ page }) => {
    await page.route("**/api/waitlist/confirm", (route) =>
      route.fulfill({ status: 400, json: { ok: false, error: "invalid_token" } }),
    );
    await page.goto("/waitlist/confirm/#token=old");
    await page.getByRole("button", { name: "Confirm my email" }).click();
    await expect(page.getByRole("status")).toContainText("invalid or has expired");
  });

  test("unsubscribe page says what to do without a token", async ({ page }) => {
    await page.goto("/waitlist/unsubscribe/");
    await expect(page.getByRole("button", { name: "Unsubscribe" })).toBeDisabled();
    await expect(page.getByRole("status")).toContainText("hello@headmesh.dev");
  });

  test("unsubscribe page posts the token when clicked", async ({ page }) => {
    await page.route("**/api/waitlist/unsubscribe", (route) => route.fulfill({ json: { ok: true } }));
    await page.goto("/waitlist/unsubscribe/#token=v1.abc.sig");
    await page.getByRole("button", { name: "Unsubscribe" }).click();
    await expect(page.getByRole("status")).toContainText("You're unsubscribed.");
  });
});

test.describe("site files", () => {
  test("security.txt follows RFC 9116", async ({ request }) => {
    const response = await request.get("/.well-known/security.txt");
    expect(response.status()).toBe(200);
    const text = await response.text();
    expect(text).toContain("Contact: mailto:security@headmesh.dev");
    expect(text).toContain("Canonical: https://headmesh.dev/.well-known/security.txt");
    const expires = Date.parse(/^Expires: (.+)$/m.exec(text)?.[1] ?? "");
    expect(expires).toBeGreaterThan(Date.now());
    expect(expires).toBeLessThan(Date.now() + 365 * 86_400_000);
  });

  test("sitemap lists public pages only", async ({ request }) => {
    expect((await request.get("/sitemap-index.xml")).status()).toBe(200);
    const sitemap = await (await request.get("/sitemap-0.xml")).text();
    for (const path of ["/", "/waitlist/", "/docs/", "/privacy/", "/terms/"]) {
      expect(sitemap).toContain(`<loc>https://headmesh.dev${path}</loc>`);
    }
    for (const path of ["/github/", "/waitlist/confirm/", "/waitlist/unsubscribe/"]) {
      expect(sitemap).not.toContain(`https://headmesh.dev${path}<`);
    }
    expect(await (await request.get("/robots.txt")).text()).toContain(
      "Sitemap: https://headmesh.dev/sitemap-index.xml",
    );
  });

  test("OG images are 1200×630 PNGs", async ({ request }) => {
    for (const slug of ["home", "waitlist", "docs", "privacy", "terms"]) {
      const response = await request.get(`/og/${slug}.png`);
      expect(response.status()).toBe(200);
      const png = await response.body();
      expect(png.subarray(1, 4).toString()).toBe("PNG");
      expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([1200, 630]);
    }
  });

  test("/github points at the repository", async ({ request }) => {
    const html = await (await request.get("/github/")).text();
    expect(html).toContain('content="0;url=https://github.com/headmesh-dev/headmesh"');
  });

  test("internal links all resolve", async ({ page, request }) => {
    const seen = new Set<string>();
    for (const path of PAGES.map((p) => p.path).filter((p) => !p.startsWith("/this-page"))) {
      await page.goto(path);
      const hrefs = await page.$$eval("a[href^='/']", (links) => links.map((a) => a.getAttribute("href") ?? ""));
      for (const href of hrefs) seen.add(href.split("#")[0] ?? href);
    }
    for (const href of seen) {
      const response = await request.get(href, { maxRedirects: 0 });
      expect(response.status(), href).toBe(200);
    }
  });
});
