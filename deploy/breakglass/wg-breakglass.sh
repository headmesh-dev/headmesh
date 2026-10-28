#!/usr/bin/env bash
# Break-glass WireGuard (plan §13.7): a direct path to Headmesh hosts that
# depends on neither Headscale nor the prod-hetzner backdoor hub. Run as root.
#
#   wg-breakglass.sh init <address/cidr> [listen-port]   create key + interface, print public key
#   wg-breakglass.sh add-peer <name> <pubkey> <allowed-ips> [endpoint]
#   wg-breakglass.sh status
#
# Addressing (10.98.0.0/24, separate from the 10.99.0.0/24 backdoor):
#   .1 hm-prod-1 (listens on UDP 51820)   .2 Mac   .4 ASUS
# Private keys are generated on each host and never leave it.
set -Eeuo pipefail

readonly IFACE="wg-breakglass"
readonly CONF="/etc/wireguard/${IFACE}.conf"

log() { printf '[breakglass] %s\n' "$*"; }
die() { printf '[breakglass] ERROR: %s\n' "$*" >&2; exit 1; }

[[ "${EUID}" -eq 0 ]] || die "run as root"
command -v wg >/dev/null || die "install wireguard-tools first"

apply() {
  if systemctl is-active --quiet "wg-quick@${IFACE}"; then
    wg syncconf "${IFACE}" <(wg-quick strip "${IFACE}")
  else
    systemctl enable --now "wg-quick@${IFACE}" >/dev/null 2>&1
  fi
}

cmd_init() {
  local address="${1:?address/cidr}" port="${2:-}"
  [[ "${address}" =~ ^10\.98\.0\.[0-9]{1,3}/[0-9]{1,2}$ ]] || die "address must be in 10.98.0.0/24"
  umask 077
  install -d -m 0700 /etc/wireguard
  if [[ ! -f "${CONF}" ]]; then
    {
      echo "# Managed by headmesh deploy/breakglass/wg-breakglass.sh (plan §13.7)"
      echo "[Interface]"
      echo "Address = ${address}"
      [[ -n "${port}" ]] && echo "ListenPort = ${port}"
      echo "PrivateKey = $(wg genkey)"
    } >"${CONF}"
    log "created ${CONF}"
  fi
  apply
  awk -F' = ' '/^PrivateKey/ {print $2}' "${CONF}" | wg pubkey
}

cmd_add_peer() {
  local name="${1:?name}" pubkey="${2:?pubkey}" allowed="${3:?allowed-ips}" endpoint="${4:-}"
  [[ -f "${CONF}" ]] || die "run init first"
  [[ "${pubkey}" =~ ^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw480]=$ ]] || die "not a WireGuard public key"
  [[ "${name}" =~ ^[a-z0-9-]+$ ]] || die "peer name: lowercase letters, digits, dashes"
  local tmp
  tmp="$(mktemp)"
  # Drop any previous block for this peer, then append the current one.
  awk -v marker="# peer: ${name}" '
    $0 == marker {skip = 1; next}
    skip && /^# peer: / {skip = 0}
    !skip {print}
  ' "${CONF}" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' >"${tmp}"
  {
    echo
    echo "# peer: ${name}"
    echo "[Peer]"
    echo "PublicKey = ${pubkey}"
    echo "AllowedIPs = ${allowed}"
    if [[ -n "${endpoint}" ]]; then
      echo "Endpoint = ${endpoint}"
      # The side with an endpoint is behind NAT; keep the mapping open.
      echo "PersistentKeepalive = 25"
    fi
  } >>"${tmp}"
  install -m 0600 "${tmp}" "${CONF}"
  rm -f "${tmp}"
  apply
  log "peer ${name} -> ${allowed}${endpoint:+ via ${endpoint}}"
}

case "${1:-}" in
  init) shift; cmd_init "$@" ;;
  add-peer) shift; cmd_add_peer "$@" ;;
  status) wg show "${IFACE}" ;;
  *) die "usage: wg-breakglass.sh init|add-peer|status" ;;
esac
