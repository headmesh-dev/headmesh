# headmesh.dev

The public marketing site, docs and waitlist for Headmesh (plan §12.1). It's a static Astro site with Starlight
docs at `/docs`, plus three Cloudflare Pages Functions for the waitlist. It deploys to Cloudflare Pages on its
own, so marketing changes never touch the product.

| Path                                   | What it is                                                                                                                                                                          |
| -------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `src/pages/`                           | `/`, `/waitlist/` (plus `confirm/` and `unsubscribe/`), `/privacy/`, `/terms/`, the 404 page, OG images (`og/[slug].png.ts`), `apple-touch-icon.png` and `.well-known/security.txt` |
| `src/content/docs/docs/`               | Starlight pages served at `/docs/` (placeholders that link to the repository docs)                                                                                                  |
| `functions/api/`                       | Pages Functions: `POST /api/waitlist`, `POST /api/waitlist/confirm`, `POST /api/waitlist/unsubscribe`                                                                               |
| `server/`                              | The Functions' logic: validation (zod), Turnstile, tokens, email, D1 queries                                                                                                        |
| `shared/`                              | Facts both sides use: URLs, CASL sender identity, consent wording and version, field limits                                                                                         |
| `migrations/`                          | D1 schema migrations                                                                                                                                                                |
| `public/_headers`, `public/_redirects` | Security headers (CSP, HSTS, Referrer-Policy, Permissions-Policy) and the `/github` redirect                                                                                        |
| `scripts/csp.mjs`                      | Post-build step that writes the SHA-256 hashes of every inline script into the CSP in `dist/_headers`                                                                               |
| `tests/functions/`, `tests/scripts/`   | Vitest: Functions inside workerd against a real local D1, and the CSP script                                                                                                        |
| `tests/e2e/`                           | Playwright + axe smoke tests against `astro preview`, run under the production headers                                                                                              |

Stack: Astro 7.4, Starlight 0.42, Tailwind CSS 4.3, Fontsource (Inter, JetBrains Mono), zod 4, Wrangler 4, Biome 2,
Vitest 5 with `@cloudflare/vitest-plugin`, Playwright + axe and Lighthouse CI.

## Develop

Node and pnpm come from the repository's `mise.toml` (`mise install`).

```sh
pnpm install          # from the repo root once pnpm-workspace.yaml lists web/marketing; until then, here
pnpm dev              # http://localhost:4321 (pages only, no Functions)
pnpm build            # astro build + CSP hashing into dist/_headers
pnpm preview          # serve dist/
```

To run the waitlist end to end on your machine (Functions, local D1, Turnstile test keys, email links in the log):

```sh
cp .dev.vars.example .dev.vars          # then set WAITLIST_SECRET (openssl rand -base64 48)
pnpm db:migrate:local
pnpm build && pnpm pages:dev            # wrangler pages dev serves dist/ + functions/ with _headers applied
```

| Script              | What it does                                                                                                               |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `pnpm lint`         | Biome (format + lint), then `astro check` and `tsc` for the Functions                                                      |
| `pnpm format`       | Biome with fixes applied                                                                                                   |
| `pnpm test`         | Vitest: `functions` project in workerd (real D1, per-test reset), `scripts` project in Node                                |
| `pnpm test:e2e`     | Playwright + axe on desktop and mobile, both colour schemes, under the CSP from `dist/_headers` (needs `pnpm build` first) |
| `pnpm lhci`         | Lighthouse CI on five pages, 3 runs each, ≥ 95 in every category (needs `pnpm build`)                                      |
| `pnpm types:worker` | Regenerate `functions/worker-configuration.d.ts` after changing `wrangler.toml`                                            |

The e2e tests stub Turnstile and block every third-party request, so they never depend on the network. Set
`PLAYWRIGHT_BASE_URL` to run them against a deployment instead of `astro preview`.

## How the waitlist works

1. The form posts JSON to `/api/waitlist`: email, optional use case (≤ 500 characters), the consent checkbox, the
   consent wording version, the Turnstile token, and the source (`?ref=`, `utm_source` or the referring host) and
   `utm_*` parameters.
2. The Function validates with zod, normalises the email (trim, NFC, lowercase, punycode domain; no Gmail-style
   rewriting), verifies Turnstile server-side (action must be `waitlist`) and limits each IP to 10 sign-ups an
   hour, keyed on an HMAC of the IP. Raw IPs are never stored.
3. It upserts the row and sends the double opt-in email through Resend. Repeat sign-ups get the same `202`
   whether or not the address is known, and the confirmation is re-sent at most every 10 minutes. A person who
   unsubscribed and signs up again gives fresh consent and has to confirm again.
4. Email links point at `/waitlist/confirm/#token=…` and `/waitlist/unsubscribe/#token=…`. The token sits in the
   fragment, so it never reaches server logs or Referer headers, and the page only posts it when the person
   clicks. Link scanners that prefetch URLs can't confirm or unsubscribe anyone. The email also carries
   RFC 8058 one-click `List-Unsubscribe` headers that point at `POST /api/waitlist/unsubscribe?token=…`.
5. Tokens are `v1.<row id>[.<expiry>].<HMAC-SHA256>`, with a separate HKDF-derived key per purpose. Confirm
   links expire after 7 days. Unsubscribe links never expire, because CASL requires them to work for at least 60
   days. Tokens contain the row id, never the email.

**CASL.** The consent checkbox is unticked by default and states the purpose. The form, every email and the privacy
policy identify the sender (Xcelsior Compute Inc., mailing address, contact email). Every email can be unsubscribed
in one click, and unsubscribes take effect immediately. Each row records which consent wording was shown
(`consent_version`) and when consent was given, confirmed and withdrawn. When you change `CONSENT_TEXT` in
`shared/site.ts`, bump `CONSENT_VERSION` too.

**Fail-closed.** Until `SENDER.postalAddress` in `shared/site.ts` is filled in, production builds refuse to run
and the Function refuses sign-ups with `503` instead of sending email that isn't CASL-compliant.

The table (`migrations/0001_create_waitlist.sql`) is `waitlist(id, email UNIQUE, use_case, source, utm,
consent_version, ip_hash, created_at, confirmed_at, unsubscribed_at, confirm_sent_at)`. `confirm_sent_at` is one
column beyond the plan; it throttles confirmation re-sends. The mailable audience is
`confirmed_at IS NOT NULL AND unsubscribed_at IS NULL`.

## Configuration

Nothing secret lives in the repository. `wrangler.toml` is the source of truth for the Pages project's bindings and
plain variables. Secrets are set per environment.

**Build time** (GitHub Actions environment, or `web/marketing/.env` locally):

| Variable                    | Where it comes from                                                                                                                        |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `PUBLIC_TURNSTILE_SITE_KEY` | Repository variable `TURNSTILE_SITE_KEY` for production. Without it, the build uses Cloudflare's always-pass test key (local and preview). |
| `PUBLIC_CF_ANALYTICS_TOKEN` | Repository variable `CF_WEB_ANALYTICS_TOKEN`. Without it, no beacon is rendered.                                                           |
| `HEADMESH_PRODUCTION=1`     | Set by the production deploy job. Fails the build if the Turnstile key is the test key or the postal address is empty.                     |

**Runtime vars** (`wrangler.toml`): `SITE_URL`, `EMAIL_FROM`, `EMAIL_REPLY_TO` and `EMAIL_DELIVERY`. Production uses
`resend`. `[env.preview]` uses `log`, so previews never send mail and their links go to
`wrangler pages deployment tail`.

**Runtime secrets** (Pages project, per environment):

| Secret                 | Production                                                                                   | Preview                                                                                                                  |
| ---------------------- | -------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `TURNSTILE_SECRET_KEY` | The widget's secret key                                                                      | Cloudflare's test secret `1x0000000000000000000000000000000AA` (it pairs with the test site key previews are built with) |
| `WAITLIST_SECRET`      | `openssl rand -base64 48`; rotating it invalidates outstanding confirm and unsubscribe links | A different random value                                                                                                 |
| `RESEND_API_KEY`       | A Resend key with sending access to `mail.headmesh.dev` only                                 | Not needed (`EMAIL_DELIVERY=log`)                                                                                        |

**GitHub** (repository settings): secrets `CLOUDFLARE_API_TOKEN` (an account API token with _Cloudflare Pages:
Edit_ and _D1: Edit_ only) and `CLOUDFLARE_ACCOUNT_ID`, plus variables `TURNSTILE_SITE_KEY` and
`CF_WEB_ANALYTICS_TOKEN`. Create the environments `marketing-production` (deployment branch: `main` only) and
`marketing-preview`. The deploy jobs are skipped until the repository variable `MARKETING_DEPLOY` is `enabled`, so
CI only lints, tests and builds until setup is finished.

## One-time setup

These steps create Cloudflare and Resend resources and need Aaryn's go-ahead. Run the `wrangler` commands from
`web/marketing/` after `pnpm exec wrangler login`.

1. **D1 databases**

   ```sh
   pnpm exec wrangler d1 create headmesh-waitlist
   pnpm exec wrangler d1 create headmesh-waitlist-preview
   ```

   Put the two printed `database_id`s into `wrangler.toml` (they're zeros now), then apply the schema with
   `pnpm exec wrangler d1 migrations apply DB --remote` and the same command with `--env preview`. CI re-applies
   pending migrations before every deploy.
2. **Pages project**

   ```sh
   pnpm exec wrangler pages project create headmesh-marketing --production-branch main
   ```

   In the dashboard, add the custom domain `headmesh.dev` (DNS is already on Cloudflare). Don't turn on automatic
   Web Analytics injection; the site renders its own beacon so the CSP stays exact.
3. **Turnstile.** In the dashboard, add a Turnstile widget (Managed) for hostname `headmesh.dev`. Save the site key
   as the GitHub variable `TURNSTILE_SITE_KEY`, and the secret with
   `pnpm exec wrangler pages secret put TURNSTILE_SECRET_KEY --project-name headmesh-marketing`.
4. **Resend.** Add the domain `mail.headmesh.dev`. Create the SPF, DKIM and bounce (MX) records it lists in
   Cloudflare DNS and wait for verification. Publish a DMARC record for `headmesh.dev` if there isn't one. Create
   an API key restricted to sending from that domain and store it with `wrangler pages secret put RESEND_API_KEY`.
5. **Signing secret.** Run `openssl rand -base64 48 | pnpm exec wrangler pages secret put WAITLIST_SECRET
   --project-name headmesh-marketing`.
6. **Preview secrets.** `wrangler pages secret put` only targets production, so set the preview values from the
   table above under _Settings → Variables and Secrets → Preview_ in the dashboard.
7. **Web Analytics.** Add the site `headmesh.dev` under _Web Analytics_ with the manual JS snippet. Save its token
   as the GitHub variable `CF_WEB_ANALYTICS_TOKEN`.
8. **Rate limiting.** Pages Functions can't use the Workers rate-limit binding the plan names: Wrangler's Pages
   config doesn't accept `ratelimits`. The Function enforces its own per-IP limit in D1. As an outer layer, add a WAF
   rate-limiting rule for `POST /api/waitlist*` on the `headmesh.dev` zone, for example 20 requests per 10 minutes
   per IP.
9. **CASL sender address.** Fill in `SENDER.postalAddress` in `shared/site.ts`. Production builds fail without it.
10. **Turn deploys on.** Once steps 1–9 are done, set the repository variable `MARKETING_DEPLOY` to `enabled`.
11. **Before launch:** have the privacy policy and terms reviewed (they have no governing-law clause yet), and make
    the GitHub repository public so the `/github` and "Star on GitHub" links work.

## Operations

There's no admin route; the waitlist is read with `wrangler d1 execute`. If an admin page is ever added, put it
behind Cloudflare Access.

```sh
# Sign-up count, the success metric
pnpm exec wrangler d1 execute DB --remote --command \
  "SELECT count(*) AS confirmed FROM waitlist WHERE confirmed_at IS NOT NULL AND unsubscribed_at IS NULL"

# Export the mailable list
pnpm exec wrangler d1 execute DB --remote --json --command \
  "SELECT email, use_case, source, utm, created_at, confirmed_at FROM waitlist
   WHERE confirmed_at IS NOT NULL AND unsubscribed_at IS NULL ORDER BY created_at"

# Delete sign-ups that were never confirmed (the privacy policy promises this; run it monthly)
pnpm exec wrangler d1 execute DB --remote --command \
  "DELETE FROM waitlist WHERE confirmed_at IS NULL AND created_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-30 days')"

# Live Function logs (structured JSON, never email addresses)
pnpm exec wrangler pages deployment tail --project-name headmesh-marketing
```

`/.well-known/security.txt` gets an `Expires` 330 days after each build, so redeploy at least once every ten
months.

## Security headers

`public/_headers` sets a CSP that allows only this origin, the SHA-256 hashes of the few inline scripts (theme
detection and Starlight's own), Turnstile (`challenges.cloudflare.com`) and the Web Analytics beacon
(`static.cloudflareinsights.com`, `cloudflareinsights.com`). Stylesheets and bundled scripts ship as files
(`inlineStylesheets: "never"`, `assetsInlineLimit: 0`), so `style-src` is `'self'` only. Inline style
_attributes_ are allowed through `style-src-attr` because Starlight sets CSS custom properties that way; they
can't load resources or run code. The build fails if an inline event handler appears or the header grows past
Pages' 2,000-character limit. The Functions set their own `no-store`, `nosniff` and `default-src 'none'` headers.
