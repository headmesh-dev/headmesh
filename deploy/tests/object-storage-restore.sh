#!/usr/bin/env bash
# Exercise the R2 backup configurations against an isolated TLS S3 endpoint.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
S3=chrislusf/seaweedfs:4.54@sha256:4b42c212543a0acd4c0e675215ecdcaf62a186ef98def76c7e12701871222d15
LITESTREAM=litestream/litestream:0.5.17@sha256:4b02b9859a6b6b4087d8b8944e15f7e984bd7957cba322bbeee38b0e27b9656a
WORK="$(mktemp -d)"
NETWORK="hm-drill-${WORK##*.}"
S3_CONTAINER="${NETWORK}-s3"
REPLICATOR="${NETWORK}-litestream"
S3_HOST=headmesh-backup-s3
DB=/var/lib/headscale/db.sqlite

cleanup() {
  local status=$?
  if (( status != 0 )); then
    docker logs --tail 25 "${REPLICATOR}" >&2 || true
    docker logs --tail 25 "${S3_CONTAINER}" >&2 || true
  fi
  docker stop -t 3 "${REPLICATOR}" "${S3_CONTAINER}" >/dev/null 2>&1 || true
  docker rm -v "${REPLICATOR}" "${S3_CONTAINER}" >/dev/null 2>&1 || true
  docker network rm "${NETWORK}" >/dev/null 2>&1 || true
  rm -r -- "${WORK}"
}
trap cleanup EXIT

export HM_DRILL_PG_KEY=headmesh-postgres-drill
export HM_DRILL_PG_SECRET="$(openssl rand -hex 20)"
export HM_DRILL_SQLITE_KEY=headmesh-sqlite-drill
export HM_DRILL_SQLITE_SECRET="$(openssl rand -hex 20)"
export HM_DRILL_KEYS_KEY=headmesh-keys-drill
export HM_DRILL_KEYS_SECRET="$(openssl rand -hex 20)"
export HEADMESH_R2_ENDPOINT="https://${S3_HOST}:8443"
export SSL_CERT_FILE=/run/drill-ca.crt

python3 - "${WORK}/iam.json" "${WORK}/litestream.aws" <<'PY'
import json
import os
from pathlib import Path
import sys

identities = []
for bucket, access, secret in (
    ("hm-pg", "HM_DRILL_PG_KEY", "HM_DRILL_PG_SECRET"),
    ("hm-hs1-litestream", "HM_DRILL_SQLITE_KEY", "HM_DRILL_SQLITE_SECRET"),
    ("hm-keys", "HM_DRILL_KEYS_KEY", "HM_DRILL_KEYS_SECRET"),
):
    identities.append({
        "name": bucket,
        "credentials": [{"accessKey": os.environ[access], "secretKey": os.environ[secret]}],
        "actions": [f"{action}:{bucket}" for action in ("Read", "List", "Tagging", "Write")],
    })
Path(sys.argv[1]).write_text(json.dumps({"identities": identities}))
Path(sys.argv[2]).write_text(
    "[default]\n"
    f"aws_access_key_id = {os.environ['HM_DRILL_SQLITE_KEY']}\n"
    f"aws_secret_access_key = {os.environ['HM_DRILL_SQLITE_SECRET']}\n"
)
PY
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${S3_HOST}" \
  -addext "subjectAltName=DNS:${S3_HOST}" -keyout "${WORK}/server.key" -out "${WORK}/ca.crt" >/dev/null 2>&1
chmod 0644 "${WORK}/ca.crt"
mkdir "${WORK}/primary" "${WORK}/restored"
docker network create --internal "${NETWORK}" >/dev/null
docker run -d --name "${S3_CONTAINER}" --network "${NETWORK}" --network-alias "${S3_HOST}" \
  --tmpfs /data:rw,size=384m --mount "type=bind,src=${WORK},dst=/sandbox,readonly" "${S3}" \
  mini -dir=/data -volume.max=6 -master.volumeSizeLimitMB=128 -bucket=hm-pg,hm-hs1-litestream,hm-keys \
  -ip.bind=0.0.0.0 -s3.port.https=8443 -s3.cert.file=/sandbox/ca.crt -s3.key.file=/sandbox/server.key \
  -s3.config=/sandbox/iam.json >/dev/null

sqlite_fixture() {
  python3 - "${WORK}/primary/db.sqlite" "$1" <<'PY'
import sqlite3
import sys

with sqlite3.connect(sys.argv[1]) as database:
    assert database.execute("PRAGMA journal_mode=WAL").fetchone() == ("wal",)
    if sys.argv[2] == "initial":
        database.executescript("""
            CREATE TABLE nodes (id INTEGER PRIMARY KEY, deleted_at TEXT);
            CREATE TABLE users (id INTEGER PRIMARY KEY, deleted_at TEXT);
            CREATE TABLE policies (id INTEGER PRIMARY KEY, deleted_at TEXT, data TEXT);
            CREATE TABLE recovery_probe (entry TEXT PRIMARY KEY);
            INSERT INTO policies VALUES (1,NULL,'{"grants":[]}');
        """)
    database.execute("INSERT INTO nodes DEFAULT VALUES")
    database.execute("INSERT INTO recovery_probe VALUES (?)", (sys.argv[2],))
PY
}

sqlite_entries() {
  python3 - "$1" <<'PY'
import sqlite3
import sys

with sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True) as database:
    assert database.execute("PRAGMA integrity_check").fetchone() == ("ok",)
    print(",".join(row[0] for row in database.execute("SELECT entry FROM recovery_probe ORDER BY entry")))
PY
}

litestream_args=(
  --network "${NETWORK}" --user "$(id -u):$(id -g)"
  --env AWS_SHARED_CREDENTIALS_FILE=/run/drill.aws --env AWS_EC2_METADATA_DISABLED=true
  --env HEADMESH_R2_ENDPOINT --env SSL_CERT_FILE
  --mount "type=bind,src=${WORK}/litestream.aws,dst=/run/drill.aws,readonly"
  --mount "type=bind,src=${WORK}/ca.crt,dst=/run/drill-ca.crt,readonly"
  --mount "type=bind,src=${ROOT}/deploy/backup/litestream-r2.yml,dst=/run/litestream.yml,readonly"
)
restore_sqlite() {
  docker run --rm "${litestream_args[@]}" \
    --mount "type=bind,src=${WORK}/restored,dst=/restore" "${LITESTREAM}" \
    restore -config /run/litestream.yml -o "/restore/$1" "${DB}"
}

wait_for_entries() {
  local expected="$1" attempt candidate
  for attempt in {1..25}; do
    candidate="probe-${expected//,/}-${attempt}.sqlite"
    if restore_sqlite "${candidate}" >"${WORK}/sqlite-restore.log" 2>&1 \
      && [[ "$(sqlite_entries "${WORK}/restored/${candidate}")" == "${expected}" ]]; then
      return 0
    fi
    sleep 2
  done
  cat "${WORK}/sqlite-restore.log" >&2
  return 1
}

sqlite_fixture initial
docker run -d --name "${REPLICATOR}" "${litestream_args[@]}" \
  --mount "type=bind,src=${WORK}/primary,dst=/var/lib/headscale" "${LITESTREAM}" \
  replicate -config /run/litestream.yml >/dev/null
wait_for_entries initial
sqlite_fixture later
wait_for_entries initial,later
docker stop -t 8 "${REPLICATOR}" >/dev/null
python3 - "${WORK}/primary/db.sqlite" <<'PY'
import sqlite3
import sys

with sqlite3.connect(sys.argv[1]) as database:
    assert database.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()[0] == 0
PY
mv "${WORK}/primary" "${WORK}/offline-primary"
restore_sqlite final.sqlite
[[ "$(sqlite_entries "${WORK}/restored/final.sqlite")" == initial,later ]]
printf '%s\n' 'Litestream restored both SQLite transactions over TLS with the source offline.'
chmod 0644 "${WORK}/offline-primary/db.sqlite" "${WORK}/restored/final.sqlite"
docker run --rm --network none --entrypoint /bin/cat "${LITESTREAM}" /usr/local/bin/litestream >"${WORK}/litestream"
chmod 0755 "${WORK}/litestream"
"${ROOT}/deploy/tests/pgbackrest-restore.sh" "${NETWORK}" "${WORK}/ca.crt" "${WORK}"
