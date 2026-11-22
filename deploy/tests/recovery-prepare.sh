#!/usr/bin/env bash
# Test the recovery bootstrap on a host that has never run the deploy bootstrap.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
image="${1:-headmesh-deploy-e2e}"
fixture="hm-recovery-prepare-$$"
toolkit="$(mktemp -d)"
cleanup() {
  docker rm -f "${fixture}" >/dev/null 2>&1 || true
  rm -rf -- "${toolkit}"
}
trap cleanup EXIT
if [[ $# == 0 ]]; then docker build -q -t "${image}" "${root}/deploy/tests" >/dev/null; fi
docker run -d --name "${fixture}" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock "${image}" >/dev/null

on() { docker exec "${fixture}" "$@"; }
wait_systemd() {
  local state
  for _ in {1..60}; do
    state="$(on systemctl is-system-running 2>/dev/null || true)"
    [[ "${state}" == running || "${state}" == degraded ]] && return 0
    sleep 1
  done
  echo "systemd did not boot: ${state}" >&2
  return 1
}
wait_systemd

HM_DEPLOY_LOCAL_ENV=/dev/null "${root}/deploy/deploy.sh" --package-recovery-toolkit "${toolkit}"
tar -C "${toolkit}" -cf - . |
  docker exec -i "${fixture}" bash -ec 'install -d -m 0755 /usr/local/lib/headmesh; tar -xf - --no-same-owner -C /usr/local/lib/headmesh'
bootstrap=/usr/local/lib/headmesh/bootstrap.sh
on "${bootstrap}" prepare-recovery
on "${bootstrap}" prepare-recovery

assert_held_offline() {
  docker exec -i "${fixture}" bash -se <<'CHECK'
for unit in nginx.service headscale.service; do
  [[ "$(readlink "/etc/systemd/system/${unit}")" == /dev/null ]]
  [[ "$(systemctl show -p ActiveState --value "${unit}")" == inactive ]]
done
for unit in headmesh-backend@prod-blue.service headmesh-cellagent.service certbot-renew.timer headmesh-backup@prod.timer; do
  [[ "$(systemctl show -p ActiveState --value "${unit}")" == inactive ]]
done
[[ -z "$(pg_lsclusters --no-header)" && ! -e /var/lib/postgresql/18/main ]]
[[ "$(pg_conftool -s /etc/postgresql-common/createcluster.conf show create_main_cluster)" == false ]]
[[ ! -d /var/lib/headscale || -z "$(find /var/lib/headscale -mindepth 1 -print -quit)" ]]
[[ ! -e /etc/headmesh/tls/fullchain.pem && ! -e /etc/headmesh/prod/db.env ]]
[[ -z "$(ss -Hltn '( sport = :80 or sport = :443 or sport = :5432 or sport = :8080 )')" ]]
CHECK
}
assert_held_offline
on env LC_ALL=en_US.UTF-8 locale charmap | grep -qx UTF-8
on test -x /usr/lib/postgresql/18/bin/pg_ctl
on test -x /opt/node/bin/node
on /opt/certbot/bin/certbot plugins --text | grep -q dns-cloudflare
on headscale version
on id -u headmesh-cellagent >/dev/null
on test -r /etc/systemd/system/headmesh-backend@.service
on test -r /usr/local/lib/headmesh/cellagent/headmesh-cellagent.service

docker restart "${fixture}" >/dev/null
wait_systemd
assert_held_offline

# A host with state must be refused before the preparation changes it.
on bash -ec 'install -d /var/lib/headscale; printf "%s\n" existing-state >/var/lib/headscale/db.sqlite'
if result="$(on "${bootstrap}" prepare-recovery 2>&1)"; then exit 1; fi
grep -q 'Headscale state already exists' <<<"${result}"
on grep -qx existing-state /var/lib/headscale/db.sqlite
echo 'Fresh standby preparation was repeatable, held services offline across reboot and refused an existing database.'
