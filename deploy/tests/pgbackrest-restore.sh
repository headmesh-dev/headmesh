#!/usr/bin/env bash
# Exercise P1.7 backup and WAL recovery on a small disposable PostgreSQL 18 cluster.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE="$(awk '/^FROM / { print $2; exit }' "${ROOT}/deploy/tests/Dockerfile")"
IMAGE=headmesh-pgbackrest-restore:local
container_args=(--network none)
if (( $# != 0 )); then
  # Called by object-storage-restore.sh with its isolated network and CA.
  [[ $# == 2 ]] || { echo "usage: $0 [test-network test-ca-file]" >&2; exit 2; }
  container_args=(
    --network "$1"
    --mount "type=bind,src=$2,dst=/run/s3-ca.crt,readonly"
    --mount "type=bind,src=${ROOT}/deploy/backup/pgbackrest-r2.conf,dst=/run/pgbackrest-r2.conf,readonly"
    --env HM_DRILL_PG_KEY
    --env HM_DRILL_PG_SECRET
    --env HM_DRILL_SQLITE_KEY
    --env HM_DRILL_SQLITE_SECRET
  )
fi

{
  printf 'FROM %s\n' "${BASE}"
  cat <<'DOCKERFILE'
ENV DEBIAN_FRONTEND=noninteractive
# PostgreSQL and pgBackRest follow the Ubuntu LTS packages used on the server.
RUN mkdir -p /etc/postgresql-common \
 && echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates curl pgbackrest postgresql-18 \
 && rm -rf /var/lib/apt/lists/*
DOCKERFILE
} | docker build -t "${IMAGE}" -

docker run --rm -i "${container_args[@]}" \
  --mount "type=bind,src=${ROOT}/deploy/backup/postgresql-archive.conf,dst=/run/postgresql-archive.conf,readonly" \
  --user postgres --entrypoint bash "${IMAGE}" -s <<'DRILL'
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
if [[ -r /run/pgbackrest-r2.conf ]]; then
  cp /run/pgbackrest-r2.conf "${ROOT}/pgbackrest.conf"
  export PGBACKREST_REPO1_S3_ENDPOINT=headmesh-backup-s3
  export PGBACKREST_REPO1_STORAGE_PORT=8443
  export PGBACKREST_REPO1_STORAGE_CA_FILE=/run/s3-ca.crt
  export PGBACKREST_REPO1_S3_KEY="${HM_DRILL_PG_KEY:?}"
  export PGBACKREST_REPO1_S3_KEY_SECRET="${HM_DRILL_PG_SECRET:?}"
  list_bucket() {
    curl --silent --show-error --max-time 2 --output /dev/null --write-out '%{http_code}' \
      --aws-sigv4 aws:amz:auto:s3 --user "$2:$3" \
      --cacert /run/s3-ca.crt "https://headmesh-backup-s3:8443/$1?list-type=2"
  }
  for _ in {1..30}; do
    [[ "$(list_bucket hm-pg "${HM_DRILL_PG_KEY}" "${HM_DRILL_PG_SECRET}" 2>/dev/null)" != 200 ]] || break
    sleep 1
  done
  [[ "$(list_bucket hm-pg "${HM_DRILL_PG_KEY}" "${HM_DRILL_PG_SECRET}")" == 200 ]]
  [[ "$(list_bucket hm-hs1-litestream "${HM_DRILL_PG_KEY}" "${HM_DRILL_PG_SECRET}")" == 403 ]]
  [[ "$(list_bucket hm-hs1-litestream "${HM_DRILL_SQLITE_KEY:?}" "${HM_DRILL_SQLITE_SECRET:?}")" == 200 ]]
  [[ "$(list_bucket hm-pg "${HM_DRILL_SQLITE_KEY}" "${HM_DRILL_SQLITE_SECRET}")" == 403 ]]
  printf '%s\n' 'Verified S3 TLS and both bucket credential scopes.'
else
  cat >"${ROOT}/pgbackrest.conf" <<CONF
[global]
repo1-path=${ROOT}/repo
repo1-retention-full=4
repo1-cipher-type=aes-256-cbc
CONF
fi
export PGBACKREST_REPO1_CIPHER_PASS="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
export PGBACKREST_LOCK_PATH="${ROOT}/lock"
export PGBACKREST_ARCHIVE_TIMEOUT=30
export PGBACKREST_LOG_LEVEL_CONSOLE=warn
export PGBACKREST_LOG_LEVEL_FILE=off
export PGBACKREST_PG1_PATH="${PRIMARY}"
export PGBACKREST_PG1_SOCKET_PATH="${SOCKET}"
export PGBACKREST_CONFIG="${ROOT}/pgbackrest.conf"
cat >>"${PRIMARY}/postgresql.conf" <<CONF
listen_addresses = ''
unix_socket_directories = '${SOCKET}'
CONF
cat /run/postgresql-archive.conf >>"${PRIMARY}/postgresql.conf"

backup() { pgbackrest --stanza=headmesh "$@"; }
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
