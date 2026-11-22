#!/usr/bin/env bash
# Exercise public client registration with the restored deploy fixture and a separate runner.
set -euo pipefail
umask 077
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$1"
image="$2"
[[ "${fixture}" == hm-deploy-e2e-* ]]
on() { docker exec "${fixture}" "$@"; }
[[ "$(on systemd-detect-virt --container)" == docker ]]
# shellcheck source=../backup/tool-images.sh
source "${root}/deploy/backup/tool-images.sh"

work="$(mktemp -d)"
client="${fixture}-client"
network="${client}"
tools_container=
cleanup() {
  docker rm -f "${client}" >/dev/null 2>&1 || true
  if [[ -n "${tools_container}" ]]; then docker rm "${tools_container}" >/dev/null 2>&1 || true; fi
  docker network disconnect "${network}" "${fixture}" >/dev/null 2>&1 || true
  docker network rm "${network}" >/dev/null 2>&1 || true
  rm -rf -- "${work}"
}
trap cleanup EXIT
tools_container="$(docker create --entrypoint /bin/true "${TAILSCALE}")"
mkdir "${work}/bin"
for tool in tailscale tailscaled; do
  docker cp "${tools_container}:/usr/local/bin/${tool}" "${work}/bin/${tool}"
done
on cat /etc/headmesh/tls/fullchain.pem >"${work}/ca.pem"
test_user="e2e-recovery-${work##*.}"
user_id="$(on headscale users create "${test_user,,}" -o json | jq -er '.id')"
on headscale preauthkeys create --user "${user_id}" --expiration 5m --ephemeral -o json | jq -er '.key' >"${work}/auth.key"
docker network create --internal "${network}" >/dev/null
docker network connect "${network}" "${fixture}"
ip="$(docker inspect -f '{{json .NetworkSettings.Networks}}' "${fixture}" | jq -er --arg name "${network}" '.[$name].IPAddress')"
mkfifo "${work}/request" "${work}/response"

docker run --rm --name "${client}" --user "$(id -u):$(id -g)" --network "${network}" \
  --add-host "hs1.headmesh.dev:${ip}" --entrypoint /bin/bash \
  -v "${work}:/proof" -v "${root}/deploy/ha/check-recovered-client.sh:/check-client:ro" "${image}" -ec '
    export PATH="/proof/bin:${PATH}" SSL_CERT_FILE=/proof/ca.pem
    exec /check-client /proof/auth.key -- bash -ec '\''cat >/proof/request; read -r outcome </proof/response; exit "${outcome}"'\''
  ' >"${work}/runner.log" 2>&1 &
runner_pid=$!

if ! timeout 40s head -n 1 "${work}/request" >"${work}/client.json"; then
  cat "${work}/runner.log" >&2
  exit 1
fi
verify() { docker exec -i "${fixture}" /usr/local/lib/headmesh/deployctl.sh verify-recovery-cell; }
result=0
verify <"${work}/client.json" || result=$?
if (( result == 0 )); then
  if jq -c '.node_key = ("nodekey:" + ("0" * 64))' "${work}/client.json" | verify >"${work}/unknown.log" 2>&1 ||
    ! grep -q 'temporary client is missing or mismatched' "${work}/unknown.log"; then
    cat "${work}/unknown.log" >&2
    result=1
  fi
fi
if (( result == 0 )); then
  # Change the running policy's bytes while preserving its semantics.
  set_policy() { docker exec -i "${fixture}" headscale policy set --file /dev/stdin >/dev/null; }
  on headscale policy get | head -c -1 >"${work}/policy.hujson"
  { cat "${work}/policy.hujson"; printf '\n'; } | set_policy
  if verify <"${work}/client.json" >"${work}/drift.log" 2>&1 ||
    ! grep -q 'policy served by Headscale differs' "${work}/drift.log"; then
    cat "${work}/drift.log" >&2
    result=1
  fi
  set_policy <"${work}/policy.hujson"
  if (( result == 0 )); then verify <"${work}/client.json" || result=$?; fi
fi
printf '%s\n' "${result}" | timeout 5s tee "${work}/response" >/dev/null
if ! wait "${runner_pid}"; then
  cat "${work}/runner.log" >&2
  exit 1
fi
node_key="$(jq -er .node_key "${work}/client.json")"
on headscale nodes list -o json | jq -e --arg key "${node_key}" \
  'all(.[] | select(.node_key == $key); .online != true and .pre_auth_key.ephemeral == true)' >/dev/null
echo 'A separate client registered and logged out. The verifier rejected an unknown node and a changed live policy.'
