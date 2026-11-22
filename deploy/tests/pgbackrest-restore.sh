#!/usr/bin/env bash
# Exercise P1.7 backup and WAL recovery on a small disposable PostgreSQL 18 cluster.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE="$(awk '/^FROM / { print $2; exit }' "${ROOT}/deploy/tests/Dockerfile")"
IMAGE=headmesh-pgbackrest-restore:local
BIN_DIR="$(mktemp -d)"
trap 'rm -r -- "${BIN_DIR}"' EXIT
(cd "${ROOT}" && CGO_ENABLED=0 go build -trimpath -o "${BIN_DIR}/headmesh-server" ./cmd/headmesh-server)
chmod 0755 "${BIN_DIR}/headmesh-server"

container_args=(--network none)
if (( $# != 0 )); then
  # Called by object-storage-restore.sh with its isolated network and CA.
  [[ $# == 3 ]] || { echo "usage: $0 [test-network test-ca-file sqlite-fixtures]" >&2; exit 2; }
  container_args=(
    --network "$1"
    --mount "type=bind,src=$2,dst=/run/s3-ca.crt,readonly"
    --mount "type=bind,src=${ROOT}/deploy/backup/pgbackrest-r2.conf,dst=/run/pgbackrest-r2.conf,readonly"
    --mount "type=bind,src=$3/offline-primary/db.sqlite,dst=/run/source-headscale.sqlite,readonly"
    --mount "type=bind,src=$3/restored/final.sqlite,dst=/run/restored-headscale.sqlite,readonly"
    --mount "type=bind,src=$3/litestream,dst=/usr/local/bin/litestream,readonly"
    --mount "type=bind,src=${ROOT}/deploy/backup/litestream-r2.yml,dst=/run/litestream.yml,readonly"
    --env HM_DRILL_PG_KEY
    --env HM_DRILL_PG_SECRET
    --env HM_DRILL_SQLITE_KEY
    --env HM_DRILL_SQLITE_SECRET
    --env HM_DRILL_KEYS_KEY
    --env HM_DRILL_KEYS_SECRET
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
 && apt-get install -y --no-install-recommends age ca-certificates curl pgbackrest postgresql-18 sqlite3 \
 && rm -rf /var/lib/apt/lists/*
DOCKERFILE
} | docker build -t "${IMAGE}" -

docker run --rm -i "${container_args[@]}" \
  --mount "type=bind,src=${ROOT}/deploy/backup/postgresql-archive.conf,dst=/run/postgresql-archive.conf,readonly" \
  --mount "type=bind,src=${BIN_DIR}/headmesh-server,dst=/usr/local/bin/headmesh-server,readonly" \
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
export HEADMESH_DATABASE_URL="host=${SOCKET} port=5432 dbname=postgres user=postgres"
HEADMESH_MIGRATE_DATABASE_URL="${HEADMESH_DATABASE_URL}" headmesh-server migrate >/dev/null
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
primary_sql <<'SQL'
INSERT INTO audit_events (occurred_at, actor_type, actor_id, action, target_type, target_id, prev_hash, hash)
VALUES (now(), 'system', 'drill', 'grant.expired', 'grant', 'synthetic-grant',
        decode(repeat('00',32),'hex'), decode(repeat('01',32),'hex'));
SQL
source_sqlite="${ROOT}/headscale.sqlite"
restored_sqlite="${ROOT}/restored-headscale.sqlite"
if [[ -r /run/source-headscale.sqlite ]]; then
  cp /run/source-headscale.sqlite "${source_sqlite}"
  cp /run/restored-headscale.sqlite "${restored_sqlite}"
else
  restored_sqlite="${source_sqlite}"
  sqlite3 -batch -bail "${source_sqlite}" <<'SQL'
CREATE TABLE nodes (id INTEGER PRIMARY KEY, deleted_at TEXT);
CREATE TABLE users (id INTEGER PRIMARY KEY, deleted_at TEXT);
CREATE TABLE policies (id INTEGER PRIMARY KEY, deleted_at TEXT, data TEXT);
INSERT INTO nodes VALUES (1,NULL), (2,'deleted');
INSERT INTO policies VALUES (1,NULL,'{"grants":[]}');
SQL
fi
for kind in noise derp; do
  printf 'privkey:%s\n' "$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" >"${ROOT}/${kind}.key"
done
noise_key="${ROOT}/noise.key"
derp_key="${ROOT}/derp.key"
capture_manifest() {
  headmesh-server backup-manifest capture -sqlite "${source_sqlite}" -noise-key "${noise_key}"
}
if [[ -r /run/litestream.yml ]]; then
  export HEADMESH_R2_ENDPOINT=https://headmesh-backup-s3:8443
  export SSL_CERT_FILE=/run/s3-ca.crt CURL_CA_BUNDLE=/run/s3-ca.crt
  export AWS_SHARED_CREDENTIALS_FILE="${ROOT}/litestream.aws" AWS_EC2_METADATA_DISABLED=true
  printf '[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n' \
    "${HM_DRILL_SQLITE_KEY}" "${HM_DRILL_SQLITE_SECRET}" >"${AWS_SHARED_CREDENTIALS_FILE}"
  printf 'machine headmesh-backup-s3\nlogin %s\npassword %s\n' \
    "${HM_DRILL_PG_KEY}" "${HM_DRILL_PG_SECRET}" >"${ROOT}/manifest.netrc"
  read_published() {
    curl -q --silent --show-error --fail --netrc-file "${ROOT}/manifest.netrc" \
      --aws-sigv4 aws:amz:auto:s3 "${HEADMESH_R2_ENDPOINT}/hm-pg/manifest.json"
  }
  publish_manifest() {
    sed "s|/var/lib/headscale/db.sqlite|$1|" /run/litestream.yml >"${ROOT}/litestream.yml"
    headmesh-server backup-manifest publish -sqlite "$1" -noise-key "${ROOT}/noise.key" \
      -litestream-config "${ROOT}/litestream.yml" -netrc "${ROOT}/manifest.netrc"
  }
  publish_manifest "${source_sqlite}"
  read_published >"${ROOT}/first-published.json"
  publish_manifest "${source_sqlite}"
  read_published >"${ROOT}/published.json"
  if cmp -s "${ROOT}/first-published.json" "${ROOT}/published.json"; then
    echo "The newer evidence did not replace the first published manifest." >&2
    exit 1
  fi
  stale_status="$(curl -q --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --netrc-file "${ROOT}/manifest.netrc" --aws-sigv4 aws:amz:auto:s3 \
    --header 'If-Match: "stale-etag"' --upload-file "${ROOT}/first-published.json" \
    "${HEADMESH_R2_ENDPOINT}/hm-pg/manifest.json")"
  [[ "${stale_status}" == 412 ]]
  cp "${source_sqlite}" "${ROOT}/divergent.sqlite"
  sqlite3 "${ROOT}/divergent.sqlite" "UPDATE policies SET data='{}'"
  if publish_manifest "${ROOT}/divergent.sqlite" >"${ROOT}/divergent.log" 2>&1; then
    echo "A manifest for a policy missing from the replica was published." >&2
    exit 1
  fi
  grep -q 'SQLite replica has not caught up' "${ROOT}/divergent.log"
  read_published >"${ROOT}/readback.json"
  cmp "${ROOT}/published.json" "${ROOT}/readback.json"
  printf '%s\n' 'Conditional publication passed and a divergent SQLite candidate left the previous manifest intact.'

  for identity in owner recovery; do
    age-keygen -o "${ROOT}/${identity}.agekey" >/dev/null 2>&1
  done
  printf 'machine headmesh-backup-s3\nlogin %s\npassword %s\n' \
    "${HM_DRILL_KEYS_KEY}" "${HM_DRILL_KEYS_SECRET}" >"${ROOT}/keys.netrc"
  printf 'server_url: https://hs1.example.test\n' >"${ROOT}/config.yaml"
  publish_keys() {
    headmesh-server backup-keys publish -config "${ROOT}/config.yaml" \
      -noise-key "${noise_key}" -derp-key "${derp_key}" \
      -recipient "$(age-keygen -y "${ROOT}/owner.agekey")" \
      -recovery-recipient "$(age-keygen -y "${ROOT}/recovery.agekey")" -netrc "${ROOT}/keys.netrc"
  }
  stage_keys() {
    headmesh-server backup-keys stage -manifest "${ROOT}/published.json" \
      -identity "${ROOT}/$1.agekey" -output-dir "${ROOT}/keys-$1" -netrc "$2"
  }
  publish_keys
  if stage_keys owner "${ROOT}/manifest.netrc" >"${ROOT}/keys-denied.log" 2>&1; then
    echo "The PostgreSQL credential unexpectedly fetched the Headscale key bundle." >&2
    exit 1
  fi
  grep -q 'refused key bundle download (HTTP 403)' "${ROOT}/keys-denied.log"
  stage_keys owner "${ROOT}/keys.netrc"
  cmp "${ROOT}/config.yaml" "${ROOT}/keys-owner/config.yaml"
  cmp "${noise_key}" "${ROOT}/keys-owner/noise_private.key"
  cmp "${derp_key}" "${ROOT}/keys-owner/derp_server_private.key"
  printf '# configuration revision in this drill\n' >>"${ROOT}/config.yaml"
  publish_keys
  stage_keys recovery "${ROOT}/keys.netrc"
  cmp "${ROOT}/config.yaml" "${ROOT}/keys-recovery/config.yaml"
  cmp "${noise_key}" "${ROOT}/keys-recovery/noise_private.key"
  cmp "${derp_key}" "${ROOT}/keys-recovery/derp_server_private.key"
  noise_key="${ROOT}/keys-recovery/noise_private.key"
  derp_key="${ROOT}/keys-recovery/derp_server_private.key"
  printf '%s\n' 'Both age recipients restored the Headscale bundle from its separate bucket.'
fi
capture_manifest >"${ROOT}/manifest.json"
primary_sql -c "SELECT pg_create_restore_point('headmesh_drill')" >/dev/null
primary_sql -c "INSERT INTO recovery_probe VALUES ('after-target')" >/dev/null
capture_manifest >"${ROOT}/after-target.json"
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
export HEADMESH_DATABASE_URL="host=${RESTORE_SOCKET} port=5433 dbname=postgres user=postgres"
verify_manifest() {
  headmesh-server backup-manifest verify -sqlite "${restored_sqlite}" \
    -noise-key "${noise_key}" -derp-key "${derp_key}" -manifest "$1"
}
verify_manifest "${ROOT}/manifest.json"
if [[ -f "${ROOT}/published.json" ]]; then
  verify_manifest "${ROOT}/published.json"
fi
if verify_manifest "${ROOT}/after-target.json" >"${ROOT}/stale-replica.log" 2>&1; then
  echo "An older restore unexpectedly verified against the later manifest." >&2
  exit 1
fi
grep -q 'has not replayed the manifest WAL position' "${ROOT}/stale-replica.log"
sed -E '/"audit_id": /s/[0-9]+/9999/' "${ROOT}/manifest.json" >"${ROOT}/missing-event.json"
if verify_manifest "${ROOT}/missing-event.json" >"${ROOT}/missing-event.log" 2>&1; then
  echo "A restore unexpectedly verified against an absent grant audit event." >&2
  exit 1
fi
grep -q 'grant audit boundary differs' "${ROOT}/missing-event.log"
printf 'Point-in-time recovery passed in %s seconds. The WAL-only row was restored and the later row was excluded.\n' "${SECONDS}"
pg_ctl -D "${RESTORED}" -m fast -w stop >/dev/null
DRILL
