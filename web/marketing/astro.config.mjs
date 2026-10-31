// @ts-check

import { existsSync } from "node:fs";
import sitemap from "@astrojs/sitemap";
import starlight from "@astrojs/starlight";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, envField, fontProviders } from "astro/config";
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
  // Self-hosted from the Fontsource packages (no CDN at build or run time). Astro writes the @font-face rules, preloads
  // what <FontFaces> asks for, and adds a fallback face whose metrics match each font, so swapping in the web font
  // doesn't shift the layout (it cost up to 0.15 CLS on slower machines).
  fonts: [
    {
      provider: fontProviders.local(),
      name: "Geist",
      cssVariable: "--font-geist",
      fallbacks: ["sans-serif"],
      options: {
        variants: [
          {
            src: ["@fontsource-variable/geist/files/geist-latin-wght-normal.woff2"],
            weight: "100 900",
            style: "normal",
          },
        ],
      },
    },
    {
      provider: fontProviders.local(),
      name: "Geist Mono",
      cssVariable: "--font-geist-mono",
      fallbacks: ["monospace"],
      options: {
        variants: [
          {
            src: ["@fontsource-variable/geist-mono/files/geist-mono-latin-wght-normal.woff2"],
            weight: "100 900",
            style: "normal",
          },
        ],
      },
    },
    {
      provider: fontProviders.local(),
      name: "Bricolage Grotesque",
      cssVariable: "--font-bricolage",
      fallbacks: ["sans-serif"],
      options: {
        variants: [
          {
            src: ["@fontsource-variable/bricolage-grotesque/files/bricolage-grotesque-latin-standard-normal.woff2"],
            weight: "200 800",
            style: "normal",
          },
        ],
      },
    },
  ],
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
      logo: { dark: "./src/assets/logo.svg", light: "./src/assets/logo-on-light.svg", alt: "Headmesh" },
      components: { Head: "./src/components/StarlightHead.astro", SiteTitle: "./src/components/DocsSiteTitle.astro" },
      favicon: "/favicon.svg?v=transparent-1",
      disable404Route: true,
      pagefind: false,
      social: [{ icon: "github", label: "GitHub", href: REPO_URL }],
      editLink: { baseUrl: `${REPO_URL}/edit/main/web/marketing/` },
      customCss: ["./src/styles/starlight.css"],
      expressiveCode: {
        styleOverrides: {
          codeFontFamily: "var(--hm-font-mono)",
          uiFontFamily: "var(--hm-font-body)",
          codeBackground: "var(--hm-surface)",
          codeForeground: "var(--hm-fg)",
          borderColor: "var(--hm-line)",
          borderRadius: "0.75rem",
          frames: {
            editorBackground: "var(--hm-surface)",
            terminalBackground: "var(--hm-surface)",
            editorActiveTabBackground: "var(--hm-surface)",
            frameBoxShadowCssValue: "none",
          },
        },
      },
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
