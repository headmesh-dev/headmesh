# deploy

Scripts that set up and deploy Headmesh's own servers. hm-prod-1 (Hetzner `cx23`, nbg1, Ubuntu 26.04) runs two
environments side by side, each with its own hostnames, services, database and configuration:

| Environment | Site                | API                     | Cell endpoint (not public) | Indexing      |
| ----------- | ------------------- | ----------------------- | -------------------------- | ------------- |
| `prod`      | `headmesh.dev` (+`www`) | `api.headmesh.dev`      | `:7443`                    | allowed       |
| `test`      | `test.headmesh.dev` | `api-test.headmesh.dev` | `:7444`                    | `noindex`     |

The table lives in [`lib/environments.sh`](lib/environments.sh), which both sides read, so they can't disagree.

| Path                         | What it is                                                                                                   |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `deploy.sh`                  | Build on this machine and deploy: `--env prod\|test\|all --target frontend\|backend\|all`                    |
| `server/deployctl.sh`        | The server half: uploads, env files, nginx transactions, blue-green switches, rollback, status, backups      |
| `server/bootstrap.sh`        | The server's application stack: packages, Node, Postgres roles and databases, units, TLS (`--bootstrap`)     |
| `lib/environments.sh`        | Hostnames, ports and paths per environment                                                                   |
| `nginx/`                     | Templates for the vhosts, the stream (TCP) proxy for cell endpoints, shared http settings and snippets       |
| `systemd/`                   | Template units: `headmesh-waitlist@<env>-<colour>`, `headmesh-backend@<env>-<colour>`, `headmesh-backup@<env>` |
| `env/<env>/*.env.example`    | Configuration templates; the real `*.env` files next to them are git-ignored                                 |
| `tests/e2e.sh`               | The whole pipeline against a throwaway systemd container (see [Testing](#testing))                            |
| `compose/bootstrap.sh`       | Base host setup, run once before anything else: admin user, sshd, host firewall, Docker, Tailscale, Ubuntu Pro |
| `breakglass/`, `cellagent/`, `host/` | Break-glass WireGuard (§13.7), the cell agent package (§9.4), ASUS host prep for the sandbox (§5.3) |

## How a deploy works

```
deploy.sh (this machine)                         hm-prod-1 (over the tailnet, sudo)
  lint + tests ─► build ─► render nginx ─► upload ─► deployctl
                                                      ├─ env files: always replace the server's copies
                                                      ├─ nginx files: install; on drift keep the server's copy
                                                      │              unless --nginx-overwrite; nginx -t, then reload
                                                      ├─ backend:  migrate ─► start idle colour ─► /readyz ─► switch
                                                      ├─ waitlist: back up DB ─► start idle colour ─► /readyz ─► switch
                                                      ├─ site:     switch the `current` symlink (nginx -t first)
                                                      └─ smoke test through nginx ─► prune old releases
```

- **nginx -t before every reload.** Every change to nginx files, a vhost, an upstream switch or the site symlink,
  goes through one transaction: write the files, run `nginx -t`, and reload only if it passes. If it fails, every
  touched file is put back and nginx keeps serving the old configuration. The deploy stops there.
- **Blue-green.** The waitlist and the backend each have a blue and a green instance per environment on separate
  loopback ports. A deploy starts the idle colour on the new release, waits for its `/readyz`, points nginx at it,
  then stops the old colour after a short drain. If the new colour never becomes ready, it's stopped and the old one
  keeps serving. Both colours share the environment's database. Migrations must therefore stay compatible with the
  previous release (expand, then contract in a later release).
- **The site** is static: each release is a directory, and `current` is a symlink switched atomically.
- **Order:** backend, then waitlist, then site, so new code never talks to an older API.
- **Releases** are named `<UTC time>-<commit>` (`-dirty` with uncommitted changes, which prod refuses). The last 5
  are kept (`--keep N`), never including the live or rollback release.
- **The server always runs the matching deployctl**: every run uploads `deployctl.sh`, `bootstrap.sh`,
  `environments.sh` and the units first.

## Usage

```sh
deploy/deploy.sh --env test --target all                 # build and deploy everything to test
deploy/deploy.sh --env prod --target frontend            # headmesh.dev only; asks you to type "prod"
deploy/deploy.sh --env all --target backend --yes        # the API in both environments, no prompt
deploy/deploy.sh --env test --target frontend --dry-run  # build, upload, show nginx drift; switch nothing
deploy/deploy.sh --env prod --target backend --rollback  # back to the previous colour
deploy/deploy.sh --status                                # releases, colours, unit and health state
```

| Flag                | Meaning                                                                                                         |
| ------------------- | --------------------------------------------------------------------------------------------------------------- |
| `--env`             | `prod`, `test` or `all`                                                                                         |
| `--target`          | `frontend` (site + waitlist), `backend` (headmesh-server) or `all`                                              |
| `--host DEST`       | SSH destination. Default `HM_DEPLOY_HOST` from `deploy/env/local.env` (hm-prod-1 over the tailnet)              |
| `--identity FILE`   | SSH key. Default `$HM_DEPLOY_IDENTITY`, else `~/.ssh/hm-prod-1` when it exists                                  |
| `--bootstrap`       | Run the server bootstrap first (idempotent)                                                                     |
| `--nginx-overwrite` | Replace server nginx files that differ from the local render                                                    |
| `--init-env`        | Create missing `deploy/env/<env>/*.env` from the examples, generating `WAITLIST_SECRET`                         |
| `--skip-checks`     | Skip lint and tests                                                                                             |
| `--dry-run`         | Build, upload and validate; no switch, no reload                                                                |
| `--rollback`        | Previous site release and previous service colour; builds nothing                                               |
| `--status`          | What's live; builds nothing                                                                                     |
| `--keep N`          | Releases kept per component (default 5)                                                                         |
| `--allow-dirty`     | Deploy uncommitted changes (test only)                                                                          |
| `--yes`             | Don't prompt before touching prod                                                                               |

## Configuration

Each environment has three files in `deploy/env/<env>/`, created from their `.example` (or with `--init-env`). They
are never committed, and every deploy uploads them, so the copy on your machine is the source of truth.

| File           | Used by                     | Holds                                                                         |
| -------------- | --------------------------- | ----------------------------------------------------------------------------- |
| `build.env`    | `pnpm build` (frontend)     | `PUBLIC_TURNSTILE_SITE_KEY`, `PUBLIC_CF_ANALYTICS_TOKEN`                      |
| `waitlist.env` | the waitlist service        | Site URL, email sender and delivery mode, Turnstile secret, Resend key, secret |
| `backend.env`  | headmesh-server             | Public URL, install URL, log level                                            |

`deploy.sh` refuses to run while a file still has a `CHANGE_ME` value. The server generates the rest and never sends
it anywhere: `/etc/headmesh/<env>/db.env` (the app role's database URL), `db-owner.env` (the schema owner's, root
only, used for migrations), and `secrets.env` with the operation-signing key and the cell CA under
`/etc/headmesh/<env>/backend/`.

## First deploy

1. The base host is already set up (`compose/bootstrap.sh init`, `firewall`, `lock-root`; plan §5.8).
2. `deploy/deploy.sh --env all --init-env`, then fill in the prod values (see `web/marketing/README.md`, "One-time
   setup").
3. `deploy/deploy.sh --env test --target all --bootstrap` and check `https://test.headmesh.dev` once DNS points at
   the server.
4. `deploy/deploy.sh --env prod --target all --bootstrap`.

## DNS and TLS

Point these Cloudflare records at hm-prod-1's public IPv4 (and the AAAA at its IPv6). They can be proxied (orange
cloud): nginx trusts `CF-Connecting-IP` only from Cloudflare's published ranges, refreshed on every deploy.

```
headmesh.dev  www.headmesh.dev  api.headmesh.dev  test.headmesh.dev  api-test.headmesh.dev
```

Until a real certificate exists, the bootstrap installs a self-signed placeholder for `headmesh.dev` and
`*.headmesh.dev`. That works with Cloudflare's SSL mode **Full**. For **Full (strict)**:

1. Create a Cloudflare API token with **Zone → DNS → Edit** on `headmesh.dev` only, and put it on the server:
   `sudo install -m 0600 /dev/stdin /etc/headmesh/cloudflare.ini <<< 'dns_cloudflare_api_token = …'`.
2. Run `HM_ACME_EMAIL=hello@headmesh.dev deploy/deploy.sh --env all --bootstrap`. Setting `HM_ACME_EMAIL` agrees to
   the Let's Encrypt subscriber agreement. certbot gets one certificate for both names by DNS-01, and `certbot.timer`
   renews it; the renewal hook runs `deployctl tls-renewed`, which reloads nginx (after `nginx -t`) and the backends.
3. Switch the zone's SSL mode to Full (strict).

**Cell endpoints are not public.** nginx passes the TLS of ports 7443 (prod) and 7444 (test) straight through to the
live backend colour, which checks the cell agent's client certificate. The host firewall only admits TCP 80 and 443
publicly, so cells reach these ports over loopback (hs1 on this server) or the tailnet (the xcelsior cell on
prod-hetzner, once its policy grant is approved for M4).

## Operations

```sh
deploy/deploy.sh --status
ssh hm-prod-1 sudo journalctl -fu 'headmesh-backend@prod-*'      # or headmesh-waitlist@…
ssh hm-prod-1 sudo /usr/local/lib/headmesh/deployctl.sh backup prod   # also daily at 03:17 UTC, 14 kept
```

On the server, releases are in `/srv/headmesh/<env>/<component>/releases/`, the active instance links in
`/srv/headmesh/slots/`, state in `/var/lib/headmesh/<env>/`, nginx's per-environment files in
`/etc/nginx/headmesh/<env>/`, and the last 20 nginx transaction backups in `/var/lib/headmesh/nginx-backups/`.

## Testing

```sh
deploy/tests/e2e.sh
```

It starts an Ubuntu 26.04 container with systemd, runs the real `deploy.sh` against it through `docker exec`
(`HM_DEPLOY_REMOTE`), and checks the bootstrap, two deploys (blue then green), a waitlist sign-up and confirmation
through nginx, the database roles and their privileges, nginx drift handling, a config that fails `nginx -t`, a dry
run, a rollback, and a prod backend beside test. It needs Docker, Go, Node and pnpm, and takes a few minutes.
`HM_E2E_KEEP=1` leaves the container running.
