# ADR 0013: nginx and systemd on hm-prod-1, with headmesh.dev hosted there

- Status: accepted (2026-10-28, Aaryn's answers to the marketing session's deploy questions)
- Plan: changes §12.1 (deploy) and §13.2 (stack); see `deploy/README.md`
- Replaces: Caddy 2 in Docker Compose (§13.2); Cloudflare Pages, Pages Functions and D1 for headmesh.dev (§12.1)

## Context

Aaryn asked for one deploy script with exactly `--env prod|test|all --target frontend|backend|all`, nginx
configuration that it renders and installs, env files that always replace the server's copies, blue-green
switches, a server bootstrap, and `nginx -t` before every reload with the reload stopped if it fails. Asked which
proxy, where the marketing site lives and where `test` runs, he chose:

1. **nginx** rather than the plan's Caddy;
2. **headmesh.dev on the server too**, rather than Cloudflare Pages;
3. **test on the same VM** as a separate stack.

## Decision

- **nginx 1.28 from Ubuntu**, not containerized: vhosts rendered from `deploy/nginx/*.tmpl` per environment, the
  stream module passing cell-endpoint TLS through to the backend, Cloudflare real-IP handling, `limit_req` on the
  waitlist and the API, and a default server that refuses unknown hostnames. Every change is a transaction: write,
  `nginx -t`, reload, or restore every touched file.
- **systemd template units** instead of Compose for the services: `headmesh-waitlist@<env>-<colour>` and
  `headmesh-backend@<env>-<colour>`, unprivileged and sandboxed (`ProtectSystem=strict`, no capabilities, syscall
  filter), on loopback ports only.
- **PostgreSQL 18 from Ubuntu** on the host, one database and two roles per environment (schema owner for
  migrations; an app role without `BYPASSRLS`), matching how the integration tests connect.
- **The waitlist moves from Pages Functions and D1 to a Node 24 service on SQLite** (`node:sqlite`), keeping its
  validation, Turnstile, token and email code. The static site is served by nginx and switched by symlink.
- `deploy/deploy.sh` builds locally, uploads over SSH on the tailnet, and drives `deploy/server/deployctl.sh` on
  the server. `deploy/tests/e2e.sh` runs the whole pipeline against a systemd container.
- **Recovery mode is explicit** (`serve -recovery`): in a blue-green switch the cells stay connected to the live
  colour, so a startup gate that waited for them would block every deploy. Normal starts only run the expiry sweep.

## Consequences

- Marketing still deploys on its own (`--target frontend`), but it now shares hm-prod-1 with the product. A server
  outage takes the site down too; the recovery workflow (§13.4) must restore it alongside the API.
- Headscale hs1 (still to be deployed) runs behind the same nginx, so its `/ts2021` upgrade and DERP need their own
  vhost on a grey-cloud hostname.
- TLS comes from certbot by DNS-01 through Cloudflare once a scoped API token is on the server; until then a
  self-signed placeholder works with Cloudflare's "Full" mode.
- The cell endpoints (7443 prod, 7444 test) aren't public: the host firewall admits only TCP 80 and 443, so cells
  reach them over loopback or the tailnet.
- Cloudflare is still the DNS host, CDN, Turnstile and Web Analytics provider; the privacy policy says so.
