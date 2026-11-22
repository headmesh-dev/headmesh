#!/usr/bin/env bash
# Called by restore-isolated.sh inside a fresh network namespace.
set -euo pipefail
umask 077

if ! ip -j link show | jq -e 'length == 1 and .[0].ifname == "lo"' >/dev/null; then
  echo 'The restored cell check requires a loopback-only network namespace.' >&2
  exit 1
fi
ip link set lo up
# Tailscale pauses its control connection while it sees only loopback. Give
# it an interface in this namespace with no peer or route to another network.
ip link add drill0 type dummy
ip addr add 192.0.2.1/32 dev drill0
ip link set drill0 up

work="$1"
runtime="$(mktemp -d /tmp/headmesh-cell.XXXXXXXX)"
hs_pid=""
ts_pid=""
cleanup() {
  if [[ -n "${ts_pid}" ]]; then kill "${ts_pid}" 2>/dev/null || true; wait "${ts_pid}" 2>/dev/null || true; fi
  if [[ -n "${hs_pid}" ]]; then kill "${hs_pid}" 2>/dev/null || true; wait "${hs_pid}" 2>/dev/null || true; fi
  rm -r -- "${runtime}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

url=https://127.0.0.1:8443
config="${runtime}/config.json"
ca="${runtime}/ca.crt"
openssl req -x509 -newkey rsa:2048 -noenc -days 1 -subj '/CN=Headmesh Restore Drill CA' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign' \
  -keyout "${runtime}/ca.key" -out "${ca}" >"${runtime}/tls.log" 2>&1
openssl req -newkey rsa:2048 -noenc -subj '/CN=127.0.0.1' \
  -keyout "${runtime}/server.key" -out "${runtime}/server.csr" >>"${runtime}/tls.log" 2>&1
openssl x509 -req -in "${runtime}/server.csr" -CA "${ca}" -CAkey "${runtime}/ca.key" \
  -days 1 -CAcreateserial -extfile <(printf 'subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n') \
  -out "${runtime}/server.crt" >>"${runtime}/tls.log" 2>&1

# The recovered configuration may point to production services. Start a minimal
# loopback-only server with the verified database and both private keys.
jq -n --arg work "${work}" --arg run "${runtime}" --arg url "${url}" '{
    server_url: $url, listen_addr: "127.0.0.1:8443", metrics_listen_addr: "",
    grpc_listen_addr: "127.0.0.1:50443", unix_socket: ($run + "/headscale.sock"),
    unix_socket_permission: "0600", disable_check_updates: true,
    tls_cert_path: ($run + "/server.crt"), tls_key_path: ($run + "/server.key"),
    noise: {private_key_path: ($work + "/keys/noise_private.key")},
    prefixes: {v4: "100.64.0.0/10", v6: "fd7a:115c:a1e0::/48", allocation: "sequential"},
    derp: {urls: [], paths: [], auto_update_enabled: false, server: {
      enabled: true, region_id: 999, region_code: "drill", region_name: "Restore Drill",
      verify_clients: true, stun_listen_addr: "127.0.0.1:3478",
      private_key_path: ($work + "/keys/derp_server_private.key"), automatically_add_embedded_derp_region: true
    }},
    database: {type: "sqlite", sqlite: {path: ($work + "/headscale.sqlite"), write_ahead_log: true}},
    policy: {mode: "database"}, dns: {magic_dns: false, override_local_dns: false}, logtail: {enabled: false}
  }' >"${config}"
cell() { env -i PATH="${PATH}" headscale -c "${config}" "$@"; }
client() { env -i PATH="${PATH}" tailscale --socket "${runtime}/tailscale.sock" "$@"; }
request() { curl -q --fail --silent --noproxy '*' --max-time 2 --cacert "${ca}" "${url}/$1"; }

env -i PATH="${PATH}" headscale -c "${config}" serve >"${runtime}/headscale.log" 2>&1 &
hs_pid=$!
for _ in {1..20}; do
  if request health >/dev/null; then break; fi
  if ! kill -0 "${hs_pid}" 2>/dev/null; then break; fi
  sleep 1
done
if ! request health >/dev/null; then
  echo 'Headscale failed to serve the restored cell.' >&2
  exit 1
fi

noise="$(request 'key?v=130' | jq -er '.publicKey | select(test("^mkey:[[:xdigit:]]{64}$"))')"
fingerprint="$(printf '%s' "${noise}" | sha256sum | cut -d ' ' -f 1)"
expected="$(jq -er .headscale.noise_public_key_sha256 "${work}/manifest.json")"
if [[ "${fingerprint}" != "${expected}" ]]; then
  echo 'The running Headscale does not serve the manifest Noise identity.' >&2
  exit 1
fi
expected_nodes="$(jq -er .headscale.node_count "${work}/manifest.json")"
restored_nodes="$(cell nodes list -o json | jq -er length)"
if [[ "${restored_nodes}" != "${expected_nodes}" ]]; then
  printf 'Headscale listed %s restored nodes but the manifest recorded %s.\n' "${restored_nodes}" "${expected_nodes}" >&2
  exit 1
fi
cell policy get >"${runtime}/policy.hujson"
cell policy check --file "${runtime}/policy.hujson" >/dev/null
if [[ -n "${2:-}" ]]; then
  bash "$(dirname -- "$0")/check-restored-control.sh" "${runtime}" "$2" "${work}/postgres"
fi

hostname="drill-${runtime##*.}"
hostname="${hostname,,}"
user_id="$(cell users create "${hostname}" -o json | jq -er .id)"
cell preauthkeys create --user "${user_id}" --expiration 5m --ephemeral -o json | jq -er .key >"${runtime}/auth.key"
env -i PATH="${PATH}" SSL_CERT_FILE="${ca}" TS_NO_LOGS_NO_SUPPORT=true tailscaled \
  --tun=userspace-networking --state=mem: --statedir="${runtime}" --socket="${runtime}/tailscale.sock" \
  >"${runtime}/tailscale.log" 2>&1 &
ts_pid=$!
for _ in {1..10}; do
  if [[ -S "${runtime}/tailscale.sock" ]]; then break; fi
  sleep 1
done
if ! client up --login-server="${url}" --auth-key="file:${runtime}/auth.key" --hostname="${hostname}" \
  --accept-dns=false --accept-routes=false --timeout=20s >"${runtime}/registration.log" 2>&1; then
  echo 'The throwaway Tailscale client could not register with the restored Headscale.' >&2
  exit 1
fi
client status --json | jq -e '.BackendState == "Running" and (.Self.TailscaleIPs | length > 0)' >/dev/null
cell nodes list -o json | jq -e --arg name "${hostname}" --argjson count "${expected_nodes}" \
  'length == ($count + 1) and any(.[]; .name == $name)' >/dev/null
printf 'Restored Headscale served the manifest Noise identity and %s nodes, passed policy validation, and registered a temporary Tailscale client.\n' \
  "${restored_nodes}"
