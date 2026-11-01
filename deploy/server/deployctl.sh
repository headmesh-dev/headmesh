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
#   rollback         <component> <env> [keep-schema]   back to the previous release (marketing) or colour (services);
#                                                      the backend's schema follows to what that colour ran with
#   status           [env...]                          releases, colours, units and health
#   prune            <env> <keep>                      delete old releases (never the live or rollback ones)
#   backup           <env>                             copy the waitlist database (also run daily by a timer)
#                                                      (Postgres is dumped before every schema change instead)
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

# write_instance_env <service> <env> <colour>: the env file the systemd instance reads.
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
  # Through a file: Ubuntu 26.04's install (uutils) can't copy /dev/stdin over an existing file.
  tmp="$(mktemp)"
  cat >"${tmp}" <<EOF
# Written by deployctl: this server keeps the agent's files apart from /etc/headmesh, which only the backend reads.
[Service]
ExecStart=
ExecStart=/usr/local/bin/headmesh-cellagent run -config ${CELLAGENT_ETC}/cellagent.json
ReadOnlyPaths=${CELLAGENT_ETC}
EOF
  install -m 0644 "${tmp}" "${CELLAGENT_DROPIN}"
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
    rollback) cmd_rollback "$@" ;;
    status) cmd_status "$@" ;;
    prune) cmd_prune "$@" ;;
    backup) cmd_backup "$@" ;;
    smoke) cmd_smoke "$@" ;;
    cell-agent) cmd_cell_agent "$@" ;;
    tls-renewed) cmd_tls_renewed ;;
    *) sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
}

main "$@"
