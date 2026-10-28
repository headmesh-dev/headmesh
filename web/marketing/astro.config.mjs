// @ts-check

import { existsSync } from "node:fs";
import sitemap from "@astrojs/sitemap";
import starlight from "@astrojs/starlight";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, envField } from "astro/config";
import { REPO_URL, SENDER, SITE_URL } from "./shared/site.ts";

/** Cloudflare's "always passes" Turnstile test site key, used when no real key is configured. */
const TURNSTILE_TEST_SITE_KEY = "1x00000000000000000000AA";

// Pages that must never appear in the sitemap.
const UNLISTED = ["/github/", "/waitlist/confirm/", "/waitlist/unsubscribe/", "/404/"];

// Config-time settings come from the environment, or from web/marketing/.env for local builds (astro:env reads the
// same sources for the pages).
const dotenv = new URL(".env", import.meta.url);
if (existsSync(dotenv)) process.loadEnvFile(dotenv);
const env = process.env;

// The production deploy job sets HEADMESH_PRODUCTION=1. Refuse to build a production site that would collect
// sign-ups it can't verify or email without CASL sender identification.
if (env.HEADMESH_PRODUCTION === "1") {
  const problems = [];
  if (!env.PUBLIC_TURNSTILE_SITE_KEY || env.PUBLIC_TURNSTILE_SITE_KEY === TURNSTILE_TEST_SITE_KEY) {
    problems.push("PUBLIC_TURNSTILE_SITE_KEY must be the real Turnstile site key");
  }
  if (!SENDER.postalAddress) {
    problems.push("SENDER.postalAddress in shared/site.ts must be set (CASL sender identification)");
  }
  if (problems.length > 0) {
    throw new Error(`Production build preflight failed:\n- ${problems.join("\n- ")}`);
  }
}

/** Cloudflare Web Analytics beacon for Starlight pages (the marketing layout renders its own copy). */
const analyticsHead = env.PUBLIC_CF_ANALYTICS_TOKEN
  ? [
      {
        tag: /** @type {const} */ ("script"),
        attrs: {
          defer: true,
          src: "https://static.cloudflareinsights.com/beacon.min.js",
          "data-cf-beacon": JSON.stringify({ token: env.PUBLIC_CF_ANALYTICS_TOKEN }),
        },
      },
    ]
  : [];

export default defineConfig({
  site: SITE_URL,
  output: "static",
  build: {
    // Ship stylesheets as files so the CSP can stay `style-src 'self'` (see public/_headers).
    inlineStylesheets: "never",
  },
  redirects: {
    // nginx answers /github with a real 302 (deploy/nginx); this generated page is the fallback for
    // `astro preview`.
    "/github": REPO_URL,
  },
  env: {
    schema: {
      PUBLIC_TURNSTILE_SITE_KEY: envField.string({
        context: "client",
        access: "public",
        default: TURNSTILE_TEST_SITE_KEY,
      }),
      PUBLIC_CF_ANALYTICS_TOKEN: envField.string({ context: "client", access: "public", optional: true }),
    },
  },
  integrations: [
    starlight({
      title: "Headmesh Docs",
      description: "Documentation for Headmesh, the open, self-hostable, multi-tenant mesh built on Headscale.",
      logo: { src: "./src/assets/logo.svg" },
      favicon: "/favicon.svg",
      disable404Route: true,
      pagefind: false,
      social: [{ icon: "github", label: "GitHub", href: REPO_URL }],
      editLink: { baseUrl: `${REPO_URL}/edit/main/web/marketing/` },
      customCss: ["@fontsource-variable/inter", "@fontsource-variable/jetbrains-mono", "./src/styles/starlight.css"],
      head: [
        { tag: "meta", attrs: { property: "og:image", content: `${SITE_URL}/og/docs.png` } },
        { tag: "meta", attrs: { name: "twitter:card", content: "summary_large_image" } },
        ...analyticsHead,
      ],
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "Overview", slug: "docs" },
            { label: "Quickstart", slug: "docs/quickstart" },
          ],
        },
        { label: "Learn", items: [{ label: "Concepts", slug: "docs/concepts" }] },
        { label: "Operate", items: [{ label: "Self-hosting", slug: "docs/self-hosting" }] },
      ],
    }),
    sitemap({
      filter: (page) => !UNLISTED.includes(new URL(page).pathname),
    }),
  ],
  vite: {
    plugins: [tailwindcss()],
    // Never inline assets or bundled scripts as data: URIs or inline <script>s; the CSP only trusts files from
    // this origin plus the hashes of the few `is:inline` scripts (scripts/csp.mjs).
    build: { assetsInlineLimit: 0 },
  },
});
