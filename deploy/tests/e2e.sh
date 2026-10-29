#!/usr/bin/env bash
# End-to-end test of the deploy pipeline: deploy.sh → bootstrap.sh → deployctl.sh, unchanged, against a throwaway
# Ubuntu 26.04 container with systemd standing in for hm-prod-1. It builds the real site, waitlist service and
# headmesh-server, deploys them twice (blue, then green), and checks nginx, health, a real waitlist sign-up, the
# database roles, nginx drift handling, the nginx -t guard, dry runs, rollback, and prod's Headscale cell hs1 with
# its cell agent connected to the backend.
#
#   deploy/tests/e2e.sh             needs Docker (privileged containers), Go, Node and pnpm; takes a few minutes
#   HM_E2E_KEEP=1 deploy/tests/e2e.sh   leave the container running afterwards
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NAME="hm-deploy-e2e-$$"
IMAGE="headmesh-deploy-e2e"
ENVDIR="$(mktemp -d)"
DEPLOY="${ROOT}/deploy/deploy.sh"
FAILED=0

say() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILED=1; }
check() { local what="$1"; shift; if "$@"; then ok "${what}"; else bad "${what}"; fi; }

cleanup() {
  if [[ -n "${HM_E2E_KEEP:-}" ]]; then
    echo "container ${NAME} left running (docker rm -f ${NAME})"
  else
    docker rm -f "${NAME}" >/dev/null 2>&1 || true
  fi
  rm -rf "${ENVDIR}"
}
trap cleanup EXIT

on() { docker exec "${NAME}" "$@"; }
# code <host> <path> [curl args]: HTTP status of a request made through nginx inside the container.
code() {
  local host="$1" path="$2"
  shift 2
  on curl -ks -o /dev/null -w '%{http_code}' --resolve "${host}:443:127.0.0.1" "$@" "https://${host}${path}" || true
}
header() { on curl -ksI --resolve "$1:443:127.0.0.1" "https://$1$2" | tr -d '\r' | grep -i "^$3:" | cut -d' ' -f2-; }
live() { on cat "/var/lib/headmesh/$1/state/$2" 2>/dev/null || true; }
deploy() { "${DEPLOY}" "$@" 2>&1 | tee -a "${ENVDIR}/deploy.log" | sed 's/^/    /'; return "${PIPESTATUS[0]}"; }
psql_su() { on runuser -u postgres -- psql -X -tA "$@"; }

export HM_DEPLOY_REMOTE="docker exec -i ${NAME}" HM_DEPLOY_SUDO="" HM_DEPLOY_ENV_DIR="${ENVDIR}"
# Nothing from this workstation's own settings: no real Cloudflare token or ACME email reaches the container.
export HM_DEPLOY_LOCAL_ENV=/dev/null HM_DEPLOY_DOTENV=/dev/null
unset CLOUDFLARE_DNS_API_TOKEN CLOUDFLARE_API_TOKEN HM_ACME_EMAIL HM_DEPLOY_ACME_EMAIL
cp -r "${ROOT}/deploy/env/." "${ENVDIR}/"
find "${ENVDIR}" -name '*.env' -delete

say "Start the stand-in server"
docker build -q -t "${IMAGE}" "${ROOT}/deploy/tests" >/dev/null
docker run -d --name "${NAME}" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock "${IMAGE}" >/dev/null
for _ in $(seq 1 60); do
  state="$(on systemctl is-system-running 2>/dev/null || true)"
  [[ "${state}" == running || "${state}" == degraded ]] && break
  sleep 1
done
ok "systemd is ${state}"

say "Bootstrap and first deploy of test (blue)"
deploy --env test --init-env
check "init-env generated a waitlist secret" grep -qE '^WAITLIST_SECRET=.{40,}$' "${ENVDIR}/test/waitlist.env"
# A Global API Key (37 hex characters) must never be installed on the server.
CLOUDFLARE_DNS_API_TOKEN="$(printf '0%.0s' {1..37})" CLOUDFLARE_API_TOKEN=account-wide-token deploy --env test --target all --bootstrap --allow-dirty --skip-checks
check "a Global API Key is refused" grep -q 'Global API Key, not an API token; not installing it' "${ENVDIR}/deploy.log"
check "…and neither it nor CLOUDFLARE_API_TOKEN reaches the server" bash -c "! docker exec ${NAME} test -e /etc/headmesh/cloudflare.ini"
check "test hosts no Headscale" bash -c "! docker exec ${NAME} systemctl is-active --quiet headscale"
first="$(live test marketing.active)"
check "site release is live (${first})" test -n "${first}"
check "waitlist runs on blue" test "$(live test waitlist.active)" = blue
check "backend runs on blue" test "$(live test backend.active)" = blue
check "GET / → 200" test "$(code test.headmesh.dev /)" = 200
check "GET /docs/ → 200" test "$(code test.headmesh.dev /docs/)" = 200
check "CSP header is sent" test -n "$(header test.headmesh.dev / content-security-policy)"
check "test is noindex" grep -q noindex <<<"$(header test.headmesh.dev / x-robots-tag)"
check "llms.txt is served as UTF-8 text" grep -q 'text/plain; charset=utf-8' <<<"$(header test.headmesh.dev /llms.txt content-type)"
check "docs Markdown copies are served as text/markdown" grep -q 'text/markdown' <<<"$(header test.headmesh.dev /docs/quickstart.md content-type)"
check "test's robots.txt disallows everything" grep -qx 'Disallow: /' <<<"$(on curl -ks --resolve test.headmesh.dev:443:127.0.0.1 https://test.headmesh.dev/robots.txt)"
check "/github redirects to the repo" test "$(header test.headmesh.dev /github location)" = "https://github.com/headmesh-dev/headmesh"
check "http:// redirects to https://" test "$(on curl -s -o /dev/null -w '%{http_code}' -H 'Host: test.headmesh.dev' http://127.0.0.1/)" = 301
check "GET /api/waitlist → 405" test "$(code test.headmesh.dev /api/waitlist)" = 405
check "API /healthz → 204" test "$(code api-test.headmesh.dev /healthz)" = 204
check "API /readyz → 204 (database reachable as the app role)" test "$(code api-test.headmesh.dev /readyz)" = 204
check "unknown hostnames get no TLS handshake" bash -c "! docker exec ${NAME} curl -ks --resolve nope.example:443:127.0.0.1 https://nope.example/ >/dev/null 2>&1"
unit_user() { on ps -o user= -p "$(on systemctl show -p MainPID --value "$1")" | tr -d ' '; }
check "the waitlist runs as the headmesh user" test "$(unit_user headmesh-waitlist@test-blue)" = headmesh
check "the backend runs as the headmesh user" test "$(unit_user headmesh-backend@test-blue)" = headmesh

say "Waitlist sign-up and confirmation through nginx"
consent_version="$(sed -n 's/^export const CONSENT_VERSION = "\(.*\)";$/\1/p' "${ROOT}/web/marketing/shared/site.ts")"
body='{"email":"e2e@example.com","use_case":"deploy test","consent":true,"consent_version":"'"${consent_version}"'","turnstile_token":"XXXX.DUMMY.TOKEN.XXXX"}'
check "POST /api/waitlist → 202" test "$(code test.headmesh.dev /api/waitlist -X POST -H 'content-type: application/json' -H 'origin: https://test.headmesh.dev' --data "${body}")" = 202
sleep 1
token="$(on journalctl -u 'headmesh-waitlist@test-blue' -o cat --no-pager | grep email_logged | tail -n 1 | grep -oE 'confirm[^"]*token=[^"&]+' | sed 's/.*token=//' || true)"
check "the confirmation link was logged (EMAIL_DELIVERY=log)" test -n "${token}"
check "POST /api/waitlist/confirm → 200" test "$(code test.headmesh.dev /api/waitlist/confirm -X POST -H 'content-type: application/json' -H 'origin: https://test.headmesh.dev' --data "{\"token\":\"${token}\"}")" = 200
check "the sign-up is confirmed in SQLite" test "$(on sqlite3 /var/lib/headmesh/test/waitlist/waitlist.db "select count(*) from waitlist where email='e2e@example.com' and confirmed_at is not null")" = 1

say "Database roles"
check "migrations applied (≥ 3)" test "$(psql_su -d headmesh_test -c 'select max(version_id) from goose_db_version')" -ge 3
check "app role can insert into audit_events" test "$(psql_su -d headmesh_test -c "select has_table_privilege('headmesh_test_app','audit_events','INSERT')")" = t
check "app role can't update audit_events" test "$(psql_su -d headmesh_test -c "select has_table_privilege('headmesh_test_app','audit_events','UPDATE')")" = f
check "app role can't bypass row-level security" test "$(psql_su -c "select rolbypassrls from pg_roles where rolname='headmesh_test_app'")" = f
check "the running backend has no schema-owner credentials" bash -c "! docker exec ${NAME} grep -q MIGRATE /etc/headmesh/instances/backend-test-blue.env"

say "Second deploy switches to green"
deploy --env test --target all --allow-dirty --skip-checks
check "waitlist moved to green" test "$(live test waitlist.active)" = green
check "backend moved to green" test "$(live test backend.active)" = green
check "blue waitlist stopped" test "$(on systemctl is-active headmesh-waitlist@test-blue || true)" = inactive
check "previous site release recorded" test "$(live test marketing.previous)" = "${first}"
check "site still serves" test "$(code test.headmesh.dev /)" = 200
check "the sign-up survived the switch" test "$(on sqlite3 /var/lib/headmesh/test/waitlist/waitlist.db 'select count(*) from waitlist')" = 1

say "nginx drift is reported, and replaced only with --nginx-overwrite"
on bash -c 'echo "# hand edit" >> /etc/nginx/sites-available/headmesh-test.conf'
deploy --env test --target frontend --allow-dirty --skip-checks
check "drift was reported" grep -q 'drift: /etc/nginx/sites-available/headmesh-test.conf' "${ENVDIR}/deploy.log"
check "the hand edit was kept" on grep -q '# hand edit' /etc/nginx/sites-available/headmesh-test.conf
deploy --env test --target frontend --allow-dirty --skip-checks --nginx-overwrite
check "--nginx-overwrite replaced it" bash -c "! docker exec ${NAME} grep -q '# hand edit' /etc/nginx/sites-available/headmesh-test.conf"

say "A config that fails nginx -t is never loaded"
before="$(on sha256sum /etc/nginx/sites-available/headmesh-test.conf)"
# shellcheck disable=SC2016 # the script runs inside the container
on bash -c '
  set -e
  s=/var/lib/headmesh/broken; rm -rf $s; mkdir -p $s/snippets $s/sites $s/stream
  cp /etc/nginx/headmesh/cloudflare-realip.conf $s/cloudflare-realip.conf
  cp /etc/nginx/conf.d/headmesh.conf $s/headmesh.conf
  cp /etc/nginx/snippets/headmesh-tls.conf /etc/nginx/snippets/headmesh-proxy.conf $s/snippets/
  cp /etc/nginx/modules-enabled/99-headmesh-stream.conf $s/stream-include.conf
  cp /etc/nginx/headmesh/test/stream.conf $s/stream/test.conf
  { cat /etc/nginx/sites-available/headmesh-test.conf; echo "this is not nginx;"; } > $s/sites/headmesh-test.conf'
check "nginx-apply refuses the broken config" bash -c "! docker exec ${NAME} /usr/local/lib/headmesh/deployctl.sh nginx-apply test /var/lib/headmesh/broken 1 >/dev/null 2>&1"
check "the previous file is back in place" test "$(on sha256sum /etc/nginx/sites-available/headmesh-test.conf)" = "${before}"
check "nginx -t passes" on nginx -t -q
check "nginx still serves" test "$(code test.headmesh.dev /)" = 200

say "Dry run changes nothing"
live_before="$(live test marketing.active)"
deploy --env test --target frontend --allow-dirty --skip-checks --dry-run
check "the live release is unchanged" test "$(live test marketing.active)" = "${live_before}"

say "Rollback"
deploy --env test --target frontend --rollback
check "the waitlist is back on blue" test "$(live test waitlist.active)" = blue
check "the site is on the previous release" test "$(live test marketing.active)" != "${live_before}"
check "site serves after rollback" test "$(code test.headmesh.dev /)" = 200

say "prod backend alongside test"
deploy --env prod --init-env
CLOUDFLARE_DNS_API_TOKEN=e2e-scoped-token deploy --env prod --target backend --bootstrap --skip-checks --yes --allow-dirty
check "prod API /readyz → 204" test "$(code api.headmesh.dev /readyz)" = 204
check "the DNS token is installed root-only for certbot" test "$(on stat -c '%a %U' /etc/headmesh/cloudflare.ini)" = "600 root"
check "…with the token from the environment" on grep -qx 'dns_cloudflare_api_token = e2e-scoped-token' /etc/headmesh/cloudflare.ini
check "without HM_ACME_EMAIL the placeholder stays" grep -q 'HM_ACME_EMAIL is unset; keeping the placeholder' "${ENVDIR}/deploy.log"
check "www redirects to the apex" test "$(header www.headmesh.dev /docs/ location)" = "https://headmesh.dev/docs/"
check "test still serves" test "$(code test.headmesh.dev /)" = 200
prod_url="$(on sed -n 's/^HEADMESH_DATABASE_URL=//p' /etc/headmesh/prod/db.env)"
check "prod's app role can't open the test database" bash -c "! docker exec ${NAME} psql -X '${prod_url%/*}/headmesh_test' -c 'select 1' >/dev/null 2>&1"

say "prod's Headscale cell hs1 and its agent"
seen() { psql_su -d headmesh_prod -c "select coalesce(extract(epoch from last_seen_at)::bigint, 0) from cells where name = 'hs1'"; }
check "Headscale is running" on systemctl is-active --quiet headscale
check "Headscale is the pinned release" grep -q '0.29.6' <<<"$(on headscale version 2>&1)"
check "hs1.headmesh.dev/health → 200 through nginx" test "$(code hs1.headmesh.dev /health)" = 200
hs_public() { on ss -Hltn | awk '{print $4}' | grep -E ':(8080|50443|9090)$' | grep -v '^127\.0\.0\.1:' || true; }
check "Headscale's HTTP, gRPC and metrics listen on loopback only" test -z "$(hs_public)"
check "the cell is registered in prod" test "$(psql_su -d headmesh_prod -c "select mode from cells where name = 'hs1'")" = enforce
check "the bootstrap's admin key is root-only" test "$(on stat -c '%a %U' /etc/headmesh/prod/cell-hs1-bootstrap.json)" = "600 root"
check "the cell agent is running" on systemctl is-active --quiet headmesh-cellagent
check "…as its own user" test "$(unit_user headmesh-cellagent)" = headmesh-cellagent
check "the agent reached the backend (cells.last_seen_at)" test "$(seen)" -gt 0
check "…and reported Headscale's version" grep -q '0.29.6' <<<"$(psql_su -d headmesh_prod -c "select headscale_version from cells where name = 'hs1'")"
check "…and Headscale's Noise key fingerprint" grep -qE '^[0-9a-f]{64}$' <<<"$(psql_su -d headmesh_prod -c "select data->>'noise_public_key_sha256' from audit_events where action = 'cell.connected' order by id desc limit 1")"
check "the certificate renewal timer is on" on systemctl is-enabled --quiet headmesh-cellcert@prod.timer
on /usr/local/lib/headmesh/deployctl.sh cell-agent prod 2>&1 | tee -a "${ENVDIR}/deploy.log" | sed 's/^/    /'
check "running the step again changes nothing" grep -q 'cell hs1: agent unchanged and running' "${ENVDIR}/deploy.log"
before_seen="$(seen)"
sleep 1
deploy --env prod --target backend --skip-checks --yes --allow-dirty
check "prod backend moved to green" test "$(live prod backend.active)" = green
check "the agent reconnected to the new release" test "$(seen)" -gt "${before_seen}"

say "Status"
"${DEPLOY}" --status 2>&1 | sed 's/^/    /'

if [[ ${FAILED} == 0 ]]; then
  printf '\n\033[1;32mdeploy e2e: all checks passed\033[0m\n'
else
  printf '\n\033[1;31mdeploy e2e: some checks failed\033[0m\n'
  exit 1
fi
