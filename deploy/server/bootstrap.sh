#!/usr/bin/env bash
# Server side of `deploy/deploy.sh --bootstrap`: everything the Headmesh application stack needs on hm-prod-1
# (Ubuntu 26.04), for the given environments. Runs as root; every step is idempotent and never replaces existing
# secrets. deploy/compose/bootstrap.sh sets up the base host first (admin user, sshd, host firewall, Tailscale).
#
#   bootstrap.sh <env>...          for example: bootstrap.sh prod test
#   bootstrap.sh prepare-recovery  install the runtime on a fresh standby with services held offline
#   bootstrap.sh restore <stage>   install the verified data on the prepared standby
#
# What it does:
#   - packages: nginx with the stream module, PostgreSQL 18, sqlite3
#   - certbot with the Cloudflare DNS plugin, hash-pinned from PyPI, at /opt/certbot (renewed by certbot-renew.timer)
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

# Pinned runtime (plan §3.2; matches mise.toml and CI). Bump together with both SHA-256 values from SHASUMS256.txt.
# renovate: datasource=node-version depName=node
readonly NODE_VERSION="24.16.0"
readonly NODE_SHA256="d804845d34eddc21dc1092b519d643ef40b1f58ec5dec5c22b1f4bd8fabde6c9"
readonly NODE_ARM64_SHA256="524659219d6a207a7400f2bde15d19ba060ffbe0d32a8643319ad67e3bb64c78"
readonly PG_MAJOR="18"
# Headscale for the environment that hosts a cell (plan §13.2), from the release's checksums.txt.
# renovate: datasource=github-releases depName=juanfont/headscale extractVersion=^v(?<version>.+)$
readonly HEADSCALE_VERSION="0.29.7"
readonly HEADSCALE_DEB_SHA256="d230c5fc8db06cf1868f4ec0077b89e5e2ccf3b22a3259efa6d080db2d5f921c"
readonly HEADSCALE_ARM64_DEB_SHA256="a68f258a2a13a87321671f8cc613500986ffc40d0d2eacfdbf0b890b8b94227e"
# certbot from PyPI rather than Ubuntu's archive, whose DNS plugin needs the end-of-life cloudflare 2.x SDK. The
# versions and hashes are in certbot-requirements.txt; bump CERTBOT_VERSION with it.
# renovate: datasource=pypi depName=certbot
readonly CERTBOT_VERSION="5.9.0"
readonly ACME_EMAIL="${HM_ACME_EMAIL:-}"
TLS_CHANGED=0
APT_UPDATED=0

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

preflight_host() {
  [[ ${EUID} -eq 0 ]] || die "run as root"
  # shellcheck source=/dev/null
  . /etc/os-release
  [[ "${ID}" == ubuntu && "${VERSION_ID}" == 26.04 ]] || die "expected Ubuntu 26.04, found ${PRETTY_NAME}"
}

preflight() {
  preflight_host
  [[ $# -gt 0 ]] || die "usage: bootstrap.sh <env>... (one or more of: ${HM_ENVS[*]})"
  local env
  for env in "$@"; do hm_is_env "${env}" || die "unknown environment '${env}'"; done
}

install_packages() {
  local -a missing=() pkg
  for pkg in "$@"; do
    dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q 'install ok installed' || missing+=("${pkg}")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "installing ${missing[*]}"
    if (( APT_UPDATED == 0 )); then
      apt-get update -qq
      APT_UPDATED=1
    fi
    apt-get install -y --no-install-recommends "${missing[@]}" >/dev/null
  fi
}

packages() {
  install_packages nginx libnginx-mod-stream "postgresql-${PG_MAJOR}" sqlite3 python3-venv \
    ca-certificates curl xz-utils openssl "$@"
}

node_runtime() {
  local arch sha dir tarball tmp
  case "$(dpkg --print-architecture)" in
    amd64) arch=x64; sha="${NODE_SHA256}" ;;
    arm64) arch=arm64; sha="${NODE_ARM64_SHA256}" ;;
    *) die 'Node.js installation supports amd64 and arm64' ;;
  esac
  dir="/opt/node-v${NODE_VERSION}-linux-${arch}"
  if [[ ! -x "${dir}/bin/node" ]]; then
    tmp="$(mktemp -d)"
    tarball="${tmp}/node.tar.xz"
    log "installing Node.js ${NODE_VERSION}"
    curl -fsSL --retry 3 -o "${tarball}" "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz"
    echo "${sha}  ${tarball}" | sha256sum -c --quiet - || die "Node.js tarball checksum mismatch"
    tar -xJf "${tarball}" -C /opt --no-same-owner
    rm -rf "${tmp}"
  fi
  [[ "$(readlink /opt/node 2>/dev/null)" == "${dir}" ]] || ln -sfn "${dir}" /opt/node
  [[ "$(/opt/node/bin/node --version)" == "v${NODE_VERSION}" ]] || die "/opt/node is not Node.js ${NODE_VERSION}"
}

# certbot_runtime: the pinned certbot in its own venv, rebuilt whenever certbot-requirements.txt changes. Ubuntu's
# certbot packages go once it works; /etc/letsencrypt (account, certificate, renewal config) stays as it is.
certbot_runtime() {
  local dir="/opt/certbot-${CERTBOT_VERSION}" req="${HM_LIB_ROOT}/certbot-requirements.txt" want
  want="$(sha256sum <"${req}" | cut -d' ' -f1)"
  if [[ "$(cat "${dir}/.requirements.sha256" 2>/dev/null)" != "${want}" ]]; then
    log "installing certbot ${CERTBOT_VERSION}"
    rm -rf "${dir}"
    python3 -m venv "${dir}"
    "${dir}/bin/pip" install --quiet --no-cache-dir --disable-pip-version-check --require-hashes -r "${req}"
    echo "${want}" >"${dir}/.requirements.sha256"
  fi
  [[ "$(readlink /opt/certbot 2>/dev/null)" == "${dir}" ]] || ln -sfn "${dir}" /opt/certbot
  ln -sfn /opt/certbot/bin/certbot /usr/local/bin/certbot
  [[ "$(/opt/certbot/bin/certbot --version 2>&1)" == "certbot ${CERTBOT_VERSION}" ]] ||
    die "/opt/certbot is not certbot ${CERTBOT_VERSION}"
  # remove, never purge: purging Ubuntu's certbot deletes /etc/letsencrypt. A backup goes to /var/backups first anyway.
  if dpkg-query -W -f='${Status}' certbot 2>/dev/null | grep -q 'install ok installed'; then
    if [[ -d /etc/letsencrypt ]]; then
      install -d -m 0700 /var/backups/letsencrypt
      tar -C /etc -czf "/var/backups/letsencrypt/letsencrypt-$(date -u +%Y%m%dT%H%M%SZ).tar.gz" letsencrypt
    fi
    log "removing Ubuntu's certbot packages (certbot-renew.timer replaces certbot.timer)"
    apt-get remove -y certbot python3-certbot-dns-cloudflare >/dev/null
    systemctl daemon-reload
  fi
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

install_units() {
  local f changed=0
  for f in "${HM_LIB_ROOT}"/systemd/*; do
    write_if_changed "/etc/systemd/system/$(basename "${f}")" 0644 root:root <"${f}" && changed=1
  done
  if (( changed )); then systemctl daemon-reload; fi
}

units() {
  install_units
  local env
  for env in "$@"; do systemctl enable --quiet --now "headmesh-backup@${env}.timer"; done
  systemctl enable --quiet --now certbot-renew.timer
}

tls() {
  local dir="${HM_ETC_ROOT}/tls" live="/etc/letsencrypt/live/headmesh.dev" before
  before="$(readlink "${dir}/fullchain.pem" 2>/dev/null || true)"
  # The placeholder comes first, so certbot's deploy hook always finds a certificate to copy.
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
  if [[ ! -d "${live}" && -s "${HM_ETC_ROOT}/cloudflare.ini" ]]; then
    if [[ -z "${ACME_EMAIL}" ]]; then
      log "TLS: ${HM_ETC_ROOT}/cloudflare.ini exists but HM_ACME_EMAIL is unset; keeping the placeholder"
    else
      chmod 0600 "${HM_ETC_ROOT}/cloudflare.ini"
      log "TLS: requesting a Let's Encrypt certificate for headmesh.dev and *.headmesh.dev (DNS-01)"
      /opt/certbot/bin/certbot certonly --non-interactive --agree-tos --email "${ACME_EMAIL}" \
        --dns-cloudflare --dns-cloudflare-credentials "${HM_ETC_ROOT}/cloudflare.ini" --dns-cloudflare-propagation-seconds 30 \
        --key-type ecdsa --cert-name headmesh.dev -d headmesh.dev -d '*.headmesh.dev' \
        --deploy-hook "${HM_LIB_ROOT}/deployctl.sh tls-renewed" ||
        log "TLS: certbot failed (see /var/log/letsencrypt/letsencrypt.log); keeping the current certificate"
    fi
  fi
  if [[ -d "${live}" ]]; then
    ln -sfn "${live}/fullchain.pem" "${dir}/fullchain.pem"
    ln -sfn "${live}/privkey.pem" "${dir}/privkey.pem"
    log "TLS: using the Let's Encrypt certificate (renewed by certbot-renew.timer)"
  fi
  # A switch from the placeholder reaches nginx, the backends and the cell agent once nginx_base has run.
  if [[ -n "${before}" && "$(readlink "${dir}/fullchain.pem")" != "${before}" ]]; then TLS_CHANGED=1; fi
}

# public_address <4|6>: this server's first global address of that family, leaving out tailnet and WireGuard links.
public_address() {
  { ip -o "-$1" addr show scope global 2>/dev/null || true; } |
    awk '$2 !~ /^(tailscale|wg|hm|docker|br-|veth)/ {print $4}' | cut -d/ -f1 |
    { grep -vE '^(100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|fd7a:)' || true; } | head -n 1
}

# headscale_backup_before_upgrade: an online copy of Headscale's SQLite database before a package change, because a
# new release may rewrite stored data at startup (0.29.7 trims hostnames, endpoints and routes) and rolling back
# means reinstalling the old package over this copy. Keeps the newest five; a fresh install has nothing to copy.
readonly HEADSCALE_DB=/var/lib/headscale/db.sqlite
readonly HEADSCALE_BACKUPS=/var/lib/headscale/backups
headscale_backup_before_upgrade() {
  local old bak
  old="$(dpkg-query -W -f='${Version}' headscale 2>/dev/null || true)"
  [[ -n "${old}" && -f "${HEADSCALE_DB}" ]] || return 0
  install -d -o headscale -g headscale -m 0700 "${HEADSCALE_BACKUPS}"
  bak="${HEADSCALE_BACKUPS}/db-${old}-$(date -u +%Y%m%dT%H%M%SZ).sqlite"
  sqlite3 "${HEADSCALE_DB}" ".backup '${bak}'" || die "could not back up Headscale's database before upgrading"
  [[ "$(sqlite3 "${bak}" 'PRAGMA integrity_check;')" == "ok" ]] || die "Headscale's database backup ${bak} is corrupt"
  chown headscale:headscale "${bak}"
  chmod 0600 "${bak}"
  find "${HEADSCALE_BACKUPS}" -maxdepth 1 -name 'db-*.sqlite' -printf '%T@ %p\n' | sort -rn | tail -n +6 |
    cut -d' ' -f2- | xargs -r rm -f --
  log "backed up Headscale ${old}'s database to ${bak}"
}

headscale_package() {
  local arch sha tmp deb
  arch="$(dpkg --print-architecture)"
  case "${arch}" in
    amd64) sha="${HEADSCALE_DEB_SHA256}" ;;
    arm64) sha="${HEADSCALE_ARM64_DEB_SHA256}" ;;
    *) die 'Headscale installation supports amd64 and arm64' ;;
  esac
  if ! dpkg-query -W -f='${Version}' headscale 2>/dev/null | grep -qx "${HEADSCALE_VERSION}"; then
    headscale_backup_before_upgrade
    tmp="$(mktemp -d)"
    deb="${tmp}/headscale.deb"
    log "installing Headscale ${HEADSCALE_VERSION}"
    curl -fsSL --retry 3 -o "${deb}" \
      "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${arch}.deb"
    echo "${sha}  ${deb}" | sha256sum -c --quiet - || die "Headscale package checksum mismatch"
    # --force-confold: an upgrade keeps the config.yaml rendered below instead of stopping at dpkg's prompt.
    apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold "${deb}" >/dev/null
    rm -rf "${tmp}"
  fi
}

# headscale_cell <env>: Headscale for the cell the environment hosts, configured from deploy/headscale/config.yaml.tmpl.
# Only the cell's name in nginx is public; the gRPC API stays on loopback and the unix socket (plan §9.4).
headscale_cell() {
  local env="$1" host base v4 v6
  host="$(hm_env "${env}" CELL_HOST)"
  base="$(hm_env "${env}" CELL_BASE_DOMAIN)"
  headscale_package
  v4="$(public_address 4)"
  v6="$(public_address 6)"
  [[ -n "${v4}" ]] || die "no public IPv4 address for the embedded DERP relay"
  local s
  s="$(<"${HM_LIB_ROOT}/headscale/config.yaml.tmpl")"
  s="${s//\{\{CELL_HOST\}\}/${host}}"
  s="${s//\{\{CELL_BASE_DOMAIN\}\}/${base}}"
  s="${s//\{\{PUBLIC_IPV4\}\}/${v4}}"
  if [[ -n "${v6}" ]]; then
    s="${s//\{\{PUBLIC_IPV6\}\}/${v6}}"
  else
    s="$(sed '/{{PUBLIC_IPV6}}/d' <<<"${s}")"
  fi
  # The unit's StateDirectory= would fix this on first start; configtest runs before that and writes its keys here.
  install -d -o headscale -g headscale -m 0750 /var/lib/headscale
  if printf '%s\n' "${s}" | write_if_changed /etc/headscale/config.yaml 0640 root:headscale; then
    runuser -u headscale -- headscale configtest -c /etc/headscale/config.yaml >/dev/null 2>&1 ||
      die "Headscale rejected /etc/headscale/config.yaml"
    systemctl restart headscale
  fi
  systemctl enable --quiet --now headscale
  for _ in $(seq 1 30); do curl -fs -o /dev/null http://127.0.0.1:8080/health && return 0; sleep 1; done
  die "Headscale did not become healthy"
}

prepare_recovery() {
  [[ $# == 0 ]] || die 'usage: bootstrap.sh prepare-recovery'
  preflight_host
  umask 077
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  local dir unit clusters
  for dir in "${HM_STATE_ROOT}/recovery" "/var/lib/postgresql/${PG_MAJOR}/main" "/etc/postgresql/${PG_MAJOR}/main"; do
    [[ ! -e "${dir}" && ! -L "${dir}" ]] || die "standby preparation requires an empty destination: ${dir}"
  done
  if [[ -e /var/lib/headscale || -L /var/lib/headscale ]]; then
    [[ -d /var/lib/headscale && ! -L /var/lib/headscale && -z "$(find /var/lib/headscale -mindepth 1 -print -quit)" ]] ||
      die 'Headscale state already exists on this host'
  fi
  if command -v pg_lsclusters >/dev/null; then
    clusters="$(pg_lsclusters --no-header)"
    [[ -z "${clusters}" ]] || die 'a PostgreSQL cluster already exists on this host'
  fi
  for unit in nginx.service headscale.service headmesh-cellagent.service headmesh-backend@prod-{blue,green}.service \
    headmesh-waitlist@prod-{blue,green}.service certbot-renew.timer headmesh-backup@prod.timer; do
    [[ "$(systemctl show -p ActiveState --value "${unit}")" == inactive ]] || die "${unit} must be inactive on the standby"
  done

  # Package maintainer scripts may attempt to start nginx and Headscale. Hold
  # them before installation and leave the masks in place across retries.
  systemctl mask --quiet nginx.service headscale.service
  install_packages postgresql-common
  pg_conftool /etc/postgresql-common/createcluster.conf set create_main_cluster false
  packages age jq locales pgbackrest
  locale-gen en_US.UTF-8 >/dev/null
  [[ "$(LC_ALL=en_US.UTF-8 locale charmap)" == UTF-8 ]] || die 'the PostgreSQL production locale is unavailable'
  clusters="$(pg_lsclusters --no-header)"
  [[ -z "${clusters}" ]] || die 'package installation created an unexpected PostgreSQL cluster'
  headscale_package
  node_runtime
  certbot_runtime
  service_user
  state_dirs prod
  install -d -m 0750 -o root -g "${HM_SERVICE_USER}" "${HM_ETC_ROOT}/prod"
  if ! id -u headmesh-cellagent >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /var/lib/headmesh-cellagent --shell /usr/sbin/nologin headmesh-cellagent
  fi
  install -d -m 0750 -o root -g headmesh-cellagent /etc/headmesh-cellagent
  install_units
  for unit in nginx.service headscale.service; do
    [[ "$(readlink "/etc/systemd/system/${unit}" || true)" == /dev/null &&
      "$(systemctl show -p ActiveState --value "${unit}")" == inactive ]] || die "${unit} lost its standby mask"
  done
  log 'standby runtime prepared; restore verified state and credentials before starting services'
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
  certbot_runtime
  service_user
  postgres
  local env
  for env in "$@"; do
    database "${env}"
    state_dirs "${env}"
  done
  units "$@"
  for env in "$@"; do
    if [[ -n "$(hm_env "${env}" CELL_NAME)" ]]; then headscale_cell "${env}"; fi
  done
  tls
  nginx_base
  if [[ ${TLS_CHANGED} == 1 ]]; then "${HM_LIB_ROOT}/deployctl.sh" tls-renewed; fi
  log "bootstrap complete for: $*"
}

case "${1:-}" in
  prepare-recovery) shift; prepare_recovery "$@" ;;
  restore) shift; exec "${HERE}/install-recovery-state.sh" "$@" ;;
  *) main "$@" ;;
esac
