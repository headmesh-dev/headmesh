#!/usr/bin/env bash
# Restore and verify the R2 backups on a disposable recovery host.
set -euo pipefail
umask 077

check_cell=0
check_control=0
stage=0
retain=0
if [[ "${1:-}" == --check-control-plane ]]; then
  check_cell=1
  check_control=1
  shift
elif [[ "${1:-}" == --check-cell ]]; then
  check_cell=1
  shift
elif [[ "${1:-}" == --stage-for-recovery ]]; then
  stage=1
  shift
fi
if [[ $# != 5 || ${EUID} == 0 ]]; then
  echo "Run as a non-root user: $0 [--check-cell|--check-control-plane|--stage-for-recovery] WORK_PARENT LITESTREAM_CONFIG PG_NETRC KEYS_NETRC AGE_IDENTITY" >&2
  exit 2
fi
work_parent="$(realpath -e -- "$1")"
if (( stage )) && [[ ! -d "${work_parent}" || "$(stat -c '%u:%a' -- "${work_parent}")" != "${EUID}:700" ]]; then
  echo 'The recovery staging parent must be owned by the caller with mode 0700.' >&2
  exit 2
fi
litestream_config="$2"
pg_netrc="$3"
keys_netrc="$4"
identity="$5"
startup_timeout="${PGCTLTIMEOUT:-900}"
if [[ ! "${startup_timeout}" =~ ^[1-9][0-9]*$ ]]; then
  echo 'PGCTLTIMEOUT must be a positive number of seconds.' >&2
  exit 2
fi
work=""
socket=""
pg_start_attempted=0

cleanup() {
  local result=$?
  if (( pg_start_attempted )) && pg_ctl -D "${work}/postgres" status >/dev/null 2>&1; then
    if ! pg_ctl -D "${work}/postgres" -m fast -w stop >/dev/null; then
      printf 'Could not stop the drill database. Private data remains in %s and %s.\n' "${work}" "${socket}" >&2
      exit 1
    fi
  fi
  if (( result != 0 )) && [[ -n "${work}" && -f "${work}/postgres.log" ]]; then
    tail -n 25 "${work}/postgres.log" >&2
  fi
  if (( result != 0 || ! retain )); then
    [[ -z "${work}" ]] || rm -r -- "${work}"
  fi
  [[ -z "${socket}" ]] || rm -r -- "${socket}"
  exit "${result}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
work="$(mktemp -d -- "${work_parent%/}/headmesh-restore.XXXXXXXX")"
socket="$(mktemp -d /tmp/headmesh-drill.XXXXXXXX)"
SECONDS=0

manifest="${work}/manifest.json"
headmesh-server backup-manifest fetch -netrc "${pg_netrc}" >"${manifest}"
fetched="$(date +%s)"
captured="$(date -d "$(jq -er .captured_at "${manifest}")" +%s)"
if (( captured > fetched )); then
  echo 'The recovery manifest timestamp is in the future. Check clock sync and run again.' >&2
  exit 1
fi
headmesh-server backup-keys stage -manifest "${manifest}" -identity "${identity}" \
  -netrc "${keys_netrc}" -output-dir "${work}/keys"
litestream restore -config "${litestream_config}" -o "${work}/headscale.sqlite" /var/lib/headscale/db.sqlite

mkdir "${work}/postgres" "${work}/tablespaces" "${work}/lock"
export PGBACKREST_ARCHIVE_ASYNC=n
pgbackrest --stanza=headmesh --repo=1 --type=default --no-delta \
  --pg1-path="${work}/postgres" \
  --lock-path="${work}/lock" --tablespace-map-all="${work}/tablespaces" \
  --log-level-file=off restore

# Use a private local socket and a fresh server config. Disable streaming,
# archiving and recovery commands that could affect another system.
touch "${socket}/postgresql.conf" "${socket}/pg_ident.conf"
printf 'local all postgres trust\n' >"${socket}/pg_hba.conf"
pg_start_attempted=1
replay_deadline=$((SECONDS + startup_timeout))
pg_ctl -D "${work}/postgres" -l "${work}/postgres.log" -w -t "${startup_timeout}" \
  -o "-c config_file=${socket}/postgresql.conf -c hba_file=${socket}/pg_hba.conf -c ident_file=${socket}/pg_ident.conf \
    -c listen_addresses= -c unix_socket_directories=${socket} -c unix_socket_permissions=0700 -c port=5432 \
    -c hot_standby=off -c archive_mode=off -c primary_conninfo= -c primary_slot_name= -c archive_cleanup_command= \
    -c recovery_end_command= -c shared_preload_libraries= -c logging_collector=off \
    -c external_pid_file=${socket}/postgres.pid" start >/dev/null
while ! pg_isready -q -h "${socket}" -p 5432 -U postgres -t 1; do
  if (( SECONDS >= replay_deadline )) || ! pg_ctl -D "${work}/postgres" status >/dev/null 2>&1; then
    echo 'The isolated PostgreSQL server did not finish WAL replay.' >&2
    exit 1
  fi
  sleep 1
done

database="$(jq -er '.postgres_database | @uri' "${manifest}")"
database_url="postgresql://postgres@/${database}?host=${socket}&port=5432&sslmode=disable"
HEADMESH_DATABASE_URL="${database_url}" \
  headmesh-server backup-manifest verify -manifest "${manifest}" -sqlite "${work}/headscale.sqlite" \
  -noise-key "${work}/keys/noise_private.key" -derp-key "${work}/keys/derp_server_private.key"
printf 'Backup verification completed in %d seconds; manifest age at download was %d seconds.\n' \
  "${SECONDS}" "$((fetched - captured))"
if (( check_cell )); then
  script_dir="$(dirname -- "$(realpath -e -- "$0")")"
  args=("${work}")
  if (( check_control )); then args+=("${database_url}"); fi
  unshare --user --map-root-user --net bash "${script_dir}/check-restored-cell.sh" "${args[@]}"
  jq -cn --argjson verification "${SECONDS}" --argjson age "$((fetched - captured))" \
    '{verification_seconds: $verification, manifest_age_seconds: $age}'
elif (( stage )); then
  pg_ctl -D "${work}/postgres" -m fast -w stop >/dev/null
  pg_start_attempted=0
  jq -cn --arg path "${work}" --argjson verification "${SECONDS}" --argjson age "$((fetched - captured))" \
    '{staged_directory: $path, verification_seconds: $verification, manifest_age_seconds: $age}'
  retain=1
fi
