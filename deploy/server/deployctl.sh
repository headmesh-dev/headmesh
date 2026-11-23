#!/usr/bin/env bash
# deployctl: the server-side half of deploy/deploy.sh. Runs as root on hm-prod-1; deploy.sh uploads it (with
# lib/environments.sh) to /usr/local/lib/headmesh/ on every run, so the server always runs the matching version.
#
#   receive          <dir>                             unpack a tar stream from stdin into a release or staging dir
#   clear-staging    <release>                         delete a deploy's staging files (env files are shredded)
#   install-env      <env> <staging-dir>               env files from the workstation, always overwritten
#   nginx-apply      <env> <staging-dir> <overwrite>   install rendered nginx config; nginx -t before any reload
#   nginx-diff       <env> <staging-dir>               show what nginx-apply would change, change nothing
#   activate-static  <env> <release>                   atomically switch the marketing site to a release
#   activate-service <service> <env> <release> [drain] blue-green switch of waitlist or backend
#   start-recovery   <release>                        start a restored prod backend and cell behind closed ingress
#   install-recovery-runtime <release> <identity> <-bundle|-netrc> <file>  install encrypted standby configuration
#   start-recovery-waitlist <release> <snapshot>        install a waitlist snapshot and start the private service
#   prepare-recovery-ingress <nginx-dir> <site-release> <ipv4> <ipv6>  check standby HTTPS and publish its DERP/STUN
#   finalize-recovery                                  adopt the recovered releases and persist startup
#   verify-recovery-cell                               verify the recovered cell using a client receipt on stdin
#   rollback         <component> <env> [keep-schema]   back to the previous release (marketing) or colour (services);
#                                                      the backend's schema follows to what that colour ran with
#   status           [env...]                          releases, colours, units and health
#   prune            <env> <keep>                      delete old releases (never the live or rollback ones)
#   backup           <env>                             copy the waitlist database (also run daily by a timer)
#                                                      (Postgres is dumped before every schema change instead)
#   publish-manifest <netrc>                           publish recovery evidence using the live prod release
#   publish-waitlist <netrc>                           publish the encrypted waitlist snapshot using the live prod release
#   smoke            <env>                             request each vhost through nginx on loopback
#   cell-agent       <env>                             register the env's Headscale cell, renew the agent's
#                                                      certificate, configure and (re)start the cell agent
#   tls-renewed                                        certbot deploy hook: refresh backend copies, reload
#
# Every nginx change follows the same rule: write the new files, run `nginx -t`, and reload only if it passes.
# If it fails, the previous files are restored and nginx keeps serving the old configuration untouched.
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "${HERE}/environments.sh" ]]; then
  # shellcheck source=../lib/environments.sh
  source "${HERE}/environments.sh"
else
  # shellcheck source=../lib/environments.sh
  source "${HERE}/../lib/environments.sh"
fi

# Tests run nginx without systemd; they override how to reload.
NGINX_RELOAD="${HM_NGINX_RELOAD:-systemctl reload nginx}"
HEALTH_TIMEOUT="${HM_HEALTH_TIMEOUT:-45}"
NGINX_BACKUPS="${HM_STATE_ROOT}/nginx-backups"
STAGING_ROOT="${HM_STATE_ROOT}/staging"

log() { printf '[deployctl] %s\n' "$*" >&2; }
die() { printf '[deployctl] ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

require_env() { hm_is_env "$1" || die "unknown environment '$1' (expected: ${HM_ENVS[*]})"; }
require_service() {
  case "$1" in waitlist | backend) ;; *) die "unknown service '$1' (expected: ${HM_SERVICES[*]})" ;; esac
}
require_release() { [[ "$1" =~ ^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{7,40}(-dirty)?$ ]] || die "bad release id '$1'"; }

state_dir() { echo "${HM_STATE_ROOT}/$1/state"; }
state_get() { cat "$(state_dir "$1")/$2" 2>/dev/null || true; }
state_set() {
  local dir
  dir="$(state_dir "$1")"
  install -d -m 0750 "${dir}"
  printf '%s\n' "$3" >"${dir}/$2.tmp"
  mv -f "${dir}/$2.tmp" "${dir}/$2"
}

# ── nginx ──────────────────────────────────────────────────────────────────────────────────────────────────────

nginx_check() {
  local out
  if out="$(nginx -t 2>&1)"; then return 0; fi
  printf '%s\n' "${out}" >&2
  return 1
}

nginx_reload() {
  # shellcheck disable=SC2086 # NGINX_RELOAD is a command line
  ${NGINX_RELOAD}
}

# A transaction over nginx files: stage changes with nginx_txn_put, then nginx_txn_commit runs nginx -t and either
# reloads or puts every touched file back the way it was.
NGINX_TXN_DIR=""
NGINX_TXN_FILES=()
nginx_txn_begin() {
  NGINX_TXN_DIR="${NGINX_BACKUPS}/$(date -u +%Y%m%dT%H%M%S)-$$"
  NGINX_TXN_FILES=()
  install -d -m 0700 "${NGINX_TXN_DIR}"
}

# nginx_txn_remember <path>: save how <path> looks now (a file, a symlink, or absent) so the transaction can undo.
nginx_txn_remember() {
  local dst="$1" backup="${NGINX_TXN_DIR}/files${1}"
  install -d -m 0755 "$(dirname "${backup}")" "$(dirname "${dst}")"
  if [[ -e "${dst}" || -L "${dst}" ]]; then
    cp -a "${dst}" "${backup}"
  else
    : >"${backup}.absent"
  fi
  NGINX_TXN_FILES+=("${dst}")
}

# nginx_txn_put <source> <target> [mode]: install a file at target, atomically.
nginx_txn_put() {
  local src="$1" dst="$2" mode="${3:-0644}"
  nginx_txn_remember "${dst}"
  install -m "${mode}" "${src}" "${dst}.headmesh-new"
  mv -Tf "${dst}.headmesh-new" "${dst}"
}

# nginx_txn_link <target> <link>: point the symlink <link> at <target>, atomically (rename over the old link).
nginx_txn_link() {
  local target="$1" link="$2"
  nginx_txn_remember "${link}"
  ln -sfn "${target}" "${link}.headmesh-new"
  mv -Tf "${link}.headmesh-new" "${link}"
}

nginx_txn_restore() {
  local dst backup
  for dst in "${NGINX_TXN_FILES[@]}"; do
    backup="${NGINX_TXN_DIR}/files${dst}"
    rm -f "${dst}"
    [[ -e "${backup}.absent" ]] || cp -a "${backup}" "${dst}"
  done
}

# Returns non-zero (after restoring) when nginx -t fails, so callers can undo their own side effects.
nginx_txn_commit() {
  local what="$1"
  if ! nginx_check; then
    log "nginx -t failed for ${what}; restoring the previous configuration and NOT reloading"
    nginx_txn_restore
    nginx_check || log "WARNING: the restored configuration doesn't pass nginx -t either; nginx keeps its running config"
    return 1
  fi
  nginx_reload
  log "nginx reloaded (${what})"
  # Keep the last 20 transaction backups.
  find "${NGINX_BACKUPS}" -mindepth 1 -maxdepth 1 -type d | sort | head -n -20 | xargs -r rm -rf
}

upstream_file() { echo "${HM_NGINX_ROOT}/$1/$2-upstream.conf"; }

# render_upstream <env> <service> <colour> <out-dir>: write the upstream block(s) pointing at one colour.
render_upstream() {
  local env="$1" service="$2" colour="$3" out="$4"
  printf '# Managed by deployctl: %s %s is live on the %s instance.\nupstream headmesh_%s_%s {\n    server 127.0.0.1:%s;\n}\n' \
    "${env}" "${service}" "${colour}" "${env}" "${service}" "$(hm_port "${env}" "${service}" "${colour}")" \
    >"${out}/${service}-upstream.conf"
  if [[ "${service}" == backend ]]; then
    printf '# Managed by deployctl: %s cell endpoint is live on the %s instance.\nupstream headmesh_%s_cell {\n    server 127.0.0.1:%s;\n}\n' \
      "${env}" "${colour}" "${env}" "$(hm_port "${env}" backend "${colour}" cell)" >"${out}/cell-upstream.conf"
  fi
}

# Upstream files must exist before nginx can load a vhost that references them. Point new ones at blue.
ensure_upstreams() {
  local env="$1" service tmp
  tmp="$(mktemp -d)"
  local colour
  for service in "${HM_SERVICES[@]}"; do
    colour="$(state_get "${env}" "${service}.active")"
    render_upstream "${env}" "${service}" "${colour:-blue}" "${tmp}"
  done
  install -d -m 0755 "${HM_NGINX_ROOT}/${env}"
  for service in waitlist backend cell; do
    [[ -f "$(upstream_file "${env}" "${service}")" ]] || install -m 0644 "${tmp}/${service}-upstream.conf" "$(upstream_file "${env}" "${service}")"
  done
  rm -rf "${tmp}"
}

# Rendered file → installed path. Generated data (the Cloudflare ranges) is always replaced. Everything else is
# configuration: installed when missing, replaced on drift only with --nginx-overwrite.
nginx_files() {
  local env="$1"
  NGINX_GENERATED=("cloudflare-realip.conf:${HM_NGINX_ROOT}/cloudflare-realip.conf")
  NGINX_MANAGED=(
    "headmesh.conf:/etc/nginx/conf.d/headmesh.conf"
    "snippets/headmesh-tls.conf:/etc/nginx/snippets/headmesh-tls.conf"
    "snippets/headmesh-proxy.conf:/etc/nginx/snippets/headmesh-proxy.conf"
    "stream-include.conf:/etc/nginx/modules-enabled/99-headmesh-stream.conf"
    "sites/headmesh-${env}.conf:/etc/nginx/sites-available/headmesh-${env}.conf"
    "stream/${env}.conf:${HM_NGINX_ROOT}/${env}/stream.conf"
  )
}

cmd_nginx_diff() {
  local env="$1" staging="$2" pair src dst same=1
  require_env "${env}"
  [[ -d "${staging}" ]] || die "no staging directory ${staging}"
  nginx_files "${env}"
  for pair in "${NGINX_GENERATED[@]}" "${NGINX_MANAGED[@]}"; do
    src="${staging}/${pair%%:*}"
    dst="${pair#*:}"
    if [[ ! -f "${dst}" ]]; then
      log "nginx ${env}: would install ${dst}"
      same=0
    elif ! cmp -s "${src}" "${dst}"; then
      log "nginx ${env}: ${dst} differs from the local render:"
      diff -u "${dst}" "${src}" | head -n 60 >&2 || true
      same=0
    fi
  done
  [[ "${same}" == 1 ]] && log "nginx ${env}: the server matches the local render"
  return 0
}

cmd_nginx_apply() {
  local env="$1" staging="$2" overwrite="${3:-0}" src dst changed=0 drift=0
  require_env "${env}"
  [[ -d "${staging}" ]] || die "no staging directory ${staging}"
  nginx_files "${env}"
  local -a generated=("${NGINX_GENERATED[@]}") managed=("${NGINX_MANAGED[@]}")

  ensure_upstreams "${env}"
  nginx_txn_begin
  local pair
  for pair in "${generated[@]}" "${managed[@]}"; do
    src="${staging}/${pair%%:*}"
    dst="${pair#*:}"
    [[ -f "${src}" ]] || die "missing rendered file ${pair%%:*}"
    if [[ -f "${dst}" ]] && cmp -s "${src}" "${dst}"; then continue; fi
    if [[ -f "${dst}" && "${overwrite}" != 1 && " ${generated[*]} " != *" ${pair} "* ]]; then
      log "drift: ${dst} differs from the local render; keeping the server's copy (use --nginx-overwrite to replace it)"
      diff -u "${dst}" "${src}" | head -n 40 >&2 || true
      drift=1
      continue
    fi
    nginx_txn_put "${src}" "${dst}"
    log "installed ${dst}"
    changed=1
  done

  local enabled="/etc/nginx/sites-enabled/headmesh-${env}.conf"
  if [[ ! -L "${enabled}" ]]; then
    nginx_txn_link "/etc/nginx/sites-available/headmesh-${env}.conf" "${enabled}"
    changed=1
  fi

  if [[ "${changed}" == 0 ]]; then
    log "nginx config for ${env} already up to date$([[ ${drift} == 1 ]] && echo ' (with drift, see above)')"
    return 0
  fi
  nginx_txn_commit "config for ${env}" || die "nginx config for ${env} rejected by nginx -t; nothing was reloaded"
}

# ── uploads ────────────────────────────────────────────────────────────────────────────────────────────────────

# Uploads may only land in a new release directory or this deploy's staging area.
cmd_receive() {
  local dir="$1" tmp
  if [[ "${dir}" =~ ^${HM_SRV_ROOT}/(prod|test)/(marketing|waitlist|backend)/releases/[0-9]{8}T[0-9]{6}Z-[0-9a-f]{7,40}(-dirty)?$ ]]; then
    [[ ! -e "${dir}" ]] || die "release directory ${dir} already exists; releases are immutable"
    install -d -m 0755 "$(dirname "${dir}")"
    tmp="$(mktemp -d "$(dirname "${dir}")/.incoming.XXXXXX")"
    tar -xf - -C "${tmp}" --no-same-owner
    # Release files are code and static assets: root-owned, readable by the service user and by nginx.
    chown -R root:root "${tmp}"
    chmod -R u=rwX,go=rX "${tmp}"
    mv "${tmp}" "${dir}"
  elif [[ "${dir}" =~ ^${STAGING_ROOT}/[0-9]{8}T[0-9]{6}Z-[0-9a-f]{7,40}(-dirty)?/(prod|test)/(env|nginx)$ ]]; then
    install -d -m 0700 "${STAGING_ROOT}" "$(dirname "$(dirname "${dir}")")" "$(dirname "${dir}")"
    rm -rf "${dir}"
    install -d -m 0700 "${dir}"
    tar -xf - -C "${dir}" --no-same-owner
    chown -R root:root "${dir}"
  else
    die "refusing to unpack into ${dir}"
  fi
}

cmd_clear_staging() {
  local release="$1" dir
  require_release "${release}"
  dir="${STAGING_ROOT}/${release}"
  [[ -d "${dir}" ]] || return 0
  find "${dir}" -type f -name '*.env' -exec shred -u {} +
  rm -rf "${dir}"
}

# ── env files ──────────────────────────────────────────────────────────────────────────────────────────────────

validate_env_file() {
  local file="$1" line n=0
  while IFS= read -r line || [[ -n "${line}" ]]; do
    n=$((n + 1))
    [[ "${line}" == *$'\r'* ]] && die "${file}:${n}: Windows line ending"
    [[ -z "${line}" || "${line}" =~ ^[[:space:]]*# ]] && continue
    [[ "${line}" =~ ^[A-Z][A-Z0-9_]*=.*$ ]] || die "${file}:${n}: expected KEY=value"
  done <"${file}"
}

cmd_install_env() {
  local env="$1" staging="$2" name installed=0
  require_env "${env}"
  install -d -o root -g "${HM_SERVICE_USER}" -m 0750 "${HM_ETC_ROOT}/${env}"
  for name in waitlist.env backend.env; do
    [[ -f "${staging}/${name}" ]] || continue
    validate_env_file "${staging}/${name}"
    install -o root -g "${HM_SERVICE_USER}" -m 0640 "${staging}/${name}" "${HM_ETC_ROOT}/${env}/${name}"
    shred -u "${staging}/${name}"
    log "env: ${HM_ETC_ROOT}/${env}/${name} replaced"
    installed=1
  done
  [[ "${installed}" == 1 ]] || log "env: nothing to install for ${env}"
}

# ── static marketing site ──────────────────────────────────────────────────────────────────────────────────────

cmd_activate_static() {
  local env="$1" release="$2" root target previous
  require_env "${env}"
  require_release "${release}"
  root="${HM_SRV_ROOT}/${env}/marketing"
  target="${root}/releases/${release}"
  [[ -f "${target}/site/index.html" ]] || die "release ${release} has no site/index.html"
  [[ -f "${target}/nginx/security-headers.conf" ]] || die "release ${release} has no nginx/security-headers.conf"

  previous="$(readlink "${root}/current" 2>/dev/null || true)"
  if [[ "${previous}" == "releases/${release}" ]]; then
    log "marketing ${env}: ${release} is already live"
    return 0
  fi

  nginx_txn_begin
  nginx_txn_link "releases/${release}" "${root}/current"
  if ! nginx_txn_commit "marketing ${env} → ${release}"; then
    die "marketing ${env}: release ${release} rejected by nginx -t; ${previous:-nothing} stays live"
  fi
  [[ -n "${previous}" ]] && state_set "${env}" marketing.previous "${previous#releases/}"
  state_set "${env}" marketing.active "${release}"
  log "marketing ${env}: ${release} is live"
}

# ── blue-green services ────────────────────────────────────────────────────────────────────────────────────────

unit_name() { echo "headmesh-$1@$2-$3.service"; }
slot_path() { echo "${HM_SRV_ROOT}/slots/$1-$2-$3"; }

health_url() {
  local env="$1" service="$2" colour="$3"
  echo "http://127.0.0.1:$(hm_port "${env}" "${service}" "${colour}")/readyz"
}

wait_healthy() {
  local url="$1" deadline=$((SECONDS + HEALTH_TIMEOUT))
  while ((SECONDS < deadline)); do
    if curl -fs -o /dev/null --max-time 3 "${url}" 2>/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

# write_instance_env <service> <env> <colour> [recovery]: the env file the instance reads.
write_instance_env() {
  local service="$1" env="$2" colour="$3" out tmp
  out="${HM_ETC_ROOT}/instances/${service}-${env}-${colour}.env"
  tmp="$(mktemp)"
  {
    echo "# Generated by deployctl for $(unit_name "${service}" "${env}" "${colour}"). Do not edit; deploy.sh"
    echo "# replaces ${HM_ETC_ROOT}/${env}/${service}.env and this file is rebuilt on every switch."
    if [[ "${service}" == backend ]]; then
      # db.env holds only the app role's URL; the schema owner's (db-owner.env) is for migrations alone.
      cat "${HM_ETC_ROOT}/${env}/secrets.env" "${HM_ETC_ROOT}/${env}/db.env"
    fi
    cat "${HM_ETC_ROOT}/${env}/${service}.env"
    if [[ "${service}" == waitlist ]]; then
      echo "WAITLIST_HOST=127.0.0.1"
      echo "WAITLIST_PORT=$(hm_port "${env}" waitlist "${colour}")"
      echo "WAITLIST_DB_PATH=${HM_STATE_ROOT}/${env}/waitlist/waitlist.db"
    else
      echo "HEADMESH_LISTEN=127.0.0.1:$(hm_port "${env}" backend "${colour}")"
      echo "HEADMESH_CELL_LISTEN=127.0.0.1:$(hm_port "${env}" backend "${colour}" cell)"
      echo "HEADMESH_TRUST_FORWARDED_FOR=true"
      if [[ "${4:-}" == recovery ]]; then echo "HEADMESH_RECOVERY=true"; fi
    fi
  } >"${tmp}"
  install -d -o root -g "${HM_SERVICE_USER}" -m 0750 "${HM_ETC_ROOT}/instances"
  install -o root -g "${HM_SERVICE_USER}" -m 0640 "${tmp}" "${out}"
  rm -f "${tmp}"
}

# First backend deploy of an environment: create the signing key, the cell CA and the backend's TLS copies, and
# record their paths in secrets.env. Never overwrites existing material.
ensure_backend_secrets() {
  local env="$1" binary="$2" dir="${HM_ETC_ROOT}/$1/backend"
  [[ -f "${HM_ETC_ROOT}/${env}/db.env" ]] || die "${HM_ETC_ROOT}/${env}/db.env is missing; run deploy.sh --bootstrap first"
  install -d -o root -g "${HM_SERVICE_USER}" -m 0750 "${dir}"
  if [[ ! -f "${dir}/op-signing.key" ]]; then
    "${binary}" keygen -out "${dir}/op-signing.key"
    chown root:"${HM_SERVICE_USER}" "${dir}/op-signing.key"
    chmod 0640 "${dir}/op-signing.key"
    log "backend ${env}: created the operation-signing key"
  fi
  if [[ ! -f "${dir}/cell-ca/ca.crt" ]]; then
    install -d -o root -g root -m 0700 "${dir}/cell-ca"
    "${binary}" ca-init -dir "${dir}/cell-ca"
    chmod 0755 "${dir}/cell-ca"
    log "backend ${env}: created the cell CA (move ${dir}/cell-ca/ca.key offline once cells have certificates)"
  fi
  refresh_backend_tls
  if [[ ! -f "${HM_ETC_ROOT}/${env}/secrets.env" ]]; then
    install -o root -g "${HM_SERVICE_USER}" -m 0640 /dev/stdin "${HM_ETC_ROOT}/${env}/secrets.env" <<EOF
# Generated by deployctl on the first backend deploy; never overwritten.
HEADMESH_OP_SIGNING_KEY=${dir}/op-signing.key
HEADMESH_CELL_CA=${dir}/cell-ca/ca.crt
HEADMESH_TLS_CERT=${HM_ETC_ROOT}/tls/backend/fullchain.pem
HEADMESH_TLS_KEY=${HM_ETC_ROOT}/tls/backend/privkey.pem
EOF
  fi
}

# The backend runs unprivileged and can't read the root-only certificate key, so it gets a group-readable copy.
refresh_backend_tls() {
  install -d -o root -g "${HM_SERVICE_USER}" -m 0750 "${HM_ETC_ROOT}/tls/backend"
  install -o root -g "${HM_SERVICE_USER}" -m 0644 "$(readlink -f "${HM_ETC_ROOT}/tls/fullchain.pem")" "${HM_ETC_ROOT}/tls/backend/fullchain.pem"
  install -o root -g "${HM_SERVICE_USER}" -m 0640 "$(readlink -f "${HM_ETC_ROOT}/tls/privkey.pem")" "${HM_ETC_ROOT}/tls/backend/privkey.pem"
}

# Migrations run as the schema owner. Afterwards the app role gets exactly what the integration tests give it:
# read and write on every table except the append-only audit log, which it can only read and insert into.
migrate_as_owner() {
  local env="$1" binary="$2" db="headmesh_$1" app="headmesh_$1_app"
  shift 2
  [[ -f "${HM_ETC_ROOT}/${env}/db-owner.env" ]] || die "${HM_ETC_ROOT}/${env}/db-owner.env is missing; run deploy.sh --bootstrap first"
  systemd-run --quiet --pipe --wait --collect \
    -p User="${HM_SERVICE_USER}" -p EnvironmentFile="${HM_ETC_ROOT}/${env}/db-owner.env" \
    "${binary}" migrate "$@"
  runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -d "${db}" <<SQL
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ${app};
REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM ${app};
REVOKE ALL ON goose_db_version FROM ${app};
SQL
}

# schema_version <env>: the newest applied migration in the env's database.
schema_version() {
  psql_env "$1" -c "SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied"
}

# run_migrations <env> <binary> <colour>: dump the database, apply the release's pending migrations, and record the
# schema version the colour runs with, so a rollback to that colour can put the schema back.
run_migrations() {
  local env="$1" binary="$2" colour="$3"
  pg_backup "${env}" deploy
  log "backend ${env}: applying database migrations"
  migrate_as_owner "${env}" "${binary}"
  state_set "${env}" "backend-${colour}.schema" "$(schema_version "${env}")"
  log "backend ${env}: schema at migration $(state_get "${env}" "backend-${colour}.schema") for ${colour}"
}

# revert_schema <env> <binary> <version>: dump the database, then revert migrations newer than <version> with the
# release that added them (an older release doesn't embed them, so it can't).
revert_schema() {
  local env="$1" binary="$2" version="$3" now
  pg_backup "${env}" rollback
  log "backend ${env}: reverting the schema from migration $(schema_version "${env}") to ${version}"
  migrate_as_owner "${env}" "${binary}" -down-to "${version}"
  now="$(schema_version "${env}")"
  # Releases from before schema rollback ignore -down-to and only apply pending migrations, which changes nothing.
  [[ "${now}" == "${version}" ]] ||
    die "backend ${env}: the schema is still at migration ${now}, not ${version}; ${binary} can't revert it. The service rollback is done; restore the dump above or revert by hand"
}

# pg_backup <env> <label>: a custom-format dump of the env's Postgres database (pg_restore reads it), taken before
# every schema change. Root-only, because it holds every tenant's data; the newest 14 are kept.
pg_backup() {
  local env="$1" label="$2" dir out
  dir="${HM_STATE_ROOT}/${env}/backups"
  install -d -o "${HM_SERVICE_USER}" -g "${HM_SERVICE_USER}" -m 0750 "${dir}"
  out="${dir}/headmesh_${env}-$(date -u +%Y%m%dT%H%M%SZ)-${label}.dump"
  (umask 077 && runuser -u postgres -- pg_dump -Fc -d "headmesh_${env}" >"${out}")
  find "${dir}" -name "headmesh_${env}-*.dump" -type f | sort | head -n -14 | xargs -r rm -f
  log "backend ${env}: database dumped to ${out}"
}

cmd_backup() {
  local env="$1" db dir stamp
  require_env "${env}"
  db="${HM_STATE_ROOT}/${env}/waitlist/waitlist.db"
  [[ -f "${db}" ]] || { log "backup ${env}: no waitlist database yet"; return 0; }
  dir="${HM_STATE_ROOT}/${env}/backups"
  install -d -o "${HM_SERVICE_USER}" -g "${HM_SERVICE_USER}" -m 0750 "${dir}"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  sqlite3 "${db}" ".backup '${dir}/waitlist-${stamp}.db'"
  chown "${HM_SERVICE_USER}:${HM_SERVICE_USER}" "${dir}/waitlist-${stamp}.db"
  chmod 0640 "${dir}/waitlist-${stamp}.db"
  find "${dir}" -name 'waitlist-*.db' -type f | sort | head -n -14 | xargs -r rm -f
  log "backup ${env}: ${dir}/waitlist-${stamp}.db"
}

cmd_publish_backup() {
  local component="$1" netrc="${2:-}" colour
  [[ -n "${netrc}" ]] || die "publish-${component} needs a credential file"
  colour="$(state_get prod backend.active)"
  [[ "${colour}" == blue || "${colour}" == green ]] || die "no active prod backend for ${component} publication"
  exec "$(slot_path backend prod "${colour}")/headmesh-server" "backup-${component}" publish -netrc "${netrc}"
}

# switch_to <service> <env> <colour> <release-for-log>: point nginx at a healthy colour (nginx -t gated).
switch_to() {
  local service="$1" env="$2" colour="$3" release="$4" tmp
  tmp="$(mktemp -d)"
  render_upstream "${env}" "${service}" "${colour}" "${tmp}"
  nginx_txn_begin
  nginx_txn_put "${tmp}/${service}-upstream.conf" "$(upstream_file "${env}" "${service}")"
  [[ "${service}" == backend ]] && nginx_txn_put "${tmp}/cell-upstream.conf" "$(upstream_file "${env}" cell)"
  rm -rf "${tmp}"
  nginx_txn_commit "${service} ${env} → ${colour} (${release})"
}

cmd_activate_service() {
  local service="$1" env="$2" release="$3" drain="${4:-10}" root target active idle unit
  require_service "${service}"
  require_env "${env}"
  require_release "${release}"
  root="${HM_SRV_ROOT}/${env}/${service}"
  target="${root}/releases/${release}"
  case "${service}" in
    waitlist) [[ -f "${target}/server/main.ts" ]] || die "release ${release} has no server/main.ts" ;;
    backend) [[ -x "${target}/headmesh-server" ]] || die "release ${release} has no executable headmesh-server" ;;
  esac
  [[ -f "${HM_ETC_ROOT}/${env}/${service}.env" ]] || die "${HM_ETC_ROOT}/${env}/${service}.env is missing (deploy.sh uploads it)"

  active="$(state_get "${env}" "${service}.active")"
  idle="$(hm_other_colour "${active}")"
  unit="$(unit_name "${service}" "${env}" "${idle}")"
  log "${service} ${env}: live colour ${active:-none}, starting ${release} on ${idle}"

  if [[ "${service}" == waitlist ]]; then
    install -d -o "${HM_SERVICE_USER}" -g "${HM_SERVICE_USER}" -m 0750 "${HM_STATE_ROOT}/${env}/waitlist"
    cmd_backup "${env}"
  else
    ensure_backend_secrets "${env}" "${target}/headmesh-server"
    run_migrations "${env}" "${target}/headmesh-server" "${idle}"
  fi

  install -d -m 0755 "${HM_SRV_ROOT}/slots"
  ln -sfn "${target}" "$(slot_path "${service}" "${env}" "${idle}")"
  write_instance_env "${service}" "${env}" "${idle}"
  systemctl restart "${unit}"

  if ! wait_healthy "$(health_url "${env}" "${service}" "${idle}")"; then
    journalctl -u "${unit}" -n 40 --no-pager >&2 || true
    systemctl stop "${unit}" || true
    die "${service} ${env}: ${idle} (${release}) never became ready; ${active:-nothing} stays live"
  fi
  log "${service} ${env}: ${idle} is ready"

  if ! switch_to "${service}" "${env}" "${idle}" "${release}"; then
    systemctl stop "${unit}" || true
    die "${service} ${env}: nginx rejected the switch to ${idle}; ${active:-nothing} stays live"
  fi

  [[ -n "${active}" ]] && state_set "${env}" "${service}.previous" "${active}"
  state_set "${env}" "${service}.active" "${idle}"
  systemctl enable --quiet "${unit}"
  if [[ -n "${active}" ]]; then
    log "${service} ${env}: draining ${active} for ${drain}s"
    sleep "${drain}"
    systemctl disable --quiet --now "$(unit_name "${service}" "${env}" "${active}")" || true
  fi
  log "${service} ${env}: ${release} is live on ${idle}"
}

# ── standby startup ──────────────────────────────────────────────────────────────────────────────────────────────────

recovery_http_code() { curl -qsS --noproxy '*' --max-time 3 -o /dev/null -w '%{http_code}' "$@"; }

recovery_noise_hash() {
  curl -qfsS --noproxy '*' --max-time 3 "$@" |
    jq -erj '.publicKey | select(test("^mkey:[[:xdigit:]]{64}$"))' | sha256sum | cut -d ' ' -f 1
}

start_recovery_headscale() {
  local saved_config="$1" cell_host="$2" conf=/etc/headscale/config.yaml
  install -m 0640 -o root -g headscale "${saved_config}" "${conf}"
  install -d -m 0755 /etc/systemd/system/headscale.service.d
  cat >/etc/systemd/system/headscale.service.d/recovery.conf <<EOF
[Service]
Environment=HEADSCALE_SERVER_URL=https://${cell_host}
Environment=HEADSCALE_LISTEN_ADDR=127.0.0.1:8080
Environment=HEADSCALE_METRICS_LISTEN_ADDR=127.0.0.1:9090
Environment=HEADSCALE_GRPC_LISTEN_ADDR=127.0.0.1:50443
Environment=HEADSCALE_DERP_SERVER_STUN_LISTEN_ADDR=127.0.0.1:3478
Environment=HEADSCALE_DERP_SERVER_PRIVATE_KEY_PATH=/var/lib/headscale/derp_server_private.key
Environment=HEADSCALE_NOISE_PRIVATE_KEY_PATH=/var/lib/headscale/noise_private.key
Environment=HEADSCALE_DATABASE_TYPE=sqlite
Environment=HEADSCALE_DATABASE_SQLITE_PATH=/var/lib/headscale/db.sqlite
Environment=HEADSCALE_UNIX_SOCKET=/var/run/headscale/headscale.sock
Environment=HEADSCALE_UNIX_SOCKET_PERMISSION=0770
EOF
  chmod 0644 /etc/systemd/system/headscale.service.d/recovery.conf
  systemctl unmask --quiet headscale.service
  systemctl disable --quiet headscale.service
  systemctl daemon-reload
  systemctl start headscale.service
}

# The saved agent configuration holds the original certificate, signing trust,
# protected nodes and invariants. Only its connection endpoint changes.
start_recovery_agent() {
  local target="$1" port="$2" config="${CELLAGENT_ETC}/cellagent-recovery.json"
  jq --arg url "https://127.0.0.1:${port}" '.server_url = $url' "${CELLAGENT_ETC}/cellagent.json" >"${config}"
  chown root:headmesh-cellagent "${config}"
  chmod 0640 "${config}"
  install -m 0644 "${HM_LIB_ROOT}/cellagent/headmesh-cellagent.service" "${CELLAGENT_UNIT}"
  install -d -m 0755 "$(dirname "${CELLAGENT_DROPIN}")"
  cat >"$(dirname "${CELLAGENT_DROPIN}")/recovery.conf" <<EOF
[Service]
ExecStart=
ExecStart=${target}/headmesh-cellagent run -config ${config}
ReadOnlyPaths=${CELLAGENT_ETC}
EOF
  systemctl daemon-reload
  systemctl start headmesh-cellagent.service
}

stop_recovery_candidate() {
  local status="$1" unit="$2"
  trap - EXIT INT TERM ERR
  set +e
  systemctl stop headmesh-cellagent.service "${unit}" headscale.service postgresql@18-main.service || log 'check that all standby units stopped'
  systemctl reset-failed headmesh-cellagent.service "${unit}" headscale.service postgresql@18-main.service 2>/dev/null || true
  systemctl mask --quiet headscale.service || log 'could not restore the Headscale mask'
  printf '%s\n' disabled >/etc/postgresql/18/main/start.conf || log 'could not disable the restored PostgreSQL cluster'
  systemctl daemon-reload
  exit "${status}"
}

cmd_install_recovery_runtime() {
  [[ $# == 4 && ( "$3" == -bundle || "$3" == -netrc ) ]] ||
    die 'usage: install-recovery-runtime <release> <identity> <-bundle|-netrc> <file>'
  umask 077
  local release="$1" record="${HM_STATE_ROOT}/recovery" binary unit work listing name dest owner group mode parent_group temp i
  local -a names modes=() owners=() groups=() parent_groups=() existing=()
  require_release "${release}"
  binary="${HM_SRV_ROOT}/prod/backend/releases/${release}/headmesh-server"
  [[ -x "${binary}" ]] || die 'install the matching recovery backend release first'
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  [[ -s "${record}/manifest.json" && -s "${record}/config.yaml" && ! -e "${record}/backend-ready" &&
    -z "$(state_get prod backend.active)" ]] || die 'install runtime configuration on the restored standby before starting it'
  for unit in nginx headscale; do
    [[ "$(readlink "/etc/systemd/system/${unit}.service" || true)" == /dev/null ]] || die "${unit}.service must be masked"
  done
  for unit in nginx headscale postgresql@18-main headmesh-cellagent headmesh-backend@prod-{blue,green} headmesh-waitlist@prod-{blue,green}; do
    [[ "$(systemctl show -p ActiveState --value "${unit}.service")" == inactive ]] || die "${unit}.service must be inactive"
  done

  work="$(mktemp -d /run/headmesh-recovery-state/runtime.XXXXXX)"
  trap 'rm -rf -- "${work}"; if [[ -n "${temp:-}" ]]; then rm -f -- "${temp}"; fi' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "${binary}" backup-runtime stage -manifest "${record}/manifest.json" -identity "$2" "$3" "$4" -output-dir "${work}/stage"
  listing="$(find "${work}/stage" -type f -printf '%P\n' | sort)"
  [[ -n "${listing}" ]] || die 'the private runtime stage is empty'
  mapfile -t names <<<"${listing}"
  for i in "${!names[@]}"; do
    name="${names[i]}"
    owner=root
    group="${HM_SERVICE_USER}"
    parent_group="${HM_SERVICE_USER}"
    mode=0640
    case "${name}" in
      headmesh/prod/db-owner.env) group=root; mode=0600 ;;
      headmesh/tls/fullchain.pem) group=root; mode=0644 ;;
      headmesh/tls/privkey.pem) group=root; mode=0600 ;;
      headmesh-cellagent/*) group=headmesh-cellagent; parent_group=headmesh-cellagent ;;
      headmesh/oauth/*) owner="${HM_SERVICE_USER}"; mode=0600 ;;
      headmesh/prod/*) ;;
      *) die "unexpected staged runtime file ${name}" ;;
    esac
    dest="/etc/${name}"
    [[ "$(realpath -m -- "${dest}")" == "${dest}" ]] || die "runtime path crosses a symlink: ${dest}"
    if [[ -e "${dest}" ]]; then
      if ! { [[ -f "${dest}" && "$(stat -c '%a:%U:%G' -- "${dest}")" == "${mode#0}:${owner}:${group}" ]] &&
        cmp -s -- "${work}/stage/${name}" "${dest}"; }; then
        die "runtime destination differs: ${dest}"
      fi
      existing[i]=1
    fi
    modes+=("${mode}")
    owners+=("${owner}")
    groups+=("${group}")
    parent_groups+=("${parent_group}")
  done
  for i in "${!names[@]}"; do
    name="${names[i]}"
    dest="/etc/${name}"
    [[ -z "${existing[i]:-}" ]] || continue
    install -d -m 0750 -o root -g "${parent_groups[i]}" "${dest%/*}"
    temp="$(mktemp "${dest%/*}/.recovery-runtime.XXXXXX")"
    install -m "${modes[i]}" -o "${owners[i]}" -g "${groups[i]}" "${work}/stage/${name}" "${temp}"
    ln -T -- "${temp}" "${dest}"
    rm -- "${temp}"
    temp=
  done
  rm -rf -- "${work}"
  trap - EXIT INT TERM
  log 'installed the runtime configuration for the restored standby'
}

cmd_start_recovery() {
  [[ $# == 1 ]] || die 'start-recovery requires one backend release'
  umask 077
  local release="$1" env=prod colour=blue cell target unit record="${HM_STATE_ROOT}/recovery"
  local path expected_noise minimum_nodes expected_db expected_lsn actual_noise api_url signing_key
  require_release "${release}"
  target="${HM_SRV_ROOT}/${env}/backend/releases/${release}"
  unit="$(unit_name backend "${env}" "${colour}")"
  cell="$(hm_env "${env}" CELL_NAME)"

  [[ -s "${record}/manifest.json" && -s "${record}/config.yaml" ]] || die 'install a verified restore first'
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  [[ ! -e "${record}/backend-ready" && -z "$(state_get "${env}" backend.active)" ]] || die 'a backend is already recorded for this standby'
  local mask
  for mask in nginx headscale; do
    [[ "$(readlink "/etc/systemd/system/${mask}.service" || true)" == /dev/null ]] || die "${mask}.service must be masked on the standby"
  done
  for path in nginx headscale postgresql@18-main headmesh-cellagent headmesh-backend@prod-blue headmesh-backend@prod-green; do
    [[ "$(systemctl show -p ActiveState --value "${path}.service")" == inactive ]] || die "${path}.service must be inactive"
  done
  [[ "$(awk 'NF && $1 !~ /^#/ {$1=$1; print}' /etc/postgresql/18/main/start.conf)" == disabled ]] ||
    die 'the restored PostgreSQL cluster must be disabled'
  [[ -x "${target}/headmesh-server" && -x "${target}/headmesh-cellagent" ]] || die 'install the matching backend and cell agent release first'
  id -u headmesh-cellagent >/dev/null || die 'install the restored cell agent credentials and service user first'
  for path in backend.env secrets.env db.env backend/op-signing.key backend/cell-ca/ca.crt; do
    [[ -s "${HM_ETC_ROOT}/${env}/${path}" ]] || die "missing restored backend file: ${path}"
  done
  signing_key="$(op_signing_public_key "${HM_ETC_ROOT}/${env}/backend/op-signing.key")"
  jq -e --arg cell "${cell}" --arg host "$(hm_env "${env}" API_HOST)" --arg key "${signing_key}" '
    .cell_name == $cell and .mode == "enforce" and .server_name == $host and
    .headscale_http == "http://127.0.0.1:8080" and .headscale_socket == "/var/run/headscale/headscale.sock" and
    (.server_keys | index($key)) != null
  ' "${CELLAGENT_ETC}/cellagent.json" >/dev/null || die 'restored cell configuration does not match this backend and local Headscale'
  expected_noise="$(jq -er '.headscale.noise_public_key_sha256 | select(test("^[0-9a-f]{64}$"))' "${record}/manifest.json")"
  minimum_nodes="$(jq -er '.headscale.node_count | numbers | select(. > 0 and floor == .)' "${record}/manifest.json")"
  expected_db="$(jq -er 'select(.postgres_database == "headmesh_prod") | .postgres_system_id | select(test("^[0-9]+$"))' "${record}/manifest.json")"
  expected_lsn="$(jq -er '.postgres_lsn | select(test("^[0-9A-F]+/[0-9A-F]+$"))' "${record}/manifest.json")"

  # Preflight above leaves existing services alone. From here, failures take
  # the candidate back offline and retain the original credentials and data.
  trap 'stop_recovery_candidate "$?" "${unit}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  printf '%s\n' manual >/etc/postgresql/18/main/start.conf
  systemctl daemon-reload
  systemctl start postgresql@18-main.service
  [[ "$(psql_env "${env}" -v identity="${expected_db}" -v lsn="${expected_lsn}" <<'SQL'
SELECT NOT pg_is_in_recovery() AND system_identifier::text = :'identity'
       AND pg_current_wal_flush_lsn() >= :'lsn'::pg_lsn FROM pg_control_system();
SQL
  )" == t ]] || die 'installed PostgreSQL does not match the verified restore'
  [[ "$(psql_env "${env}" -v cell="${cell}" <<<"SELECT count(*) FROM cells WHERE name = :'cell' AND mode = 'enforce';")" == 1 ]] ||
    die 'the restored database does not contain the local enforce-mode cell'

  start_recovery_headscale "${record}/config.yaml" "$(hm_env "${env}" CELL_HOST)"
  wait_healthy http://127.0.0.1:8080/health || die 'restored Headscale did not become healthy'
  actual_noise="$(recovery_noise_hash 'http://127.0.0.1:8080/key?v=130')"
  [[ "${actual_noise}" == "${expected_noise}" ]] || die 'restored Headscale is serving a different Noise identity'
  headscale --output json nodes list | jq -e --argjson minimum "${minimum_nodes}" 'length >= $minimum' >/dev/null ||
    die 'restored Headscale has fewer nodes than the manifest'

  refresh_backend_tls
  install -d -m 0755 "${HM_SRV_ROOT}/slots"
  ln -sfn "${target}" "$(slot_path backend "${env}" "${colour}")"
  write_instance_env backend "${env}" "${colour}" recovery
  systemctl start "${unit}"
  api_url="http://127.0.0.1:$(hm_port "${env}" backend "${colour}")"
  wait_healthy "${api_url}/healthz" || die 'the recovery backend did not start'
  [[ "$(recovery_http_code "${api_url}/readyz")" == 503 ]] ||
    die 'the recovery backend opened readiness before the local agent started'
  [[ "$(recovery_http_code -X POST "${api_url}/v1/grants")" == 503 ]] ||
    die 'the recovery backend did not gate writes before the local agent started'
  start_recovery_agent "${target}" "$(hm_port "${env}" backend "${colour}" cell)"
  wait_healthy "${api_url}/readyz" || die 'the restored backend did not finish the recovery policy checks'
  [[ "$(recovery_http_code "${api_url}/readyz")" == 204 &&
    "$(systemctl show -p ActiveState --value nginx.service)" == inactive ]] || die 'standby readiness or closed ingress could not be confirmed'

  # The workflow can proceed to ingress preparation after checking live readiness.
  printf '%s\n' "${release}" >"${record}/backend-ready.tmp"
  mv -f "${record}/backend-ready.tmp" "${record}/backend-ready"
  trap - EXIT
  log 'restored backend is ready and nginx remains masked'
}

rollback_recovery_waitlist() {
  local status="$1" unit="$2" scratch="$3" db="$4" installed="$5" ready="$6"
  trap - EXIT INT TERM ERR
  set +e
  if (( installed )) && [[ ! -f "${ready}" ]]; then
    if systemctl stop "${unit}"; then
      systemctl reset-failed "${unit}" 2>/dev/null || true
      rm -f -- "${db}" "${db}-wal" "${db}-shm" "${db}-journal"
    else
      log 'could not stop the standby waitlist; its database has been retained'
    fi
  fi
  rm -f -- "${scratch}" "${scratch}-wal" "${scratch}-shm" "${scratch}-journal"
  exit "${status}"
}

cmd_start_recovery_waitlist() {
  [[ $# == 2 ]] || die 'start-recovery-waitlist requires a waitlist release and a standalone SQLite snapshot'
  umask 077
  local release="$1" snapshot="$2" env=prod colour=blue record="${HM_STATE_ROOT}/recovery"
  local target="${HM_SRV_ROOT}/prod/waitlist/releases/${release}" dir="${HM_STATE_ROOT}/prod/waitlist"
  local ready="${record}/waitlist-ready" db="${dir}/waitlist.db" unit path scratch verified installed=0
  require_release "${release}"
  unit="$(unit_name waitlist "${env}" "${colour}")"
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  [[ -s "${record}/manifest.json" && -s "${record}/backend-ready" && ! -e "${record}/ingress-ready" &&
    "$(readlink /etc/systemd/system/nginx.service || true)" == /dev/null &&
    "$(systemctl show -p ActiveState --value nginx.service)" == inactive ]] ||
    die 'start the restored backend and keep nginx masked before installing the waitlist'
  [[ ! -e "${ready}" && ! -L "${ready}" && -z "$(state_get "${env}" waitlist.active)" ]] || die 'the standby waitlist is already recorded'
  for path in "${unit}" "$(unit_name waitlist "${env}" green)"; do
    [[ "$(systemctl show -p ActiveState --value "${path}")" == inactive ]] || die "${path} must be inactive"
  done
  [[ -f "${target}/server/main.ts" && -s "${HM_ETC_ROOT}/${env}/waitlist.env" ]] ||
    die 'install the matching waitlist release and original waitlist.env first'
  runuser -u "${HM_SERVICE_USER}" -- test -r "${target}/server/main.ts" ||
    die 'the service account cannot read the staged waitlist release'
  [[ -f "${snapshot}" && ! -L "${snapshot}" && -s "${snapshot}" ]] || die 'the waitlist snapshot must be a nonempty regular file'
  for path in "${snapshot}-wal" "${snapshot}-shm" "${snapshot}-journal" "${db}" "${db}-wal" "${db}-shm" "${db}-journal"; do
    [[ ! -e "${path}" && ! -L "${path}" ]] || die 'use a standalone snapshot and an empty waitlist destination'
  done
  [[ ! -L "${dir}" ]] || die 'the waitlist directory must not be a symlink'
  install -d -o "${HM_SERVICE_USER}" -g "${HM_SERVICE_USER}" -m 0750 "${dir}"
  scratch="$(mktemp "${dir}/.recovery.XXXXXXXX")"
  trap 'rollback_recovery_waitlist "$?" "${unit}" "${scratch}" "${db}" "${installed}" "${ready}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  cp -- "${snapshot}" "${scratch}"
  if ! verified="$(sqlite3 -batch -bail -readonly "${scratch}" 2>/dev/null <<'SQL'
PRAGMA integrity_check;
SELECT count(*) FROM schema_migrations WHERE name = '0001_create_waitlist.sql';
SELECT id, email, consent_version, confirmed_at, unsubscribed_at FROM waitlist LIMIT 0;
SQL
  )" || [[ "${verified}" != $'ok\n1' ]]; then
    die 'the waitlist snapshot failed SQLite integrity or schema validation'
  fi
  chown "${HM_SERVICE_USER}:${HM_SERVICE_USER}" "${scratch}"
  ln -T -- "${scratch}" "${db}"
  installed=1
  rm -f -- "${scratch}" "${scratch}-wal" "${scratch}-shm" "${scratch}-journal"
  install -d -m 0755 "${HM_SRV_ROOT}/slots"
  ln -sfn "${target}" "$(slot_path waitlist "${env}" "${colour}")"
  write_instance_env waitlist "${env}" "${colour}"
  systemctl start "${unit}"
  wait_healthy "$(health_url "${env}" waitlist "${colour}")" || die 'the restored waitlist did not become ready'
  [[ "$(recovery_http_code "$(health_url "${env}" waitlist "${colour}")")" == 204 ]] || die 'the restored waitlist did not return readiness'
  printf '%s\n' "${release}" >"${ready}.tmp"
  mv -f -- "${ready}.tmp" "${ready}"
  trap - EXIT INT TERM
  log 'restored waitlist is ready on loopback'
}

rollback_recovery_ingress() {
  local status="$1" relay_conf="$2" cell="$3"
  local relay_changed=0
  trap - EXIT INT TERM ERR
  set +e
  systemctl stop nginx.service || log 'check that nginx stopped'
  systemctl mask --quiet nginx.service || log 'could not restore the nginx mask'
  systemctl reset-failed nginx.service 2>/dev/null || true
  [[ -f "${relay_conf}" ]] && relay_changed=1
  nginx_txn_restore || log "check the previous nginx files in ${NGINX_TXN_DIR}"
  if (( relay_changed )); then
    if [[ -e "${relay_conf}" ]] || ! restart_recovery_cell "${cell}"; then
      systemctl stop headmesh-cellagent.service headscale.service || log 'check that the failed standby cell stopped'
      log 'could not recover the private cell after rolling back public ingress'
    fi
  fi
  exit "${status}"
}

restart_recovery_cell() {
  local cell="$1" since
  systemctl stop headmesh-cellagent.service || return 1
  systemctl daemon-reload || return 1
  systemctl restart headscale.service || return 1
  wait_healthy http://127.0.0.1:8080/health || return 1
  since="$(psql_env prod <<<"SELECT now();")" || return 1
  systemctl start headmesh-cellagent.service || return 1
  wait_cell_seen prod "${cell}" "${since}"
}

check_recovery_ingress() {
  local expected_noise="$1" ca=/etc/ssl/certs/ca-certificates.crt api cell site actual_noise ready
  api="$(hm_env prod API_HOST)"
  cell="$(hm_env prod CELL_HOST)"
  site="$(hm_env prod SITE_HOST)"
  [[ "$(recovery_http_code --cacert "${ca}" --resolve "${api}:443:127.0.0.1" "https://${api}/readyz")" == 204 ]] ||
    die 'the standby API is unready through nginx'
  [[ "$(recovery_http_code --cacert "${ca}" --resolve "${cell}:443:127.0.0.1" "https://${cell}/health")" == 200 ]] ||
    die 'the standby Headscale is unready through nginx'
  [[ "$(recovery_http_code --cacert "${ca}" --resolve "${cell}:443:127.0.0.1" "https://${cell}/derp/probe")" == 200 ]] ||
    die 'the standby DERP probe is unready through nginx'
  actual_noise="$(recovery_noise_hash --cacert "${ca}" --resolve "${cell}:443:127.0.0.1" "https://${cell}/key?v=130")"
  [[ "${actual_noise}" == "${expected_noise}" ]] || die 'nginx served a different Headscale Noise identity'
  [[ "$(recovery_http_code --cacert "${ca}" --resolve "${site}:443:127.0.0.1" "https://${site}/")" == 200 ]] ||
    die 'the standby site is unready through nginx'
  ready="$(recovery_http_code --cacert "${ca}" --resolve "${site}:443:127.0.0.1" "https://${site}/api/waitlist/readyz")"
  if [[ "${ready}" == 429 ]]; then
    # nginx shares a six-requests-per-minute bucket with the waitlist route.
    sleep 10
    ready="$(recovery_http_code --cacert "${ca}" --resolve "${site}:443:127.0.0.1" "https://${site}/api/waitlist/readyz")"
  fi
  [[ "${ready}" == 204 ]] || die "the standby waitlist is unready through nginx (HTTP ${ready})"
}

cmd_prepare_recovery_ingress() {
  [[ $# == 4 ]] || die 'prepare-recovery-ingress requires an nginx directory, a marketing release and the standby IPv4 and IPv6'
  umask 077
  local staging="$1" site_release="$2" ipv4="$3" ipv6="$4" env=prod record="${HM_STATE_ROOT}/recovery"
  local root="${HM_SRV_ROOT}/prod/marketing" tls="${HM_ETC_ROOT}/tls/fullchain.pem"
  local ca=/etc/ssl/certs/ca-certificates.crt enabled=/etc/nginx/sites-enabled
  local relay_conf=/etc/systemd/system/headscale.service.d/standby-relay.conf
  local backend api cell cell_name site expected_noise host pair service waitlist stun_pid
  require_release "${site_release}"
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  [[ -s "${record}/manifest.json" && -s "${record}/backend-ready" && -z "$(state_get "${env}" backend.active)" ]] ||
    die 'start the restored backend first'
  [[ ! -e "${record}/ingress-ready" && "$(readlink /etc/systemd/system/nginx.service || true)" == /dev/null &&
    "$(systemctl show -p ActiveState --value nginx.service)" == inactive ]] || die 'nginx must be masked and inactive on the standby'
  [[ ! -e "${relay_conf}" && ! -L "${relay_conf}" ]] || die 'a public relay configuration already exists on the standby'
  ip -4 -j address show scope global | jq -e --arg ip "${ipv4}" 'any(.[].addr_info[]; .local == $ip)' >/dev/null ||
    die 'the standby IPv4 must be assigned to this server'
  ip -6 -j address show scope global | jq -e --arg ip "${ipv6}" 'any(.[].addr_info[]; .local == $ip and .tentative != true and .dadfailed != true)' >/dev/null ||
    die 'the standby IPv6 must be assigned to this server'
  backend="$(<"${record}/backend-ready")"
  require_release "${backend}"
  [[ "$(readlink "$(slot_path backend "${env}" blue)" || true)" == "${HM_SRV_ROOT}/prod/backend/releases/${backend}" &&
    "$(recovery_http_code "$(health_url "${env}" backend blue)")" == 204 ]] || die 'the recorded standby backend is unready'
  [[ -s "${record}/waitlist-ready" ]] || die 'start the restored waitlist before opening standby ingress'
  waitlist="$(<"${record}/waitlist-ready")"
  require_release "${waitlist}"
  [[ "$(readlink "$(slot_path waitlist "${env}" blue)" || true)" == "${HM_SRV_ROOT}/prod/waitlist/releases/${waitlist}" &&
    "$(recovery_http_code "$(health_url "${env}" waitlist blue)")" == 204 ]] || die 'the recorded standby waitlist is unready'
  [[ -s "${root}/releases/${site_release}/site/index.html" && -s "${root}/releases/${site_release}/nginx/security-headers.conf" ]] ||
    die 'install the matching marketing release first'
  api="$(hm_env "${env}" API_HOST)"
  cell="$(hm_env "${env}" CELL_HOST)"
  cell_name="$(hm_env "${env}" CELL_NAME)"
  site="$(hm_env "${env}" SITE_HOST)"
  expected_noise="$(jq -er '.headscale.noise_public_key_sha256 | select(test("^[0-9a-f]{64}$"))' "${record}/manifest.json")"
  [[ -s "${tls}" && -s "${HM_ETC_ROOT}/tls/privkey.pem" ]] || die 'install the DNS-01 certificate and private key first'
  local -a tls_hosts=("${api}" "${cell}" "${site}") extra_hosts
  read -r -a extra_hosts <<<"$(hm_env "${env}" EXTRA_HOSTS)"
  for host in "${tls_hosts[@]}" "${extra_hosts[@]}"; do
    openssl verify -CAfile "${ca}" -no-CApath -no-CAstore -untrusted "${tls}" -purpose sslserver \
      -verify_hostname "${host}" "${tls}" >/dev/null || die "the standby certificate is untrusted or invalid for ${host}"
  done
  nginx_files "${env}"
  for pair in "${NGINX_GENERATED[@]}" "${NGINX_MANAGED[@]}"; do
    [[ -s "${staging}/${pair%%:*}" ]] || die "missing rendered file ${pair%%:*}"
  done
  nginx_txn_begin
  trap 'rollback_recovery_ingress "$?" "${relay_conf}" "${cell_name}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  render_upstream "${env}" backend blue "${NGINX_TXN_DIR}"
  render_upstream "${env}" waitlist blue "${NGINX_TXN_DIR}"
  for service in waitlist backend cell; do
    nginx_txn_put "${NGINX_TXN_DIR}/${service}-upstream.conf" "$(upstream_file "${env}" "${service}")"
  done
  for pair in "${NGINX_GENERATED[@]}" "${NGINX_MANAGED[@]}"; do
    nginx_txn_put "${staging}/${pair%%:*}" "${pair#*:}"
  done
  if [[ "$(readlink "${enabled}/default" || true)" == /etc/nginx/sites-available/default ]]; then
    nginx_txn_remember "${enabled}/default"
    rm "${enabled}/default"
  fi
  nginx_txn_link "/etc/nginx/sites-available/headmesh-${env}.conf" "${enabled}/headmesh-${env}.conf"
  nginx_txn_link "releases/${site_release}" "${root}/current"
  nginx_check || die 'the standby nginx configuration failed validation'
  cat >"${NGINX_TXN_DIR}/recovery-relay.conf" <<EOF
[Service]
Environment=HEADSCALE_DERP_SERVER_IPV4=${ipv4}
Environment=HEADSCALE_DERP_SERVER_IPV6=${ipv6}
Environment=HEADSCALE_DERP_SERVER_STUN_LISTEN_ADDR=[::]:3478
EOF
  nginx_txn_put "${NGINX_TXN_DIR}/recovery-relay.conf" "${relay_conf}"
  restart_recovery_cell "${cell_name}" || die 'the standby cell did not reconnect after enabling its relay'
  stun_pid="$(systemctl show -p MainPID --value headscale.service)"
  ss -6 -Hlunp 'sport = :3478' | grep -F "pid=${stun_pid}," >/dev/null || die 'Headscale did not start its public STUN listener'
  systemctl unmask --quiet nginx.service
  systemctl disable --quiet nginx.service
  systemctl start nginx.service
  check_recovery_ingress "${expected_noise}"
  [[ "$(recovery_http_code --cacert "${ca}" --resolve "${site}:443:127.0.0.1" "https://${site}/api/waitlist")" == 405 ]] ||
    die 'the standby waitlist route is unready through nginx'
  printf '%s\n%s\n' "${backend}" "${site_release}" >"${record}/ingress-ready.tmp"
  mv -f "${record}/ingress-ready.tmp" "${record}/ingress-ready"
  trap - EXIT INT TERM
  log 'standby HTTPS passed on loopback and Headscale STUN is listening; run headmesh-recover prove-ingress from the recovery runner'
}

reconnect_recovery_agent() {
  local cell="$1" since
  systemctl stop headmesh-cellagent.service || return 1
  since="$(psql_env prod <<<"SELECT now();")" || return 1
  systemctl start headmesh-cellagent.service || return 1
  wait_cell_seen prod "${cell}" "${since}"
}

cmd_finalize_recovery() {
  [[ $# == 0 ]] || die 'finalize-recovery takes no arguments'
  umask 077
  local record="${HM_STATE_ROOT}/recovery" state backend waitlist site schema expected_noise agent_cell tmp file unit
  local start_conf=/etc/postgresql/18/main/start.conf agent=/usr/local/bin/headmesh-cellagent
  local agent_config="${CELLAGENT_ETC}/cellagent.json" direct_dropin="${CELLAGENT_DROPIN%/*}/recovery.conf"
  local -a ingress new_boot_units=() files=("${start_conf}" "${agent}" "${agent_config}" "${CELLAGENT_DROPIN}" "${direct_dropin}")
  local -a units=(postgresql.service headscale.service headmesh-backend@prod-blue.service
    headmesh-waitlist@prod-blue.service nginx.service headmesh-cellagent.service)
  state="$(state_dir prod)"
  install -d -m 0700 /run/headmesh-recovery-state
  exec 9>/run/headmesh-recovery-state/lock
  flock -n 9 || die 'another recovery operation is in progress'
  [[ -s "${record}/manifest.json" && -s "${record}/backend-ready" && -s "${record}/waitlist-ready" && -s "${record}/ingress-ready" ]] ||
    die 'prepare the restored standby ingress first'
  backend="$(<"${record}/backend-ready")"
  waitlist="$(<"${record}/waitlist-ready")"
  mapfile -t ingress <"${record}/ingress-ready"
  [[ ${#ingress[@]} == 2 && "${ingress[0]}" == "${backend}" ]] || die 'the standby records disagree on the backend release'
  site="${ingress[1]}"
  require_release "${backend}"
  require_release "${waitlist}"
  require_release "${site}"

  if [[ -e "${state}" || -L "${state}" ]]; then
    [[ "$(state_get prod recovery.finalized)" == "${backend} ${waitlist} ${site}" &&
      "$(state_get prod backend.active)" == blue && "$(state_get prod waitlist.active)" == blue &&
      "$(state_get prod marketing.active)" == "${site}" ]] || die 'production deploy state already exists'
  fi
  [[ "$(readlink "$(slot_path backend prod blue)" || true)" == "${HM_SRV_ROOT}/prod/backend/releases/${backend}" &&
    "$(readlink "$(slot_path waitlist prod blue)" || true)" == "${HM_SRV_ROOT}/prod/waitlist/releases/${waitlist}" &&
    "$(readlink "${HM_SRV_ROOT}/prod/marketing/current" || true)" == "releases/${site}" ]] ||
    die 'the standby releases differ from the verified ingress'
  for unit in "${units[@]:1}" postgresql@18-main.service; do
    systemctl is-active --quiet "${unit}" || die "${unit} is not running on the standby"
  done
  expected_noise="$(jq -er '.headscale.noise_public_key_sha256 | select(test("^[0-9a-f]{64}$"))' "${record}/manifest.json")"
  if [[ -d "${state}" ]]; then
    [[ "$(<"${start_conf}")" == auto && ! -e "${direct_dropin}" ]] || die 'the finalized standby startup configuration changed'
    for unit in "${units[@]}"; do
      systemctl is-enabled --quiet "${unit}" || die "${unit} is no longer enabled"
    done
    check_recovery_ingress "${expected_noise}"
    log 'the restored standby is already finalized'
    return 0
  fi

  [[ "$(<"${start_conf}")" == manual && -s "${direct_dropin}" && -s "${agent_config}" ]] ||
    die 'expected manually started recovery services'
  schema="$(schema_version prod)"
  [[ "${schema}" =~ ^[1-9][0-9]*$ ]] || die 'could not read the restored schema version'
  for unit in "${units[@]}"; do
    case "$(systemctl is-enabled "${unit}" 2>/dev/null || true)" in
      enabled) ;;
      disabled) new_boot_units+=("${unit}") ;;
      *) die "unexpected startup state for ${unit}" ;;
    esac
  done

  tmp="$(mktemp -d "${state%/*}/.finalize.XXXXXX")"
  trap 'rm -rf -- "${tmp}"' EXIT
  install -d -m 0750 "${tmp}/state"
  printf '%s\n' blue >"${tmp}/state/backend.active"
  printf '%s\n' blue >"${tmp}/state/waitlist.active"
  printf '%s\n' "${site}" >"${tmp}/state/marketing.active"
  printf '%s\n' "${schema}" >"${tmp}/state/backend-blue.schema"
  printf '%s %s %s\n' "${backend}" "${waitlist}" "${site}" >"${tmp}/state/recovery.finalized"
  for file in "${files[@]}"; do
    if [[ -e "${file}" || -L "${file}" ]]; then cp -a -- "${file}" "${tmp}/${file##*/}"; fi
  done
  jq --arg url "https://127.0.0.1:$(hm_env prod CELL_PORT)" '.server_url = $url' "${agent_config}" >"${tmp}/agent.json"

  # shellcheck disable=SC2329 # Called by the EXIT trap.
  rollback_finalization() {
    local status="$1" saved
    trap - EXIT ERR INT TERM
    # The atomic state publication commits the change, including when interrupted immediately afterward.
    if [[ -d "${tmp}/state" ]]; then
      set +e
      if (( ${#new_boot_units[@]} )); then
        systemctl disable --quiet "${new_boot_units[@]}" || log 'check the affected standby boot links'
      fi
      for file in "${files[@]}"; do
        saved="${tmp}/${file##*/}"
        if [[ -e "${saved}" || -L "${saved}" ]]; then
          cp -a --remove-destination -- "${saved}" "${file}" || log "could not restore ${file}"
        else
          rm -f -- "${file}" || log "could not remove ${file}"
        fi
      done
      if ! { systemctl daemon-reload && reconnect_recovery_agent "${agent_cell}"; }; then
        log 'check the restored direct recovery agent connection'
      fi
    fi
    rm -rf -- "${tmp}"
    exit "${status}"
  }
  agent_cell="$(hm_env prod CELL_NAME)"
  trap 'rollback_finalization "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  install -o root -g headmesh-cellagent -m 0640 "${tmp}/agent.json" "${agent_config}"
  install -o root -g root -m 0755 "${HM_SRV_ROOT}/prod/backend/releases/${backend}/headmesh-cellagent" "${agent}"
  write_cellagent_dropin "${tmp}/normal.conf"
  rm -- "${direct_dropin}"
  systemctl daemon-reload
  reconnect_recovery_agent "${agent_cell}" || die 'the standby cell did not connect through the nginx mTLS proxy'
  check_recovery_ingress "${expected_noise}"

  printf '%s\n' auto >"${start_conf}"
  systemctl enable --quiet "${units[@]}"
  mv -nT -- "${tmp}/state" "${state}"
  [[ ! -d "${tmp}/state" ]] || die 'production deploy state appeared during finalization'
  trap - EXIT INT TERM
  rm -rf -- "${tmp}"
  log 'restored standby adopted on Blue and enabled for normal startup'
}

# Only the latest effective revision from the current recovery boot qualifies.
# A newer due revision that is pending or failed must prevent a passing check.
recovery_policy_revision() {
  PGOPTIONS='-c default_transaction_read_only=on' psql_env prod -v start="$1" -v cell="$2" <<'SQL'
SELECT r.revision, r.sha256, r.status,
       COALESCE(r.applied_at >= :'start'::timestamptz, false)
FROM policy_revisions r JOIN cells c ON c.id = r.cell_id
WHERE c.name = :'cell' AND c.mode = 'enforce'
  AND r.compiled_at >= :'start'::timestamptz AND r.effective_at <= clock_timestamp()
ORDER BY r.revision DESC
LIMIT 1;
SQL
}

cmd_verify_recovery_cell() {
  [[ $# == 0 ]] || die 'verify-recovery-cell reads the client receipt from stdin'
  local record="${HM_STATE_ROOT}/recovery" state unit service started cell minimum noise receipt count
  local revision expected status acknowledged applied before actual digest
  state="$(state_dir prod)"
  unit="$(unit_name backend prod blue)"
  [[ -s "${record}/manifest.json" && -s "${state}/recovery.finalized" && "$(state_get prod backend.active)" == blue ]] ||
    die 'verify the cell on the finalized recovered standby before an ordinary backend deploy'
  grep -qx 'HEADMESH_RECOVERY=true' "${HM_ETC_ROOT}/instances/backend-prod-blue.env" || die 'the active Blue backend must be in recovery mode'
  [[ "$(recovery_http_code "$(health_url prod backend blue)")" == 204 ]] || die 'the recovered backend is unready'
  for service in "${unit}" headmesh-cellagent.service headscale.service; do
    systemctl is-active --quiet "${service}" || die "${service} is not running on the recovered standby"
  done

  receipt="$(head -c 4097)"
  [[ "${#receipt}" -le 4096 ]] || die 'temporary client receipt exceeds 4096 bytes'
  jq -se '
    length == 1 and (.[0] | keys == ["hostname", "ips", "node_key"]
      and (.hostname | test("^hm-recovery-[0-9a-z]{10}$"))
      and (.node_key | test("^nodekey:[0-9a-f]{64}$"))
      and (.ips | type == "array" and length > 0 and length <= 2 and all(.[]; type == "string")))
  ' <<<"${receipt}" >/dev/null 2>&1 || die 'invalid temporary client receipt'
  minimum="$(jq -er '.headscale.node_count | numbers | select(. > 0 and floor == .)' "${record}/manifest.json")"
  noise="$(jq -er '.headscale.noise_public_key_sha256 | select(test("^[0-9a-f]{64}$"))' "${record}/manifest.json")"
  [[ "$(recovery_noise_hash 'http://127.0.0.1:8080/key?v=130')" == "${noise}" ]] || die 'the local cell serves a different Noise identity'

  count="$(headscale nodes list -o json | jq -er --argjson client "${receipt}" --argjson minimum "${minimum}" '
    ([.[] | select(.node_key == $client.node_key)]) as $matches
    | select(($matches | length) == 1 and ($matches[0] |
        .name == $client.hostname and .online == true and .pre_auth_key.ephemeral == true
        and (.pre_auth_key.reusable != true) and ((.ip_addresses | sort) == ($client.ips | sort))))
    | length - 1 | select(. >= $minimum)
  ')" || die 'the temporary client is missing or mismatched, or the restored node count is below the manifest'

  started="$(LC_ALL=C systemctl show --value -p ExecMainStartTimestamp "${unit}")"
  [[ -n "${started}" ]] || die 'could not read the active recovery backend start time'
  started="$(LC_ALL=C date -ud "${started}" '+%Y-%m-%dT%H:%M:%SZ')"
  cell="$(hm_env prod CELL_NAME)"
  before="$(recovery_policy_revision "${started}" "${cell}")"
  IFS='|' read -r revision expected status acknowledged <<<"${before}"
  [[ "${revision}" =~ ^[1-9][0-9]*$ && "${expected}" =~ ^[0-9a-f]{64}$ && "${status}" == applied && "${acknowledged}" == t ]] ||
    die 'the current policy lacks a successful acknowledgement from this recovery boot'
  # Headscale's CLI appends one newline to the exact policy returned by gRPC.
  applied="$(headscale policy get | head -c -1 | sha256sum)"
  actual="${applied%% *}"
  [[ "${actual}" == "${expected}" ]] || die 'the policy served by Headscale differs from the acknowledged policy'
  digest="$(recovery_policy_revision "${started}" "${cell}")"
  [[ "${digest}" == "${before}" ]] || die 'the current policy changed during verification. Repeat the check.'
  log "verified an online ephemeral client, ${count} other nodes, and acknowledged policy revision ${revision} (${actual})"
}

cmd_rollback() {
  local component="$1" env="$2" keep_schema="${3:-}"
  require_env "${env}"
  if [[ "${component}" == marketing ]]; then
    local previous
    previous="$(state_get "${env}" marketing.previous)"
    [[ -n "${previous}" ]] || die "marketing ${env}: no previous release to roll back to"
    cmd_activate_static "${env}" "${previous}"
    return
  fi
  require_service "${component}"
  [[ -z "${keep_schema}" || ("${keep_schema}" == keep-schema && "${component}" == backend) ]] ||
    die "rollback: unexpected argument '${keep_schema}'"
  local active previous unit current="" wanted=""
  active="$(state_get "${env}" "${component}.active")"
  previous="$(hm_other_colour "${active}")"
  [[ -n "${active}" && -L "$(slot_path "${component}" "${env}" "${previous}")" ]] ||
    die "${component} ${env}: no previous colour to roll back to"
  unit="$(unit_name "${component}" "${env}" "${previous}")"
  log "${component} ${env}: rolling back from ${active} to ${previous} ($(basename "$(readlink "$(slot_path "${component}" "${env}" "${previous}")")"))"
  if [[ "${component}" == backend && -z "${keep_schema}" ]]; then
    current="$(schema_version "${env}")"
    wanted="$(state_get "${env}" "backend-${previous}.schema")"
    if [[ -z "${wanted}" ]]; then
      log "backend ${env}: ${previous} has no recorded schema version (deployed before schema rollback); keeping migration ${current}"
    elif ((wanted > current)); then
      # Rolling forward again: the newer release brings its migrations back before it starts, as on a deploy.
      run_migrations "${env}" "$(slot_path backend "${env}" "${previous}")/headmesh-server" "${previous}"
    fi
  fi
  write_instance_env "${component}" "${env}" "${previous}"
  systemctl restart "${unit}"
  wait_healthy "$(health_url "${env}" "${component}" "${previous}")" || {
    systemctl stop "${unit}" || true
    die "${component} ${env}: ${previous} didn't become ready; ${active} stays live"
  }
  switch_to "${component}" "${env}" "${previous}" rollback || die "${component} ${env}: nginx rejected the rollback"
  state_set "${env}" "${component}.previous" "${active}"
  state_set "${env}" "${component}.active" "${previous}"
  systemctl enable --quiet "${unit}"
  systemctl disable --quiet --now "$(unit_name "${component}" "${env}" "${active}")" || true
  log "${component} ${env}: rolled back to ${previous}"
  # Reverting goes last: the older release is already live, and it doesn't use what the newer migrations added.
  if [[ -n "${wanted}" ]] && ((wanted < current)); then
    revert_schema "${env}" "$(slot_path backend "${env}" "${active}")/headmesh-server" "${wanted}"
  elif [[ -n "${keep_schema}" ]]; then
    log "backend ${env}: keep-schema: the schema stays at migration $(schema_version "${env}")"
  fi
}

# ── housekeeping ───────────────────────────────────────────────────────────────────────────────────────────────

cmd_prune() {
  local env="$1" keep="$2" component dir release
  require_env "${env}"
  [[ "${keep}" =~ ^[0-9]+$ && "${keep}" -ge 2 ]] || die "keep must be a number ≥ 2"
  for component in marketing "${HM_SERVICES[@]}"; do
    dir="${HM_SRV_ROOT}/${env}/${component}/releases"
    [[ -d "${dir}" ]] || continue
    local -a protected=()
    if [[ "${component}" == marketing ]]; then
      protected+=("$(state_get "${env}" marketing.active)" "$(state_get "${env}" marketing.previous)")
    else
      local colour
      for colour in "${HM_COLOURS[@]}"; do
        protected+=("$(basename "$(readlink "$(slot_path "${component}" "${env}" "${colour}")" 2>/dev/null || echo none)")")
      done
    fi
    while IFS= read -r release; do
      [[ " ${protected[*]} " == *" ${release} "* ]] && continue
      rm -rf "${dir:?}/${release}"
      log "prune ${env}/${component}: removed ${release}"
    done < <(find "${dir}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort -r | tail -n +"$((keep + 1))")
  done
}

# tls_probe <host>: a TLS handshake with nginx on loopback, sending <host> as SNI the way Cloudflare does. Prints the
# certificate's issuer and expiry, or says the handshake failed: headmesh.conf's default server refuses names no
# enabled vhost serves, and Cloudflare reports that as error 525.
tls_probe() {
  local host="$1" pem issuer expiry state=valid
  pem="$(timeout 5 openssl s_client -connect 127.0.0.1:443 -servername "${host}" </dev/null 2>/dev/null | openssl x509 2>/dev/null || true)"
  if [[ -z "${pem}" ]]; then
    echo "handshake FAILED (no enabled vhost serves this name; Cloudflare shows 525)"
    return 0
  fi
  issuer="$(openssl x509 -noout -issuer <<<"${pem}" | sed 's/^issuer= *//')"
  expiry="$(openssl x509 -noout -enddate <<<"${pem}" | sed 's/^notAfter=//')"
  openssl x509 -noout -checkend 0 <<<"${pem}" >/dev/null || state=EXPIRED
  printf '%s, expires %s (%s)\n' "${issuer}" "${expiry}" "${state}"
}

cmd_status() {
  local env service colour unit url host
  local -a envs=("$@")
  [[ ${#envs[@]} -gt 0 ]] || envs=("${HM_ENVS[@]}")
  for env in "${envs[@]}"; do
    require_env "${env}"
    printf '== %s (%s, %s)\n' "${env}" "$(hm_env "${env}" SITE_HOST)" "$(hm_env "${env}" API_HOST)"
    printf '   vhost      %s\n' "$([[ -L "/etc/nginx/sites-enabled/headmesh-${env}.conf" ]] && echo enabled || echo 'NOT ENABLED (run a deploy for this environment)')"
    for host in $(hm_env "${env}" SITE_HOST) $(hm_env "${env}" EXTRA_HOSTS) $(hm_env "${env}" API_HOST); do
      printf '   tls        %-24s %s\n' "${host}" "$(tls_probe "${host}")"
    done
    printf '   marketing  live: %s  previous: %s\n' "$(state_get "${env}" marketing.active)" "$(state_get "${env}" marketing.previous)"
    for service in "${HM_SERVICES[@]}"; do
      colour="$(state_get "${env}" "${service}.active")"
      printf '   %-9s  live colour: %s\n' "${service}" "${colour:-none}"
      for colour in "${HM_COLOURS[@]}"; do
        unit="$(unit_name "${service}" "${env}" "${colour}")"
        url="$(health_url "${env}" "${service}" "${colour}")"
        printf '     %-5s %-8s %-40s %-9s %s\n' "${colour}" "$(systemctl is-active "${unit}" 2>/dev/null || true)" \
          "$(basename "$(readlink "$(slot_path "${service}" "${env}" "${colour}")" 2>/dev/null || echo -)")" \
          "$(if curl -fs -o /dev/null --max-time 2 "${url}" 2>/dev/null; then echo ready; else echo 'not ready'; fi)" \
          "$([[ "${service}" == backend ]] && echo "schema $(state_get "${env}" "backend-${colour}.schema" | grep . || echo '?')")"
      done
    done
  done
  printf '== nginx: %s\n' "$(nginx_check && echo 'config OK' || echo 'config BROKEN')"
}

cmd_smoke() {
  local env="$1" site api code failed=0
  require_env "${env}"
  site="$(hm_env "${env}" SITE_HOST)"
  api="$(hm_env "${env}" API_HOST)"
  check() {
    local host="$1" path="$2" method="$3" want="$4"
    code="$(curl -ks -o /dev/null -w '%{http_code}' -X "${method}" --max-time 5 --resolve "${host}:443:127.0.0.1" "https://${host}${path}" || true)"
    if [[ "${code}" == "${want}" ]]; then
      log "smoke ${env}: ${method} https://${host}${path} → ${code}"
    else
      log "smoke ${env}: ${method} https://${host}${path} → ${code} (expected ${want})"
      failed=1
    fi
  }
  [[ -L "${HM_SRV_ROOT}/${env}/marketing/current" ]] && {
    check "${site}" / GET 200
    check "${site}" /waitlist/ GET 200
    check "${site}" /github GET 302
  }
  [[ -n "$(state_get "${env}" waitlist.active)" ]] && check "${site}" /api/waitlist GET 405
  [[ -n "$(state_get "${env}" backend.active)" ]] && check "${api}" /healthz GET 204
  [[ "${failed}" == 0 ]] || die "smoke ${env}: some checks failed"
}

# ── cell agent ─────────────────────────────────────────────────────────────────────────────────────────────────────
# An environment with a CELL_NAME runs Headscale on this server (bootstrap.sh) and headmesh-cellagent beside it. The
# agent reaches the live backend colour through nginx's stream proxy on loopback and verifies the API hostname's
# certificate (server_name). Its client certificate comes from the environment's cell CA and is reissued by
# headmesh-cellcert@<env>.timer within 30 days of expiry.

CELLAGENT_ETC="/etc/headmesh-cellagent"
CELLAGENT_UNIT="/etc/systemd/system/headmesh-cellagent.service"
CELLAGENT_DROPIN="/etc/systemd/system/headmesh-cellagent.service.d/deploy.conf"
# The operator org a cell is registered under the first time its agent is set up.
CELL_OPS_ORG="headmesh-ops"

# psql_env <env> [psql args...]: SQL from stdin against the environment's database, as postgres; bare rows out.
psql_env() { runuser -u postgres -- psql -X -q -A -t -v ON_ERROR_STOP=1 -d "headmesh_$1" "${@:2}"; }

# op_signing_public_key <seed-file>: the base64 Ed25519 public key for a `headmesh-server keygen` seed, the form
# cellagent.json's server_keys takes. openssl wants the seed wrapped as PKCS#8 DER (RFC 8410).
op_signing_public_key() {
  { printf '\x30\x2e\x02\x01\x00\x30\x05\x06\x03\x2b\x65\x70\x04\x22\x04\x20'; base64 -d <"$1"; } |
    openssl pkey -inform DER -pubout -outform DER | tail -c 32 | base64
}

# register_cell <env> <headmesh-server>: create the cell (and its operator org) once. The bootstrap's one-time admin
# API key goes to a root-only file for the operator to move into a password manager.
register_cell() {
  local env="$1" binary="$2" cell out
  cell="$(hm_env "${env}" CELL_NAME)"
  [[ "$(psql_env "${env}" -v cell="${cell}" <<<"SELECT count(*) FROM cells WHERE name = :'cell';")" == 1 ]] && return 0
  out="${HM_ETC_ROOT}/${env}/cell-${cell}-bootstrap.json"
  log "cell ${cell}: registering it in ${env} under the ${CELL_OPS_ORG} org"
  (
    umask 077
    systemd-run --quiet --pipe --wait --collect \
      -p User="${HM_SERVICE_USER}" -p EnvironmentFile="${HM_ETC_ROOT}/${env}/db.env" \
      "${binary}" bootstrap -cell "${cell}" -server-url "https://$(hm_env "${env}" CELL_HOST)" \
      -base-domain "$(hm_env "${env}" CELL_BASE_DOMAIN)" -cell-mode "$(hm_env "${env}" CELL_MODE)" \
      -org "${CELL_OPS_ORG}" -org-name "Headmesh operations" >"${out}"
  )
  log "cell ${cell}: registered; ${out} (root only) holds the org's admin API key: move it to a password manager"
}

# cell_cert <env> <headmesh-server>: the agent's client certificate, reissued within 30 days of expiry.
cell_cert() {
  local env="$1" binary="$2" cell ca tmp crt="${CELLAGENT_ETC}/client.crt"
  cell="$(hm_env "${env}" CELL_NAME)"
  ca="${HM_ETC_ROOT}/${env}/backend/cell-ca"
  if [[ -f "${crt}" ]] && openssl x509 -checkend $((30 * 86400)) -noout -in "${crt}" >/dev/null; then return 0; fi
  if [[ ! -f "${ca}/ca.key" ]]; then
    [[ -f "${crt}" ]] && openssl x509 -checkend 0 -noout -in "${crt}" >/dev/null &&
      { log "cell ${cell}: certificate expires within 30 days and ${ca}/ca.key is offline; reissue it by hand"; return 0; }
    die "cell ${cell}: no valid agent certificate and ${ca}/ca.key is offline"
  fi
  tmp="$(mktemp -d)"
  "${binary}" cell-cert -dir "${ca}" -cell "${cell}" -out "${tmp}" -days 90 >/dev/null 2>&1
  install -o root -g headmesh-cellagent -m 0644 "${tmp}/${cell}.crt" "${crt}"
  install -o root -g headmesh-cellagent -m 0640 "${tmp}/${cell}.key" "${CELLAGENT_ETC}/client.key"
  rm -rf "${tmp}"
  log "cell ${cell}: issued the agent a 90-day client certificate"
}

# Everything whose change means a restart: the binary, the unit, the config and the certificates.
cellagent_fingerprint() {
  { cat /usr/local/bin/headmesh-cellagent "${CELLAGENT_UNIT}" "${CELLAGENT_DROPIN}" "${CELLAGENT_ETC}/cellagent.json" \
    "${CELLAGENT_ETC}/client.crt" "${CELLAGENT_ETC}/server-ca.pem" 2>/dev/null || true; } | sha256sum
}

# wait_cell_seen <env> <cell> <since>: until the hub records the agent's Hello (cells.last_seen_at) after <since>.
wait_cell_seen() {
  local env="$1" cell="$2" since="$3" deadline=$((SECONDS + HEALTH_TIMEOUT))
  while ((SECONDS < deadline)); do
    [[ "$(psql_env "${env}" -v cell="${cell}" -v since="${since}" \
      <<<"SELECT count(*) FROM cells WHERE name = :'cell' AND last_seen_at >= :'since';")" == 1 ]] && return 0
    sleep 1
  done
  return 1
}

write_cellagent_dropin() {
  # Ubuntu 26.04's install (uutils) can't copy /dev/stdin over an existing file.
  cat >"$1" <<EOF
# Written by deployctl: this server keeps the agent's files apart from /etc/headmesh, which only the backend reads.
[Service]
ExecStart=
ExecStart=/usr/local/bin/headmesh-cellagent run -config ${CELLAGENT_ETC}/cellagent.json
ReadOnlyPaths=${CELLAGENT_ETC}
EOF
  install -m 0644 "$1" "${CELLAGENT_DROPIN}"
}

cmd_cell_agent() {
  local env="$1" cell colour release before since server_ca="" tmp
  require_env "${env}"
  cell="$(hm_env "${env}" CELL_NAME)"
  [[ -n "${cell}" ]] || { log "cell agent ${env}: this environment hosts no cell"; return 0; }
  colour="$(state_get "${env}" backend.active)"
  [[ -n "${colour}" ]] || die "cell agent ${env}: no live backend yet"
  release="$(readlink -f "$(slot_path backend "${env}" "${colour}")")"
  [[ -x "${release}/headmesh-cellagent" ]] || die "cell agent ${env}: release $(basename "${release}") has no headmesh-cellagent"
  systemctl is-active --quiet headscale || die "cell agent ${env}: Headscale isn't running (deploy.sh --bootstrap installs it)"

  if ! id headmesh-cellagent &>/dev/null; then
    useradd --system --no-create-home --home-dir /var/lib/headmesh-cellagent --shell /usr/sbin/nologin headmesh-cellagent
  fi
  install -d -o root -g headmesh-cellagent -m 0750 "${CELLAGENT_ETC}"
  before="$(cellagent_fingerprint)"

  register_cell "${env}" "${release}/headmesh-server"
  cell_cert "${env}" "${release}/headmesh-server"
  # A Let's Encrypt certificate verifies against the system roots; the self-signed placeholder is its own root.
  if [[ "$(readlink -f "${HM_ETC_ROOT}/tls/fullchain.pem")" == /etc/letsencrypt/* ]]; then
    rm -f "${CELLAGENT_ETC}/server-ca.pem"
  else
    install -o root -g headmesh-cellagent -m 0644 "$(readlink -f "${HM_ETC_ROOT}/tls/fullchain.pem")" "${CELLAGENT_ETC}/server-ca.pem"
    server_ca="${CELLAGENT_ETC}/server-ca.pem"
  fi
  tmp="$(mktemp)"
  cat >"${tmp}" <<EOF
{
  "cell_name": "${cell}",
  "mode": "$(hm_env "${env}" CELL_MODE)",
  "state_dir": "/var/lib/headmesh-cellagent",
  "headscale_socket": "/var/run/headscale/headscale.sock",
  "headscale_http": "http://127.0.0.1:8080",
  "server_url": "https://127.0.0.1:$(hm_env "${env}" CELL_PORT)",
  "server_name": "$(hm_env "${env}" API_HOST)",
  "server_ca": "${server_ca}",
  "client_cert": "${CELLAGENT_ETC}/client.crt",
  "client_key": "${CELLAGENT_ETC}/client.key",
  "server_keys": ["$(op_signing_public_key "${HM_ETC_ROOT}/${env}/backend/op-signing.key")"],
  "protected_nodes": [],
  "invariants": []
}
EOF
  install -o root -g headmesh-cellagent -m 0640 "${tmp}" "${CELLAGENT_ETC}/cellagent.json"
  rm -f "${tmp}"
  install -m 0755 -o root -g root "${release}/headmesh-cellagent" /usr/local/bin/headmesh-cellagent
  install -m 0644 "${HM_LIB_ROOT}/cellagent/headmesh-cellagent.service" "${CELLAGENT_UNIT}"
  install -d -m 0755 "$(dirname "${CELLAGENT_DROPIN}")"
  tmp="$(mktemp)"
  write_cellagent_dropin "${tmp}"
  rm -f "${tmp}"
  systemctl daemon-reload
  systemctl enable --quiet headmesh-cellagent.service "headmesh-cellcert@${env}.timer"
  systemctl start --quiet "headmesh-cellcert@${env}.timer"

  if [[ "$(cellagent_fingerprint)" == "${before}" ]] && systemctl is-active --quiet headmesh-cellagent.service; then
    log "cell ${cell}: agent unchanged and running"
    return 0
  fi
  since="$(psql_env "${env}" <<<"SELECT now();")"
  systemctl restart headmesh-cellagent.service
  if ! wait_cell_seen "${env}" "${cell}" "${since}"; then
    journalctl -u headmesh-cellagent -n 30 --no-pager >&2 || true
    die "cell ${cell}: the agent didn't reach the ${env} backend within ${HEALTH_TIMEOUT}s"
  fi
  log "cell ${cell}: agent connected to the ${env} backend ($(hm_env "${env}" CELL_MODE) mode)"
}

cmd_tls_renewed() {
  refresh_backend_tls
  nginx_check || die "nginx -t failed after certificate renewal; not reloading"
  nginx_reload
  local env service=backend colour
  for env in "${HM_ENVS[@]}"; do
    colour="$(state_get "${env}" backend.active)"
    [[ -n "${colour}" ]] && systemctl try-restart "$(unit_name "${service}" "${env}" "${colour}")"
    # The first Let's Encrypt certificate replaces the self-signed root the agent was told to trust.
    [[ -f "${CELLAGENT_ETC}/cellagent.json" && -n "$(hm_env "${env}" CELL_NAME)" ]] && cmd_cell_agent "${env}"
  done
  log "certificate renewed; nginx reloaded and live backends restarted"
}

main() {
  [[ ${EUID} -eq 0 ]] || die "run as root"
  local command="${1:-}"
  shift || true
  case "${command}" in
    receive) cmd_receive "$@" ;;
    clear-staging) cmd_clear_staging "$@" ;;
    install-env) cmd_install_env "$@" ;;
    nginx-apply) cmd_nginx_apply "$@" ;;
    nginx-diff) cmd_nginx_diff "$@" ;;
    activate-static) cmd_activate_static "$@" ;;
    activate-service) cmd_activate_service "$@" ;;
    install-recovery-runtime) cmd_install_recovery_runtime "$@" ;;
    start-recovery) cmd_start_recovery "$@" ;;
    start-recovery-waitlist) cmd_start_recovery_waitlist "$@" ;;
    prepare-recovery-ingress) cmd_prepare_recovery_ingress "$@" ;;
    finalize-recovery) cmd_finalize_recovery "$@" ;;
    verify-recovery-cell) cmd_verify_recovery_cell "$@" ;;
    rollback) cmd_rollback "$@" ;;
    status) cmd_status "$@" ;;
    prune) cmd_prune "$@" ;;
    backup) cmd_backup "$@" ;;
    publish-manifest) cmd_publish_backup manifest "$@" ;;
    publish-waitlist) cmd_publish_backup waitlist "$@" ;;
    smoke) cmd_smoke "$@" ;;
    cell-agent) cmd_cell_agent "$@" ;;
    tls-renewed) cmd_tls_renewed ;;
    *) sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
}

main "$@"
