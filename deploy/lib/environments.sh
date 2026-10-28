# shellcheck shell=bash
# shellcheck disable=SC2034 # every variable here is used by the scripts that source this file
# The deployable environments and everything that differs between them. Sourced by deploy/deploy.sh on the
# workstation and by deployctl.sh on the server, so both sides agree on names, hosts and ports.
#
# Both environments live on hm-prod-1 as separate stacks: their own vhosts, systemd instances, ports, SQLite file,
# Postgres database and env files. Every service listens on loopback only; nginx is the sole public entry point.

HM_ENVS=(prod test)
HM_COLOURS=(blue green)
# Services deployed blue-green. The static marketing site switches atomically through a symlink instead.
HM_SERVICES=(waitlist backend)

HM_REPO_URL="https://github.com/headmesh-dev/headmesh"
HM_SRV_ROOT="/srv/headmesh"
HM_STATE_ROOT="/var/lib/headmesh"
HM_ETC_ROOT="/etc/headmesh"
HM_NGINX_ROOT="/etc/nginx/headmesh"
HM_LIB_ROOT="/usr/local/lib/headmesh"
HM_SERVICE_USER="headmesh"

hm_is_env() {
  local candidate="$1" known
  for known in "${HM_ENVS[@]}"; do [[ "${candidate}" == "${known}" ]] && return 0; done
  return 1
}

# hm_env <env> <key>: print one setting. Keys:
#   SITE_HOST        marketing site hostname
#   EXTRA_HOSTS      hostnames that redirect to SITE_HOST (may be empty)
#   API_HOST         public API hostname. One label below the apex, so Cloudflare's universal certificate covers it
#   CELL_PORT        public TCP port for the mTLS cell endpoint (nginx stream, TLS passed through)
#   NOINDEX          "1" to send X-Robots-Tag: noindex on every response
#   PRODUCTION       "1" for the production build preflight (real Turnstile key, CASL postal address)
hm_env() {
  local env="$1" key="$2"
  case "${env}:${key}" in
    prod:SITE_HOST) echo "headmesh.dev" ;;
    prod:EXTRA_HOSTS) echo "www.headmesh.dev" ;;
    prod:API_HOST) echo "api.headmesh.dev" ;;
    prod:CELL_PORT) echo "7443" ;;
    prod:NOINDEX) echo "0" ;;
    prod:PRODUCTION) echo "1" ;;
    test:SITE_HOST) echo "test.headmesh.dev" ;;
    test:EXTRA_HOSTS) echo "" ;;
    test:API_HOST) echo "api-test.headmesh.dev" ;;
    test:CELL_PORT) echo "7444" ;;
    test:NOINDEX) echo "1" ;;
    test:PRODUCTION) echo "0" ;;
    *) echo "unknown environment setting ${env}:${key}" >&2; return 1 ;;
  esac
}

# hm_port <env> <service> <colour> [cell]: loopback port of one instance.
#   waitlist  83xx    backend API  81xx    backend cell endpoint  82xx
#   prod x01/x02 (blue/green), test x11/x12
hm_port() {
  local env="$1" service="$2" colour="$3" kind="${4:-}" base offset
  case "${service}:${kind}" in
    waitlist:) base=8300 ;;
    backend:) base=8100 ;;
    backend:cell) base=8200 ;;
    *) echo "unknown service ${service} ${kind}" >&2; return 1 ;;
  esac
  case "${env}" in prod) offset=0 ;; test) offset=10 ;; *) echo "unknown env ${env}" >&2; return 1 ;; esac
  case "${colour}" in blue) offset=$((offset + 1)) ;; green) offset=$((offset + 2)) ;; *) echo "unknown colour ${colour}" >&2; return 1 ;; esac
  echo $((base + offset))
}

hm_other_colour() {
  case "$1" in blue) echo green ;; green) echo blue ;; *) echo blue ;; esac
}
