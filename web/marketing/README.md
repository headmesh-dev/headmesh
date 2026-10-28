# headmesh.dev

The public marketing site, docs and waitlist for Headmesh (plan §12.1): a static Astro site with Starlight docs at
`/docs`, and a small Node service for the waitlist. Both run on hm-prod-1 behind nginx and deploy with
`deploy/deploy.sh --target frontend`, separately from the product, so marketing changes never touch it.

| Path                              | What it is                                                                                                                                                                          |
| --------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `src/pages/`                      | `/`, `/waitlist/` (plus `confirm/` and `unsubscribe/`), `/privacy/`, `/terms/`, the 404 page, OG images (`og/[slug].png.ts`), `apple-touch-icon.png` and `.well-known/security.txt` |
| `src/content/docs/docs/`          | Starlight pages served at `/docs/` (placeholders that link to the repository docs)                                                                                                  |
| `server/`                         | The waitlist service: `POST /api/waitlist`, `/confirm`, `/unsubscribe`, `/healthz`, `/readyz` on `node:http`, with zod, Turnstile, tokens, email and SQLite (`node:sqlite`)         |
| `shared/`                         | Facts the site and the service share: URLs, CASL sender identity, consent wording and version, the API contract                                                                     |
| `migrations/`                     | SQLite schema migrations, applied by the service when it starts                                                                                                                     |
| `nginx/security-headers.conf`     | Template for the CSP and other security headers nginx sends                                                                                                                         |
| `scripts/csp.mjs`                 | Post-build step that hashes every inline script and style into `build/nginx/security-headers.conf`                                                                                  |
| `tests/server/`, `tests/scripts/` | Vitest: the service against in-memory and on-disk SQLite (including a real socket), and the CSP script                                                                              |
| `tests/e2e/`                      | Playwright + axe smoke tests against `astro preview`, run under the production headers                                                                                              |

Stack: Astro 7.4, Starlight 0.42, Tailwind CSS 4.3, Fontsource (Inter, JetBrains Mono), zod 4,
Node 24 (`node:http`, `node:sqlite`, native TypeScript), Biome 2, Vitest 5, Playwright + axe and Lighthouse CI.

## Develop

Node and pnpm come from the repository's `mise.toml` (`mise install`). Install from the repo root, which is a pnpm
workspace.

```sh
pnpm install          # at the repo root
pnpm dev              # http://localhost:4321 (pages only)
pnpm build            # astro build, then CSP hashing into build/nginx/security-headers.conf
pnpm preview          # serve dist/
```

To run the waitlist end to end (Turnstile test keys, email links printed to the log instead of sent):

```sh
cp .env.example .env    # then set WAITLIST_SECRET (openssl rand -base64 48)
pnpm serve              # http://127.0.0.1:8787; `pnpm dev` doesn't proxy /api, so post to it directly
```

| Script          | What it does                                                                                                                        |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `pnpm lint`     | Biome (format + lint), then `astro check` and `tsc` for the service                                                                 |
| `pnpm format`   | Biome with fixes applied                                                                                                            |
| `pnpm test`     | Vitest: the service (in-memory and file SQLite, real HTTP socket, migrations, config) and the CSP script                            |
| `pnpm test:e2e` | Playwright + axe on desktop and mobile, both colour schemes, under the headers in `build/nginx/security-headers.conf` (build first) |
| `pnpm lhci`     | Lighthouse CI on five pages, 3 runs each, ≥ 95 in every category (needs `pnpm build`)                                               |
| `pnpm serve`    | Run the waitlist service with `.env`                                                                                                |

The e2e tests stub Turnstile and block every third-party request, so they never depend on the network. Set
`PLAYWRIGHT_BASE_URL` to run them against a deployment instead of `astro preview`.

## How the waitlist works

1. The form posts JSON to `/api/waitlist`: email, optional use case (≤ 500 characters), the consent checkbox, the
   consent wording version, the Turnstile token, and the source (`?ref=`, `utm_source` or the referring host) and
   `utm_*` parameters.
2. The service validates with zod, normalises the email (trim, NFC, lowercase, punycode domain; no Gmail-style
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
and the service refuses sign-ups with `503` instead of sending email that isn't CASL-compliant.

The SQLite table (`migrations/0001_create_waitlist.sql`, applied by the service at start) is `waitlist(id, email UNIQUE, use_case, source, utm,
consent_version, ip_hash, created_at, confirmed_at, unsubscribed_at, confirm_sent_at)`. `confirm_sent_at` is one
column beyond the plan; it throttles confirmation re-sends. The mailable audience is
`confirmed_at IS NOT NULL AND unsubscribed_at IS NULL`.

## Configuration and deploys

Everything server-side is in [`deploy/`](../../deploy/README.md). Per environment, two files configure this package
(each has a committed `.example`; the real files are git-ignored and every deploy uploads them, replacing the
server's copy):

- `deploy/env/<env>/build.env`: `PUBLIC_TURNSTILE_SITE_KEY` and `PUBLIC_CF_ANALYTICS_TOKEN`, read by `pnpm build`.
  Without a site key the build uses Cloudflare's always-pass test key. `deploy.sh` sets `HEADMESH_PRODUCTION=1`
  for prod, which fails the build while the Turnstile key is the test key or `SENDER.postalAddress` is empty.
- `deploy/env/<env>/waitlist.env`: the service's settings (`server/config.ts`): `SITE_URL`, `EMAIL_FROM`,
  `EMAIL_REPLY_TO`, `EMAIL_DELIVERY` (`resend` in prod, `log` in test), `TURNSTILE_SECRET_KEY`, `RESEND_API_KEY`
  and `WAITLIST_SECRET`. The listen address, port and database path are set per instance on the server.

```sh
deploy/deploy.sh --env test --target frontend          # test.headmesh.dev
deploy/deploy.sh --env prod --target frontend          # headmesh.dev (asks for confirmation)
```

The site switches atomically (a symlink, `nginx -t` first); the service runs blue-green, so a deploy never drops a
sign-up. The service starts before the site, so a new form never posts to an old API.

## One-time setup (Aaryn)

1. **CASL sender address.** Fill in `SENDER.postalAddress` in `shared/site.ts`. Production builds fail without it.
2. **Turnstile.** In the Cloudflare dashboard, add a Turnstile widget (Managed) for `headmesh.dev`. The site key goes
   in `deploy/env/prod/build.env`, the secret in `deploy/env/prod/waitlist.env`.
3. **Resend.** Add the domain `mail.headmesh.dev`, create the SPF, DKIM and bounce (MX) records it lists in
   Cloudflare DNS, and wait for verification. Publish a DMARC record for `headmesh.dev` if there isn't one. Put an API
   key restricted to sending from that domain in `deploy/env/prod/waitlist.env`.
4. **Web Analytics.** Add `headmesh.dev` under _Web Analytics_ with the manual JS snippet (not automatic injection,
   so the CSP stays exact), and put its token in `deploy/env/prod/build.env`.
5. **DNS and TLS:** see [`deploy/README.md`](../../deploy/README.md#dns-and-tls).
6. **Rate limiting.** nginx allows 6 sign-up requests a minute per client (burst 5) and the service caps each IP at
   10 sign-ups an hour. As an outer layer at the edge, add a Cloudflare WAF rate-limiting rule for
   `POST /api/waitlist*` on the zone, for example 20 requests per 10 minutes per IP.
7. **Before launch:** have the privacy policy and terms reviewed (they have no governing-law clause yet), and make
   the GitHub repository public so the `/github` and "Star on GitHub" links work.

## Operations

There's no admin route; read the waitlist on the server (as root) with `sqlite3`. The database is
`/var/lib/headmesh/<env>/waitlist/waitlist.db`, backed up daily to `/var/lib/headmesh/<env>/backups/` (14 kept).

```sh
db=/var/lib/headmesh/prod/waitlist/waitlist.db

# Sign-up count, the success metric
sudo sqlite3 "$db" "SELECT count(*) AS confirmed FROM waitlist WHERE confirmed_at IS NOT NULL AND unsubscribed_at IS NULL"

# Export the mailable list as CSV
sudo sqlite3 -header -csv "$db" "SELECT email, use_case, source, utm, created_at, confirmed_at FROM waitlist
  WHERE confirmed_at IS NOT NULL AND unsubscribed_at IS NULL ORDER BY created_at" > waitlist.csv

# Delete sign-ups that were never confirmed (the privacy policy promises this; run it monthly)
sudo sqlite3 "$db" "DELETE FROM waitlist WHERE confirmed_at IS NULL AND created_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-30 days')"

# Live service logs (structured JSON, never email addresses)
sudo journalctl -fu 'headmesh-waitlist@prod-*'
```

`/.well-known/security.txt` gets an `Expires` 330 days after each build, so redeploy at least once every ten
months.

## Security headers

nginx sends the headers in `build/nginx/security-headers.conf`, generated per build: a CSP that allows only this
origin, the SHA-256 hashes of the few inline scripts and styles (theme detection and Starlight's own), Turnstile
(`challenges.cloudflare.com`) and the Web Analytics beacon (`static.cloudflareinsights.com`,
`cloudflareinsights.com`), plus HSTS, `nosniff`, `X-Frame-Options`, Referrer-Policy, Permissions-Policy and COOP.
Stylesheets and bundled scripts ship as files (`inlineStylesheets: "never"`, `assetsInlineLimit: 0`). Inline style
_attributes_ are allowed through `style-src-attr` because Starlight sets CSS custom properties that way; they can't
load resources or run code. The build fails if an inline event handler appears. The service sets its own
`no-store`, `nosniff` and `default-src 'none'` headers on API responses. The test environment adds
`X-Robots-Tag: noindex`.
