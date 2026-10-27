import { buildPagesASSETSBinding, cloudflareTest, readD1Migrations } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

export default defineConfig(async () => {
  const migrations = await readD1Migrations("./migrations");
  // Pages Functions receive an ASSETS binding; serve the static public/ folder so tests don't need a build.
  const assets = await buildPagesASSETSBinding(new URL("./public", import.meta.url).pathname);

  return {
    test: {
      projects: [
        {
          // Pages Functions and their modules run inside workerd with a real (local, per-test isolated) D1.
          plugins: [
            cloudflareTest({
              wrangler: { configPath: "./wrangler.toml" },
              miniflare: {
                serviceBindings: { ASSETS: assets },
                bindings: {
                  TEST_MIGRATIONS: migrations,
                  TURNSTILE_SECRET_KEY: "test-turnstile-secret",
                  RESEND_API_KEY: "test-resend-key",
                  WAITLIST_SECRET: "test-waitlist-secret-that-is-long-enough-0123456789",
                },
              },
            }),
          ],
          test: {
            name: "functions",
            include: ["tests/functions/**/*.test.ts"],
            setupFiles: ["tests/functions/setup.ts"],
          },
        },
        {
          test: {
            name: "scripts",
            include: ["tests/scripts/**/*.test.ts"],
            environment: "node",
          },
        },
      ],
    },
  };
});
