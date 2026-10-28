#!/usr/bin/env bash
# Server side of `deploy/deploy.sh --bootstrap`: everything the Headmesh application stack needs on hm-prod-1
# (Ubuntu 26.04), for the given environments. Runs as root; every step is idempotent and never replaces existing
# secrets. deploy/compose/bootstrap.sh sets up the base host first (admin user, sshd, host firewall, Tailscale).
#
#   bootstrap.sh <env>...          for example: bootstrap.sh prod test
#
# What it does:
#   - packages: nginx with the stream module, PostgreSQL 18, sqlite3, certbot with the Cloudflare DNS plugin
#   - Node.js (the pinned release the waitlist service and CI use), checksum-verified, at /opt/node
#   - the unprivileged `headmesh` service user and the directories under /srv, /var/lib and /etc/headmesh
#   - per environment: database headmesh_<env> with two roles, headmesh_<env>_owner (runs migrations) and
#     headmesh_<env>_app (the service; no BYPASSRLS, so row-level security applies). Their URLs go in
#     /etc/headmesh/<env>/db-owner.env (root only) and db.env (readable by the service)
#   - the systemd units and the daily waitlist backup timer
#   - TLS: a Let's Encrypt certificate for headmesh.dev and *.headmesh.dev by DNS-01 when
#     /etc/headmesh/cloudflare.ini holds a Cloudflare API token (Zone:DNS:Edit on headmesh.dev only) and
#     HM_ACME_EMAIL is set; until then a self-signed placeholder (enough for Cloudflare's "Full" SSL mode)
#   - nginx: Ubuntu's default site is disabled (headmesh.conf takes over as the default server)
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/environments.sh
source "${HERE}/environments.sh" 2>/dev/null || source "${HERE}/../lib/environments.sh"

# Pinned runtime (plan §3.2; matches mise.toml and CI). Bump together with its SHA-256 from SHASUMS256.txt.
readonly NODE_VERSION="24.16.0"
readonly NODE_SHA256="d804845d34eddc21dc1092b519d643ef40b1f58ec5dec5c22b1f4bd8fabde6c9"
readonly PG_MAJOR="18"
readonly ACME_EMAIL="${HM_ACME_EMAIL:-}"

export DEBIAN_FRONTEND=noninteractive

log() { printf '[bootstrap] %s\n' "$*" >&2; }
die() { printf '[bootstrap] ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

# write_if_changed <path> <mode> <owner:group>: stdin → path, only when the content differs. Returns 1 if unchanged.
write_if_changed() {
  local path="$1" mode="$2" owner="$3" tmp
  tmp="$(mktemp)"
  cat >"${tmp}"
  if [[ -f "${path}" ]] && cmp -s "${tmp}" "${path}"; then
    rm -f "${tmp}"
    return 1
  fi
  install -D -m "${mode}" -o "${owner%%:*}" -g "${owner##*:}" "${tmp}" "${path}"
  rm -f "${tmp}"
  log "wrote ${path}"
}

preflight() {
  [[ ${EUID} -eq 0 ]] || die "run as root"
  # shellcheck source=/dev/null
  . /etc/os-release
  [[ "${ID}" == ubuntu && "${VERSION_ID}" == 26.04 ]] || die "expected Ubuntu 26.04, found ${PRETTY_NAME}"
  [[ $# -gt 0 ]] || die "usage: bootstrap.sh <env>... (one or more of: ${HM_ENVS[*]})"
  local env
  for env in "$@"; do hm_is_env "${env}" || die "unknown environment '${env}'"; done
}

packages() {
  local -a want=(nginx libnginx-mod-stream "postgresql-${PG_MAJOR}" sqlite3 certbot python3-certbot-dns-cloudflare
    ca-certificates curl xz-utils openssl)
  local -a missing=() pkg
  for pkg in "${want[@]}"; do
    dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q 'install ok installed' || missing+=("${pkg}")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "installing ${missing[*]}"
    apt-get update -qq
    apt-get install -y --no-install-recommends "${missing[@]}" >/dev/null
  fi
}

node_runtime() {
  local dir="/opt/node-v${NODE_VERSION}-linux-x64" tarball tmp
  if [[ ! -x "${dir}/bin/node" ]]; then
    tmp="$(mktemp -d)"
    tarball="${tmp}/node.tar.xz"
    log "installing Node.js ${NODE_VERSION}"
    curl -fsSL --retry 3 -o "${tarball}" "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"
    echo "${NODE_SHA256}  ${tarball}" | sha256sum -c --quiet - || die "Node.js tarball checksum mismatch"
    tar -xJf "${tarball}" -C /opt --no-same-owner
    rm -rf "${tmp}"
  fi
  [[ "$(readlink /opt/node 2>/dev/null)" == "${dir}" ]] || ln -sfn "${dir}" /opt/node
  [[ "$(/opt/node/bin/node --version)" == "v${NODE_VERSION}" ]] || die "/opt/node is not Node.js ${NODE_VERSION}"
}

service_user() {
  if ! id -u "${HM_SERVICE_USER}" >/dev/null 2>&1; then
    useradd --system --home-dir "${HM_STATE_ROOT}" --no-create-home --shell /usr/sbin/nologin "${HM_SERVICE_USER}"
    log "created user ${HM_SERVICE_USER}"
  fi
  install -d -m 0755 "${HM_SRV_ROOT}" "${HM_LIB_ROOT}" "${HM_NGINX_ROOT}"
  install -d -m 0755 -o root -g "${HM_SERVICE_USER}" "${HM_STATE_ROOT}"
  install -d -m 0750 -o root -g "${HM_SERVICE_USER}" "${HM_ETC_ROOT}" "${HM_ETC_ROOT}/instances"
  install -d -m 0755 "${HM_ETC_ROOT}/tls"
}

postgres() {
  local conf="/etc/postgresql/${PG_MAJOR}/main/conf.d/headmesh.conf"
  # Sized for a cx23 (4 GB) sharing the box with nginx, Headscale and the services (plan §13.2).
  if write_if_changed "${conf}" 0644 postgres:postgres <<'EOF'; then
# Managed by deploy/server/bootstrap.sh
listen_addresses = 'localhost'
password_encryption = scram-sha-256
shared_buffers = 512MB
effective_cache_size = 1536MB
work_mem = 8MB
maintenance_work_mem = 128MB
max_connections = 60
log_min_duration_statement = 500ms
EOF
    systemctl restart "postgresql@${PG_MAJOR}-main"
  fi
  systemctl enable --quiet --now "postgresql@${PG_MAJOR}-main"
  for _ in $(seq 1 30); do runuser -u postgres -- pg_isready -q && return 0; sleep 1; done
  die "PostgreSQL did not become ready"
}

psql_su() { runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 "$@"; }

# database <env>: one database and two roles; passwords are created once and only live in the env files.
database() {
  local env="$1" db="headmesh_$1" owner="headmesh_$1_owner" app="headmesh_$1_app"
  local owner_env="${HM_ETC_ROOT}/${env}/db-owner.env" app_env="${HM_ETC_ROOT}/${env}/db.env" pw role file
  install -d -m 0750 -o root -g "${HM_SERVICE_USER}" "${HM_ETC_ROOT}/${env}"

  for role in "${owner}" "${app}"; do
    psql_su <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${role}') THEN
    CREATE ROLE ${role} LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  END IF;
END \$\$;
SQL
  done
  for role in "${owner}" "${app}"; do
    [[ "${role}" == "${owner}" ]] && file="${owner_env}" || file="${app_env}"
    [[ -s "${file}" ]] && continue
    # Hex keeps the password URL-safe. psql reads it from stdin, so it never appears in a process list.
    pw="$(openssl rand -hex 24)"
    psql_su <<SQL
ALTER ROLE ${role} PASSWORD '${pw}';
SQL
    if [[ "${role}" == "${owner}" ]]; then
      printf '# Generated by deploy/server/bootstrap.sh. Schema owner: used by migrations only.\nHEADMESH_MIGRATE_DATABASE_URL=postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable\n' \
        "${role}" "${pw}" "${db}" | write_if_changed "${file}" 0600 root:root || true
    else
      printf '# Generated by deploy/server/bootstrap.sh. App role (row-level security applies).\nHEADMESH_DATABASE_URL=postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable\n' \
        "${role}" "${pw}" "${db}" | write_if_changed "${file}" 0640 "root:${HM_SERVICE_USER}" || true
    fi
  done

  if [[ -z "$(psql_su -tA -c "SELECT 1 FROM pg_database WHERE datname = '${db}'")" ]]; then
    psql_su -c "CREATE DATABASE ${db} OWNER ${owner}"
    log "created database ${db}"
  fi
  # Only this environment's roles may connect; tables the owner creates later are granted to the app role
  # automatically (deployctl re-applies the exact grants after every migration).
  psql_su -d "${db}" <<SQL
REVOKE ALL ON DATABASE ${db} FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE ${db} TO ${owner}, ${app};
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO ${owner};
GRANT USAGE ON SCHEMA public TO ${app};
ALTER DEFAULT PRIVILEGES FOR ROLE ${owner} IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ${app};
ALTER DEFAULT PRIVILEGES FOR ROLE ${owner} IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ${app};
SQL
}

state_dirs() {
  local env="$1"
  install -d -m 0755 "${HM_SRV_ROOT}/${env}"
  install -d -m 0750 -o root -g "${HM_SERVICE_USER}" "${HM_STATE_ROOT}/${env}"
  install -d -m 0750 -o "${HM_SERVICE_USER}" -g "${HM_SERVICE_USER}" "${HM_STATE_ROOT}/${env}/waitlist"
}

units() {
  local f changed=0
  for f in "${HM_LIB_ROOT}"/systemd/*; do
    write_if_changed "/etc/systemd/system/$(basename "${f}")" 0644 root:root <"${f}" && changed=1
  done
  [[ ${changed} == 1 ]] && systemctl daemon-reload
  local env
  for env in "$@"; do systemctl enable --quiet --now "headmesh-backup@${env}.timer"; done
}

tls() {
  local dir="${HM_ETC_ROOT}/tls" live="/etc/letsencrypt/live/headmesh.dev"
  if [[ ! -d "${live}" && -s "${HM_ETC_ROOT}/cloudflare.ini" ]]; then
    if [[ -z "${ACME_EMAIL}" ]]; then
      log "TLS: ${HM_ETC_ROOT}/cloudflare.ini exists but HM_ACME_EMAIL is unset; keeping the placeholder"
    else
      chmod 0600 "${HM_ETC_ROOT}/cloudflare.ini"
      log "TLS: requesting a Let's Encrypt certificate for headmesh.dev and *.headmesh.dev (DNS-01)"
      certbot certonly --non-interactive --agree-tos --email "${ACME_EMAIL}" \
        --dns-cloudflare --dns-cloudflare-credentials "${HM_ETC_ROOT}/cloudflare.ini" --dns-cloudflare-propagation-seconds 30 \
        --key-type ecdsa --cert-name headmesh.dev -d headmesh.dev -d '*.headmesh.dev' \
        --deploy-hook "${HM_LIB_ROOT}/deployctl.sh tls-renewed"
    fi
  fi
  if [[ -d "${live}" ]]; then
    ln -sfn "${live}/fullchain.pem" "${dir}/fullchain.pem"
    ln -sfn "${live}/privkey.pem" "${dir}/privkey.pem"
    log "TLS: using the Let's Encrypt certificate (renewed by certbot.timer)"
    return 0
  fi
  if [[ ! -e "${dir}/fullchain.pem" ]]; then
    install -d -m 0700 "${dir}/selfsigned"
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 825 \
      -subj "/CN=headmesh.dev" -addext "subjectAltName=DNS:headmesh.dev,DNS:*.headmesh.dev" \
      -keyout "${dir}/selfsigned/privkey.pem" -out "${dir}/selfsigned/fullchain.pem" 2>/dev/null
    chmod 0600 "${dir}/selfsigned/privkey.pem"
    ln -sfn "${dir}/selfsigned/fullchain.pem" "${dir}/fullchain.pem"
    ln -sfn "${dir}/selfsigned/privkey.pem" "${dir}/privkey.pem"
    log "TLS: self-signed placeholder in place (see deploy/README.md to get the real certificate)"
  fi
}

nginx_base() {
  systemctl enable --quiet --now nginx
  if [[ -L /etc/nginx/sites-enabled/default ]]; then
    rm -f /etc/nginx/sites-enabled/default
    if nginx -t 2>/dev/null; then
      systemctl reload-or-restart nginx
      log "nginx: disabled Ubuntu's default site"
    else
      ln -s /etc/nginx/sites-available/default /etc/nginx/sites-enabled/default
      die "nginx -t failed after disabling the default site; restored it"
    fi
  fi
}

main() {
  preflight "$@"
  packages
  node_runtime
  service_user
  postgres
  local env
  for env in "$@"; do
    database "${env}"
    state_dirs "${env}"
  done
  units "$@"
  tls
  nginx_base
  log "bootstrap complete for: $*"
}

main "$@"
