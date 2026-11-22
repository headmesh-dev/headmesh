#!/usr/bin/env bash
# Run from an independent client after verify-cutover. The verification command
# receives only the temporary client's hostname, public key and assigned IPs on stdin.
set -euo pipefail
umask 077

if [[ $# -lt 3 || "$2" != -- || ! -f "$1" || ! -s "$1" ]]; then
  echo 'usage: check-recovered-client.sh <ephemeral-auth-key-file> -- <standby verifier command and args>' >&2
  exit 2
fi
key_file="$(realpath -- "$1")"
shift 2
runtime="$(mktemp -d /tmp/hm-client.XXXXXXXXXX)"
hostname="hm-recovery-${runtime##*.}"
hostname="${hostname,,}"

client_env=(env -i "PATH=${PATH}" TS_NO_LOGS_NO_SUPPORT=true)
if [[ -n "${SSL_CERT_FILE:-}" ]]; then client_env+=("SSL_CERT_FILE=${SSL_CERT_FILE}"); fi
client() { "${client_env[@]}" tailscale --socket "${runtime}/tailscale.sock" "$@"; }
pid=
registered=false
cleanup() {
  local status="$1"
  trap - EXIT INT TERM
  if "${registered}" && ! timeout 10s "${client_env[@]}" tailscale --socket "${runtime}/tailscale.sock" logout \
    >"${runtime}/logout.log" 2>&1; then
    echo 'Tailscale logout failed. The ephemeral node will expire after it goes offline.' >&2
    if (( status == 0 )); then status=1; fi
  fi
  if [[ -n "${pid}" ]]; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
  rm -r -- "${runtime}"
  exit "${status}"
}
trap 'cleanup "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"${client_env[@]}" tailscaled --tun=userspace-networking --state=mem: --statedir="${runtime}" \
  --socket="${runtime}/tailscale.sock" >"${runtime}/daemon.log" 2>&1 &
pid=$!
for _ in {1..10}; do
  [[ -S "${runtime}/tailscale.sock" ]] && break
  kill -0 "${pid}" 2>/dev/null || break
  sleep 1
done
if [[ ! -S "${runtime}/tailscale.sock" ]]; then
  echo 'The userspace Tailscale daemon did not start.' >&2
  exit 1
fi
if ! client up --login-server=https://hs1.headmesh.dev --auth-key="file:${key_file}" \
  --hostname="${hostname}" --accept-dns=false --accept-routes=false --timeout=30s \
  >"${runtime}/registration.log" 2>&1; then
  echo 'The temporary Tailscale client could not register with public Headscale.' >&2
  exit 1
fi
registered=true

# Keep the control session alive while the standby finds the node in Headscale.
if ! client status --json | jq -ce --arg name "${hostname}" '
  select(.BackendState == "Running" and .Self.HostName == $name)
  | {hostname: .Self.HostName, node_key: .Self.PublicKey, ips: .Self.TailscaleIPs}
  | select((.node_key | test("^nodekey:[0-9a-f]{64}$")) and (.ips | length > 0))
' >"${runtime}/client.json"; then
  echo 'The registered client did not report an online node identity and assigned addresses.' >&2
  exit 1
fi

timeout 60s "$@" <"${runtime}/client.json"
