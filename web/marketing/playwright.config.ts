import { defineConfig, devices } from "@playwright/test";

const PORT = 4321;
const externalBaseUrl = process.env.PLAYWRIGHT_BASE_URL;
const CI = Boolean(process.env.CI);

export default defineConfig({
  testDir: "tests/e2e",
  fullyParallel: true,
  forbidOnly: CI,
  retries: CI ? 1 : 0,
  reporter: CI ? [["github"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: externalBaseUrl ?? `http://localhost:${PORT}`,
    trace: "retain-on-failure",
  },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"] } },
    { name: "mobile", use: { ...devices["Pixel 7"] } },
  ],
  // Smoke tests run against the built site (`pnpm build` first). Set PLAYWRIGHT_BASE_URL to test a deployment.
  ...(externalBaseUrl
    ? {}
    : {
        webServer: {
          // --ignore-lock: a one-off foreground server owned by Playwright (Astro would otherwise background
          // itself when it detects an agent, and Playwright would think it exited).
          command: `pnpm exec astro preview --port ${PORT} --ignore-lock`,
          url: `http://localhost:${PORT}`,
          reuseExistingServer: !CI,
        },
      }),
});
