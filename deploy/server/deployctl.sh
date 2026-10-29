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
#   rollback         <component> <env>                 back to the previous release (marketing) or colour (services)
#   status           [env...]                          releases, colours, units and health
#   prune            <env> <keep>                      delete old releases (never the live or rollback ones)
#   backup           <env>                             copy the waitlist database (also run daily by a timer)
#   smoke            <env>                             request each vhost through nginx on loopback
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
run_migrations() {
  local env="$1" binary="$2" db="headmesh_$1" app="headmesh_$1_app"
  [[ -f "${HM_ETC_ROOT}/${env}/db-owner.env" ]] || die "${HM_ETC_ROOT}/${env}/db-owner.env is missing; run deploy.sh --bootstrap first"
  log "backend ${env}: applying database migrations"
  systemd-run --quiet --pipe --wait --collect \
    -p User="${HM_SERVICE_USER}" -p EnvironmentFile="${HM_ETC_ROOT}/${env}/db-owner.env" \
    "${binary}" migrate
  runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -d "${db}" <<SQL
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ${app};
REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM ${app};
REVOKE ALL ON goose_db_version FROM ${app};
SQL
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
    run_migrations "${env}" "${target}/headmesh-server"
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
  local component="$1" env="$2"
  require_env "${env}"
  if [[ "${component}" == marketing ]]; then
    local previous
    previous="$(state_get "${env}" marketing.previous)"
    [[ -n "${previous}" ]] || die "marketing ${env}: no previous release to roll back to"
    cmd_activate_static "${env}" "${previous}"
    return
  fi
  require_service "${component}"
  local active previous unit
  active="$(state_get "${env}" "${component}.active")"
  previous="$(hm_other_colour "${active}")"
  [[ -n "${active}" && -L "$(slot_path "${component}" "${env}" "${previous}")" ]] ||
    die "${component} ${env}: no previous colour to roll back to"
  unit="$(unit_name "${component}" "${env}" "${previous}")"
  log "${component} ${env}: rolling back from ${active} to ${previous} ($(basename "$(readlink "$(slot_path "${component}" "${env}" "${previous}")")"))"
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
        printf '     %-5s %-8s %-40s %s\n' "${colour}" "$(systemctl is-active "${unit}" 2>/dev/null || true)" \
          "$(basename "$(readlink "$(slot_path "${service}" "${env}" "${colour}")" 2>/dev/null || echo -)")" \
          "$(if curl -fs -o /dev/null --max-time 2 "${url}" 2>/dev/null; then echo ready; else echo 'not ready'; fi)"
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

cmd_tls_renewed() {
  refresh_backend_tls
  nginx_check || die "nginx -t failed after certificate renewal; not reloading"
  nginx_reload
  local env service=backend colour
  for env in "${HM_ENVS[@]}"; do
    colour="$(state_get "${env}" backend.active)"
    [[ -n "${colour}" ]] && systemctl try-restart "$(unit_name "${service}" "${env}" "${colour}")"
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
    tls-renewed) cmd_tls_renewed ;;
    *) sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
}

main "$@"
