#!/usr/bin/env bash
# deploy.sh: build Headmesh on this machine and deploy it to hm-prod-1 (plan §13.2): nginx in front, every service
# on loopback, blue-green switches, nginx -t before every reload.
#
#   deploy/deploy.sh --env prod|test|all --target frontend|backend|all [options]
#
# Targets
#   frontend   headmesh.dev from web/marketing: the static site and docs (switched atomically by symlink) and the
#              waitlist service the site's form posts to (blue-green)
#   backend    headmesh-server: the REST API and the mTLS cell endpoint (blue-green; migrations run first)
#   all        both; the backend goes first
#
# Options
#   --host DEST         SSH destination (default: $HM_DEPLOY_HOST from deploy/env/local.env: hm-prod-1 over the
#                       tailnet; $HM_DEPLOY_BREAKGLASS_HOST is the break-glass WireGuard address)
#   --identity FILE     SSH key (default: $HM_DEPLOY_IDENTITY, else ~/.ssh/hm-prod-1 when it exists)
#   --bootstrap         install or update the server side first: packages, Node, Postgres roles and databases,
#                       systemd units, TLS, nginx base config (idempotent; safe to repeat)
#   --nginx-overwrite   replace nginx files on the server that differ from the local render. Without it, drift is
#                       shown and the server's copy is kept. Upstream switches and generated files always apply.
#   --init-env          create missing deploy/env/<env>/*.env files from the examples, generating secrets
#   --skip-checks       don't run lint and tests before building
#   --dry-run           build, upload and validate, show nginx drift; switch nothing and reload nothing
#   --rollback          switch the target back to its previous release (site) or colour (services); builds nothing
#   --status            show what is live in each environment; builds nothing
#   --keep N            releases to keep per component (default 5; live and rollback releases are never removed)
#   --allow-dirty       deploy uncommitted changes (refused for prod)
#   --yes               don't ask for confirmation before touching prod
#   -h, --help          this text
#
# Local settings (deploy/env/local.env, git-ignored; see local.env.example): HM_DEPLOY_HOST, HM_DEPLOY_HOST_KEY_ALIAS,
# HM_DEPLOY_BREAKGLASS_HOST and HM_DEPLOY_IDENTITY. The environment overrides the file.
#
# Configuration (always uploaded, always replaces the server's copy; never committed):
#   deploy/env/<env>/build.env      build-time settings for the site (PUBLIC_* keys, HEADMESH_PRODUCTION)
#   deploy/env/<env>/waitlist.env   waitlist service settings and secrets (see web/marketing/server/config.ts)
#   deploy/env/<env>/backend.env    headmesh-server settings (see cmd/headmesh-server/config.go)
# Each has a committed *.env.example next to it. Database URLs, signing keys and the cell CA are generated on the
# server and never leave it.
#
# Tests drive the same code against a local container: HM_DEPLOY_REMOTE replaces "ssh <host>" with another command
# prefix (for example "docker exec -i <container>"), HM_DEPLOY_SUDO replaces "sudo -n", and HM_DEPLOY_ENV_DIR replaces
# deploy/env (deploy/tests/e2e.sh).
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/environments.sh
source "${ROOT}/deploy/lib/environments.sh"
# The server's addresses stay out of the repository: they come from this git-ignored file or the environment.
if [[ -f "${ROOT}/deploy/env/local.env" ]]; then
  while IFS='=' read -r key value; do
    [[ "${key}" =~ ^HM_DEPLOY_[A-Z_]+$ && -z "${!key:-}" ]] && export "${key}=${value}"
  done < <(grep -E '^HM_DEPLOY_[A-Z_]+=' "${ROOT}/deploy/env/local.env")
fi

log() { printf '\033[1m[deploy]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[deploy] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

# The header comment above, up to the first line of code.
usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

# ── arguments ─────────────────────────────────────────────────────────────────────────────────────────────────────

ENV_ARG="" TARGET_ARG="" HOST="${HM_DEPLOY_HOST:-}" IDENTITY="${HM_DEPLOY_IDENTITY:-}"
BOOTSTRAP=0 NGINX_OVERWRITE=0 INIT_ENV=0 SKIP_CHECKS=0 DRY_RUN=0 ROLLBACK=0 STATUS=0 KEEP=5 ALLOW_DIRTY=0 YES=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env) ENV_ARG="${2:-}"; shift 2 ;;
    --env=*) ENV_ARG="${1#*=}"; shift ;;
    --target) TARGET_ARG="${2:-}"; shift 2 ;;
    --target=*) TARGET_ARG="${1#*=}"; shift ;;
    --host) HOST="${2:-}"; shift 2 ;;
    --host=*) HOST="${1#*=}"; shift ;;
    --identity) IDENTITY="${2:-}"; shift 2 ;;
    --identity=*) IDENTITY="${1#*=}"; shift ;;
    --keep) KEEP="${2:-}"; shift 2 ;;
    --keep=*) KEEP="${1#*=}"; shift ;;
    --bootstrap) BOOTSTRAP=1; shift ;;
    --nginx-overwrite) NGINX_OVERWRITE=1; shift ;;
    --init-env) INIT_ENV=1; shift ;;
    --skip-checks) SKIP_CHECKS=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    --status) STATUS=1; shift ;;
    --allow-dirty) ALLOW_DIRTY=1; shift ;;
    --yes | -y) YES=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

case "${ENV_ARG}" in
  all) ENVS=("${HM_ENVS[@]}") ;;
  "") if [[ ${STATUS} == 1 ]]; then ENVS=("${HM_ENVS[@]}"); else die "--env prod|test|all is required"; fi ;;
  *) hm_is_env "${ENV_ARG}" || die "--env must be one of: ${HM_ENVS[*]} all"; ENVS=("${ENV_ARG}") ;;
esac
case "${TARGET_ARG}" in
  all) TARGETS=(backend frontend) ;;
  frontend | backend) TARGETS=("${TARGET_ARG}") ;;
  "")
    if [[ ${STATUS} == 1 || ${BOOTSTRAP} == 1 || ${INIT_ENV} == 1 ]]; then TARGETS=(); else die "--target frontend|backend|all is required"; fi
    ;;
  *) die "--target must be one of: frontend backend all" ;;
esac
[[ "${KEEP}" =~ ^[0-9]+$ && "${KEEP}" -ge 2 ]] || die "--keep must be a number ≥ 2"
(( ROLLBACK + STATUS + DRY_RUN <= 1 )) || die "--rollback, --status and --dry-run don't combine"
[[ -n "${HOST}" || -n "${HM_DEPLOY_REMOTE:-}" ]] || die "no server: set HM_DEPLOY_HOST in deploy/env/local.env (see local.env.example) or pass --host"

has_target() { local t; for t in "${TARGETS[@]}"; do [[ "$t" == "$1" ]] && return 0; done; return 1; }
has_env() { local e; for e in "${ENVS[@]}"; do [[ "$e" == "$1" ]] && return 0; done; return 1; }

# ── remote execution ──────────────────────────────────────────────────────────────────────────────────────────────

SUDO="${HM_DEPLOY_SUDO-sudo -n}"
CTL="${HM_LIB_ROOT}/deployctl.sh"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
# The tailnet and break-glass addresses present the host key that was saved under the server's public address.
[[ -n "${HM_DEPLOY_HOST_KEY_ALIAS:-}" ]] && SSH_OPTS+=(-o "HostKeyAlias=${HM_DEPLOY_HOST_KEY_ALIAS}")
[[ -z "${IDENTITY}" && -f "${HOME}/.ssh/hm-prod-1" ]] && IDENTITY="${HOME}/.ssh/hm-prod-1"
if [[ -n "${IDENTITY}" ]]; then
  [[ -f "${IDENTITY}" ]] || die "--identity ${IDENTITY}: no such file"
  SSH_OPTS+=(-i "${IDENTITY}" -o IdentitiesOnly=yes)
fi

# remote <args...>: run one command on the server. Arguments are quoted for the remote shell.
remote() {
  local cmd="" arg
  for arg in "$@"; do cmd+="$(printf '%q ' "${arg}")"; done
  if [[ -n "${HM_DEPLOY_REMOTE:-}" ]]; then
    # shellcheck disable=SC2086 # HM_DEPLOY_REMOTE is a command prefix
    ${HM_DEPLOY_REMOTE} bash -c "${cmd}"
  else
    ssh "${SSH_OPTS[@]}" "${HOST}" "${cmd}"
  fi
}

# ctl <deployctl args...>: run deployctl as root on the server.
ctl() {
  # shellcheck disable=SC2086 # SUDO is a command prefix (empty in tests)
  remote ${SUDO} "${CTL}" "$@"
}

# push <local-dir> <remote-dir>: stream a directory to the server; deployctl unpacks it into a fresh directory.
push() {
  tar -C "$1" -cf - . | ctl receive "$2"
}

# Install deployctl, the environment table, the server bootstrap and the systemd units. Runs on every deploy, so the
# server always runs the version that matches this checkout.
install_toolkit() {
  local tk
  tk="$(mktemp -d)"
  install -m 0755 "${ROOT}/deploy/server/deployctl.sh" "${ROOT}/deploy/server/bootstrap.sh" "${tk}/"
  install -m 0644 "${ROOT}/deploy/lib/environments.sh" "${tk}/"
  install -d "${tk}/systemd"
  install -m 0644 "${ROOT}"/deploy/systemd/* "${tk}/systemd/"
  # shellcheck disable=SC2016,SC2086 # expanded remotely; SUDO is a command prefix
  tar -C "${tk}" -cf - . | remote ${SUDO} bash -c '
    set -e
    d="$1"; tmp="$(mktemp -d)"
    tar -xf - -C "${tmp}" --no-same-owner
    install -d -m 0755 "${d}" "${d}/systemd"
    install -m 0755 "${tmp}/deployctl.sh" "${tmp}/bootstrap.sh" "${d}/"
    install -m 0644 "${tmp}/environments.sh" "${d}/"
    install -m 0644 "${tmp}"/systemd/* "${d}/systemd/"
    rm -rf "${tmp}"' _ "${HM_LIB_ROOT}"
  rm -rf "${tk}"
}

# ── local configuration ───────────────────────────────────────────────────────────────────────────────────────────

env_dir() { echo "${HM_DEPLOY_ENV_DIR:-${ROOT}/deploy/env}/$1"; }

# Files each target needs per environment.
env_files_for() {
  case "$1" in
    frontend) echo "build.env waitlist.env" ;;
    backend) echo "backend.env" ;;
  esac
}

validate_env_file() {
  local file="$1" line n=0
  while IFS= read -r line || [[ -n "${line}" ]]; do
    n=$((n + 1))
    [[ -z "${line}" || "${line}" =~ ^[[:space:]]*# ]] && continue
    [[ "${line}" =~ ^[A-Z][A-Z0-9_]*=.*$ ]] || die "${file}:${n}: expected KEY=value"
  done <"${file}"
}

# init_env <env> <file>: create one env file from its example, filling generated secrets.
init_env_file() {
  local env="$1" name="$2" src dst
  src="$(env_dir "${env}")/${name}.example"
  dst="$(env_dir "${env}")/${name}"
  [[ -f "${dst}" ]] && return 0
  [[ -f "${src}" ]] || die "no example for ${dst}"
  (umask 077 && sed "s|^WAITLIST_SECRET=CHANGE_ME.*$|WAITLIST_SECRET=$(openssl rand -base64 48 | tr -d '\n')|" "${src}" >"${dst}")
  log "created ${dst#"${ROOT}"/} from its example; review it before deploying ${env}"
}

check_env_files() {
  local env target name file missing=0
  for env in "${ENVS[@]}"; do
    for target in "${TARGETS[@]}"; do
      for name in $(env_files_for "${target}"); do
        file="$(env_dir "${env}")/${name}"
        [[ ${INIT_ENV} == 1 ]] && init_env_file "${env}" "${name}"
        if [[ ! -f "${file}" ]]; then
          log "missing ${file#"${ROOT}"/} (copy ${name}.example next to it, or run with --init-env)"
          missing=1
          continue
        fi
        validate_env_file "${file}"
        if grep -qE '^[A-Z_]+=CHANGE_ME' "${file}"; then
          grep -nE '^[A-Z_]+=CHANGE_ME' "${file}" | sed "s|^|    ${file#"${ROOT}"/}:|" >&2
          log "${file#"${ROOT}"/} still has CHANGE_ME placeholders (listed above)"
          missing=1
        fi
      done
    done
  done
  [[ ${missing} == 0 ]] || die "configuration incomplete"
}

# ── building ─────────────────────────────────────────────────────────────────────────────────────────────────────

WORK=""
cleanup() { [[ -n "${WORK}" && -d "${WORK}" ]] && rm -rf "${WORK}"; return 0; }
trap cleanup EXIT

release_id() {
  local sha dirty=""
  sha="$(git -C "${ROOT}" rev-parse --short=12 HEAD)"
  [[ -n "$(git -C "${ROOT}" status --porcelain)" ]] && dirty="-dirty"
  echo "$(date -u +%Y%m%dT%H%M%SZ)-${sha}${dirty}"
}

run_checks() {
  [[ ${SKIP_CHECKS} == 1 ]] && { log "skipping lint and tests (--skip-checks)"; return 0; }
  if has_target frontend; then
    log "checks: web/marketing lint, type-check and tests"
    pnpm --dir "${ROOT}" --filter @headmesh/marketing run lint >/dev/null
    pnpm --dir "${ROOT}" --filter @headmesh/marketing run test >/dev/null
  fi
  if has_target backend; then
    log "checks: go vet and unit tests"
    (cd "${ROOT}" && go vet ./... && go test -count=1 ./... >/dev/null)
  fi
}

build_backend() {
  local out="${WORK}/backend"
  install -d "${out}"
  log "build: headmesh-server ${RELEASE} (linux/amd64, static)"
  (cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.version=${RELEASE}" -o "${out}/headmesh-server" ./cmd/headmesh-server)
}

# The waitlist service: the marketing package's server half with only its production dependencies.
build_waitlist() {
  local out="${WORK}/waitlist"
  log "build: waitlist service ${RELEASE}"
  # package.json "files" limits the copy to server/, shared/ and migrations/, plus production dependencies.
  pnpm --dir "${ROOT}" --filter @headmesh/marketing deploy --prod "${out}" >/dev/null
  [[ -f "${out}/server/main.ts" && -d "${out}/node_modules/zod" && -d "${out}/node_modules/resend" && ! -d "${out}/src" ]] || die "waitlist bundle is incomplete"
}

# build_site <env>: the static site with that environment's build settings.
build_site() {
  local env="$1"
  local out="${WORK}/site-${env}" pkg="${ROOT}/web/marketing"
  log "build: headmesh.dev site for ${env}"
  (
    set -a
    # shellcheck source=/dev/null
    source "$(env_dir "${env}")/build.env"
    set +a
    [[ "$(hm_env "${env}" PRODUCTION)" == 1 ]] && export HEADMESH_PRODUCTION=1
    cd "${pkg}" && pnpm run build >/dev/null
  )
  install -d "${out}/site" "${out}/nginx"
  cp -a "${pkg}/dist/." "${out}/site/"
  install -m 0644 "${pkg}/build/nginx/security-headers.conf" "${out}/nginx/"
  [[ -f "${out}/site/index.html" ]] || die "site build for ${env} has no index.html"
}

# ── nginx rendering ───────────────────────────────────────────────────────────────────────────────────────────────

# render <template> <out> <env>: replace {{KEY}} placeholders (bash replacement, so nginx's own $variables are safe).
render() {
  local tmpl="$1" out="$2" env="$3" s extra="" noindex="" robots=""
  s="$(<"${tmpl}")"
  if [[ -n "$(hm_env "${env}" EXTRA_HOSTS)" ]]; then
    extra="$(<"${ROOT}/deploy/nginx/extra-hosts.conf.tmpl")"
  fi
  if [[ "$(hm_env "${env}" NOINDEX)" == 1 ]]; then
    noindex='add_header X-Robots-Tag "noindex, nofollow" always;'
    robots="$(<"${ROOT}/deploy/nginx/robots-noindex.conf.tmpl")"
  fi
  s="${s//\{\{EXTRA_HOSTS_SERVER\}\}/${extra}}"
  s="${s//\{\{ROBOTS_OVERRIDE\}\}/${robots}}"
  s="${s//\{\{NOINDEX_HEADER\}\}/${noindex}}"
  s="${s//\{\{ENV\}\}/${env}}"
  s="${s//\{\{SITE_HOST\}\}/$(hm_env "${env}" SITE_HOST)}"
  s="${s//\{\{EXTRA_HOSTS\}\}/$(hm_env "${env}" EXTRA_HOSTS)}"
  s="${s//\{\{API_HOST\}\}/$(hm_env "${env}" API_HOST)}"
  s="${s//\{\{CELL_PORT\}\}/$(hm_env "${env}" CELL_PORT)}"
  s="${s//\{\{SRV_ROOT\}\}/${HM_SRV_ROOT}}"
  s="${s//\{\{NGINX_ROOT\}\}/${HM_NGINX_ROOT}}"
  s="${s//\{\{REPO_URL\}\}/${HM_REPO_URL}}"
  [[ "${s}" != *"{{"* ]] || die "unrendered placeholder in ${tmpl}: $(grep -o '{{[A-Z_]*}}' <<<"${s}" | sort -u | tr '\n' ' ')"
  install -d "$(dirname "${out}")"
  printf '%s\n' "${s}" >"${out}"
}

# Cloudflare's edge ranges, refreshed when cloudflare.com answers, else the committed snapshot.
render_cloudflare_realip() {
  local out="$1" ranges
  ranges="$({ curl -fsS --max-time 10 https://www.cloudflare.com/ips-v4 && echo && curl -fsS --max-time 10 https://www.cloudflare.com/ips-v6; } 2>/dev/null || true)"
  if ! grep -qE '^[0-9a-f.:]+/[0-9]+$' <<<"${ranges}"; then
    log "nginx: cloudflare.com unreachable; using the committed range snapshot"
    ranges="$(grep -vE '^\s*(#|$)' "${ROOT}/deploy/nginx/cloudflare-ips.txt")"
  fi
  {
    echo "# Generated by deploy/deploy.sh from https://www.cloudflare.com/ips/ ($(date -u +%F)). Replaced on every deploy."
    grep -E '^[0-9a-f.:]+/[0-9]+$' <<<"${ranges}" | sed 's/^/set_real_ip_from /; s/$/;/'
  } >"${out}"
}

render_nginx() {
  local env="$1"
  local out="${WORK}/nginx-${env}"
  install -d "${out}/snippets" "${out}/sites" "${out}/stream"
  render_cloudflare_realip "${out}/cloudflare-realip.conf"
  install -m 0644 "${ROOT}/deploy/nginx/headmesh.conf" "${ROOT}/deploy/nginx/stream-include.conf" "${out}/"
  install -m 0644 "${ROOT}"/deploy/nginx/snippets/*.conf "${out}/snippets/"
  render "${ROOT}/deploy/nginx/site.conf.tmpl" "${out}/sites/headmesh-${env}.conf" "${env}"
  render "${ROOT}/deploy/nginx/stream.conf.tmpl" "${out}/stream/${env}.conf" "${env}"
}

# ── steps ─────────────────────────────────────────────────────────────────────────────────────────────────────────

confirm_prod() {
  has_env prod || return 0
  [[ ${YES} == 1 || ${DRY_RUN} == 1 || ${STATUS} == 1 ]] && return 0
  [[ -t 0 ]] || die "deploying prod needs --yes when not run from a terminal"
  local what="deploy ${TARGETS[*]} to prod"
  [[ ${ROLLBACK} == 1 ]] && what="roll back ${TARGETS[*]} in prod"
  [[ ${#TARGETS[@]} == 0 ]] && what="bootstrap the server"
  read -r -p "About to ${what} on ${HOST}. Type 'prod' to continue: " answer
  [[ "${answer}" == prod ]] || die "cancelled"
}

preflight() {
  local tool
  for tool in git tar openssl curl; do command -v "${tool}" >/dev/null || die "${tool} is required"; done
  if [[ ${ROLLBACK} == 0 && ${STATUS} == 0 && ${#TARGETS[@]} -gt 0 ]]; then
    if has_target frontend && ! { command -v pnpm && command -v node; } >/dev/null; then die "pnpm and node are required for the frontend"; fi
    if has_target backend && ! command -v go >/dev/null; then die "go is required for the backend"; fi
    if [[ -n "$(git -C "${ROOT}" status --porcelain)" ]]; then
      # Only a real server is protected; the e2e test's container (HM_DEPLOY_REMOTE) may take a dirty prod build.
      if has_env prod && [[ ${DRY_RUN} == 0 && -z "${HM_DEPLOY_REMOTE:-}" ]]; then
        die "the working tree has uncommitted changes; commit them before deploying prod"
      fi
      [[ ${ALLOW_DIRTY} == 1 || ${DRY_RUN} == 1 ]] || die "the working tree has uncommitted changes (use --allow-dirty for test)"
      log "deploying uncommitted changes; the release is marked -dirty"
    fi
  fi
  log "server: ${HM_DEPLOY_REMOTE:-${HOST}}"
  remote true || die "can't reach ${HOST} (tailnet down? try --host ${HM_DEPLOY_BREAKGLASS_HOST:-<break-glass address>})"
  # shellcheck disable=SC2086
  remote ${SUDO} true || die "passwordless sudo is required on ${HOST}"
}

deploy_env() {
  local env="$1" name
  local stage="${HM_STATE_ROOT}/staging/${RELEASE}/${env}"
  log "── ${env}: $(hm_env "${env}" SITE_HOST), $(hm_env "${env}" API_HOST)"

  # Configuration first: env files always replace the server's copies; nginx files follow --nginx-overwrite.
  local cfg="${WORK}/env-${env}"
  install -d -m 0700 "${cfg}"
  if has_target frontend; then install -m 0600 "$(env_dir "${env}")/waitlist.env" "${cfg}/"; fi
  if has_target backend; then install -m 0600 "$(env_dir "${env}")/backend.env" "${cfg}/"; fi
  push "${WORK}/nginx-${env}" "${stage}/nginx"
  if [[ ${DRY_RUN} == 1 ]]; then
    ctl nginx-diff "${env}" "${stage}/nginx"
  else
    push "${cfg}" "${stage}/env"
    ctl install-env "${env}" "${stage}/env"
    ctl nginx-apply "${env}" "${stage}/nginx" "${NGINX_OVERWRITE}"
  fi

  if has_target backend; then
    push "${WORK}/backend" "${HM_SRV_ROOT}/${env}/backend/releases/${RELEASE}"
    [[ ${DRY_RUN} == 1 ]] || ctl activate-service backend "${env}" "${RELEASE}"
  fi
  if has_target frontend; then
    push "${WORK}/waitlist" "${HM_SRV_ROOT}/${env}/waitlist/releases/${RELEASE}"
    push "${WORK}/site-${env}" "${HM_SRV_ROOT}/${env}/marketing/releases/${RELEASE}"
    if [[ ${DRY_RUN} == 0 ]]; then
      # The service before the site: a new form never posts to an old API.
      ctl activate-service waitlist "${env}" "${RELEASE}"
      ctl activate-static "${env}" "${RELEASE}"
    fi
  fi

  ctl clear-staging "${RELEASE}"
  if [[ ${DRY_RUN} == 1 ]]; then
    log "${env}: dry run complete; release ${RELEASE} is uploaded but not live"
    return 0
  fi
  ctl smoke "${env}"
  ctl prune "${env}" "${KEEP}"
  for name in "${TARGETS[@]}"; do log "${env}: ${name} ${RELEASE} is live"; done
}

rollback_env() {
  local env="$1"
  if has_target backend; then ctl rollback backend "${env}"; fi
  if has_target frontend; then
    ctl rollback marketing "${env}"
    ctl rollback waitlist "${env}"
  fi
  ctl smoke "${env}"
}

main() {
  if [[ ${INIT_ENV} == 1 && ${#TARGETS[@]} == 0 ]]; then
    local env n
    for env in "${ENVS[@]}"; do for n in build.env waitlist.env backend.env; do init_env_file "${env}" "${n}"; done; done
    if [[ ${BOOTSTRAP} == 0 ]]; then return 0; fi
  fi
  confirm_prod
  preflight
  install_toolkit

  if [[ ${STATUS} == 1 ]]; then
    ctl status "${ENVS[@]}"
    return 0
  fi
  if [[ ${BOOTSTRAP} == 1 ]]; then
    log "bootstrap: server packages, Node, Postgres, units, TLS (${ENVS[*]})"
    # shellcheck disable=SC2086
    remote ${SUDO} "${HM_LIB_ROOT}/bootstrap.sh" "${ENVS[@]}"
  fi
  [[ ${#TARGETS[@]} -gt 0 ]] || return 0
  if [[ ${ROLLBACK} == 1 ]]; then
    local env
    for env in "${ENVS[@]}"; do rollback_env "${env}"; done
    return 0
  fi

  check_env_files
  RELEASE="$(release_id)"
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/headmesh-deploy.XXXXXX")"
  log "release ${RELEASE}: ${TARGETS[*]} → ${ENVS[*]}"
  run_checks
  if has_target backend; then build_backend; fi
  if has_target frontend; then build_waitlist; fi
  local env
  for env in "${ENVS[@]}"; do
    if has_target frontend; then build_site "${env}"; fi
    render_nginx "${env}"
  done
  for env in "${ENVS[@]}"; do deploy_env "${env}"; done
  log "done: ${RELEASE}"
}

main
