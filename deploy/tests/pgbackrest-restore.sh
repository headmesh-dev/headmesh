#!/usr/bin/env bash
# Exercise P1.7 backup and WAL recovery on a small disposable PostgreSQL 18 cluster.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE="$(awk '/^FROM / { print $2; exit }' "${ROOT}/deploy/tests/Dockerfile")"
IMAGE=headmesh-pgbackrest-restore:local

{
  printf 'FROM %s\n' "${BASE}"
  cat <<'DOCKERFILE'
ENV DEBIAN_FRONTEND=noninteractive
# PostgreSQL and pgBackRest follow the Ubuntu LTS packages used on the server.
RUN mkdir -p /etc/postgresql-common \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates pgbackrest postgresql-18 \
 && rm -rf /var/lib/apt/lists/*
DOCKERFILE
} | docker build -t "${IMAGE}" -

docker run --rm -i --network none --user postgres --entrypoint bash "${IMAGE}" -s <<'DRILL'
set -euo pipefail
umask 077
export PATH="/usr/lib/postgresql/18/bin:${PATH}"
ROOT="$(mktemp -d)"
PRIMARY="${ROOT}/primary"
RESTORED="${ROOT}/restored"
SOCKET="${ROOT}/socket"
RESTORE_SOCKET="${ROOT}/restore-socket"
mkdir "${SOCKET}" "${RESTORE_SOCKET}" "${RESTORED}" "${ROOT}/repo" "${ROOT}/lock"

diagnostics() {
  local status=$?
  if (( status != 0 )); then
    for log in "${ROOT}/primary.log" "${ROOT}/restored.log"; do
      [[ ! -f "${log}" ]] || tail -n 25 "${log}" >&2
    done
  fi
}
trap diagnostics EXIT

pgbackrest --version
postgres --version
initdb -D "${PRIMARY}" --no-locale --auth-local=trust --auth-host=reject >/dev/null
cat >"${ROOT}/pgbackrest.conf" <<CONF
[global]
repo1-path=${ROOT}/repo
repo1-retention-full=4
repo1-cipher-type=aes-256-cbc
repo1-cipher-pass=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
lock-path=${ROOT}/lock
archive-timeout=30
log-level-console=warn
log-level-file=off

[headmesh]
pg1-path=${PRIMARY}
pg1-socket-path=${SOCKET}
CONF
cat >>"${PRIMARY}/postgresql.conf" <<CONF
listen_addresses = ''
unix_socket_directories = '${SOCKET}'
archive_mode = on
archive_timeout = 300
archive_command = 'pgbackrest --config=${ROOT}/pgbackrest.conf --stanza=headmesh archive-push "%p"'
CONF

backup() { pgbackrest --config="${ROOT}/pgbackrest.conf" --stanza=headmesh "$@"; }
restore() { backup --pg1-path="${RESTORED}" --type=name --target=headmesh_drill --target-action=promote restore; }
primary_sql() { psql -XqAt -v ON_ERROR_STOP=1 -h "${SOCKET}" -d postgres "$@"; }
restored_sql() { psql -XqAt -v ON_ERROR_STOP=1 -h "${RESTORE_SOCKET}" -p 5433 -d postgres "$@"; }

pg_ctl -D "${PRIMARY}" -l "${ROOT}/primary.log" -w start >/dev/null
backup stanza-create
backup check
primary_sql -c "CREATE TABLE recovery_probe (step text PRIMARY KEY); INSERT INTO recovery_probe VALUES ('full')"
backup --type=full backup
primary_sql -c "INSERT INTO recovery_probe VALUES ('differential')"
backup --type=diff backup
inventory="$(backup info)"
grep -q 'full backup:' <<<"${inventory}"
grep -q 'diff backup:' <<<"${inventory}"
printf '%s\n' 'Encrypted full and differential backups present.'

primary_sql -c "INSERT INTO recovery_probe VALUES ('wal-only')" >/dev/null
primary_sql -c "SELECT pg_create_restore_point('headmesh_drill')" >/dev/null
primary_sql -c "INSERT INTO recovery_probe VALUES ('after-target')" >/dev/null
backup check
pg_ctl -D "${PRIMARY}" -m fast -w stop >/dev/null
mv "${PRIMARY}" "${ROOT}/offline-primary"

if PGBACKREST_REPO1_CIPHER_PASS=incorrect restore >"${ROOT}/wrong-key.log" 2>&1; then
  printf '%s\n' 'An incorrect repository key unexpectedly restored the database.' >&2
  exit 1
fi
grep -q 'unable to load info file' "${ROOT}/wrong-key.log" || {
  cat "${ROOT}/wrong-key.log" >&2
  exit 1
}
printf '%s\n' 'An incorrect repository key was rejected.'

SECONDS=0
restore
pg_ctl -D "${RESTORED}" -l "${ROOT}/restored.log" \
  -o "-k ${RESTORE_SOCKET} -p 5433 -c archive_mode=off" -w start >/dev/null
for _ in {1..30}; do
  [[ "$(restored_sql -c 'SELECT pg_is_in_recovery()')" != f ]] || break
  sleep 1
done
[[ "$(restored_sql -c 'SELECT pg_is_in_recovery()')" == f ]]
[[ "$(restored_sql -c 'SELECT step FROM recovery_probe ORDER BY step')" == $'differential\nfull\nwal-only' ]]
printf 'Point-in-time recovery passed in %s seconds. The WAL-only row was restored and the later row was excluded.\n' "${SECONDS}"
pg_ctl -D "${RESTORED}" -m fast -w stop >/dev/null
DRILL
