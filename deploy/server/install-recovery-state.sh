#!/usr/bin/env bash
# Install a verified restore on a fresh standby. All services stay offline.
set -Eeuo pipefail
umask 077

die() { printf '[recovery-state] %s\n' "$*" >&2; exit 1; }

[[ $# == 1 && ${EUID} == 0 ]] || die "usage (as root): $0 STAGED_DIRECTORY"
stage="$(realpath -e -- "$1")"
pguser="$(id -u postgres)"
id -u headscale >/dev/null
[[ -d "${stage}" && "$(stat -c '%u:%a' -- "${stage}")" == "${pguser}:700" ]] ||
  die 'the verified stage must be owned by postgres with mode 0700'
for file in manifest.json headscale.sqlite keys/config.yaml keys/noise_private.key keys/derp_server_private.key postgres/PG_VERSION; do
  [[ -f "${stage}/${file}" && ! -L "${stage}/${file}" && -s "${stage}/${file}" ]] || die "missing stage file: ${file}"
done
for dir in postgres keys tablespaces; do
  [[ -d "${stage}/${dir}" && ! -L "${stage}/${dir}" ]] || die "missing stage directory: ${dir}"
done

readonly pg_version=18
readonly pg_parent=/var/lib/postgresql/18
readonly pg_data="${pg_parent}/main"
readonly pg_tablespaces="${pg_parent}/headmesh-tablespaces"
readonly pg_config=/etc/postgresql/18/main
readonly hs_data=/var/lib/headscale
readonly recovery_record=/var/lib/headmesh/recovery

install -d -m 0700 /run/headmesh-recovery-state
exec 9>/run/headmesh-recovery-state/lock
flock -n 9 || die 'another recovery state installation is in progress'
[[ "$(readlink /etc/systemd/system/headscale.service || true)" == /dev/null ]] ||
  die 'mask headscale.service while preparing the fresh standby'
clusters="$(pg_lsclusters --no-header)"
[[ -z "${clusters}" ]] || die 'a PostgreSQL cluster is already configured on this host'
for dest in "${pg_data}" "${pg_config}" "${pg_tablespaces}" "${recovery_record}"; do
  [[ ! -e "${dest}" && ! -L "${dest}" ]] || die "recovery destination already exists: ${dest}"
done
if [[ -e "${hs_data}" || -L "${hs_data}" ]]; then
  [[ -d "${hs_data}" && ! -L "${hs_data}" && -z "$(find "${hs_data}" -mindepth 1 -print -quit)" ]] ||
    die 'Headscale state already exists on this host'
fi

pg_shutdown_state() {
  LC_ALL=C runuser -u postgres -- "/usr/lib/postgresql/${pg_version}/bin/pg_controldata" "$1" |
    awk -F ': *' '$1 == "Database cluster state" {print $2}'
}

[[ "$(<"${stage}/postgres/PG_VERSION")" == "${pg_version}" &&
  ! -e "${stage}/postgres/postmaster.pid" && ! -e "${stage}/postgres/recovery.signal" &&
  ! -e "${stage}/postgres/standby.signal" && "$(pg_shutdown_state "${stage}/postgres")" == 'shut down' ]] ||
  die 'the PostgreSQL stage must be a stopped PostgreSQL 18 primary after WAL replay'
[[ "$(runuser -u postgres -- sqlite3 -readonly "${stage}/headscale.sqlite" 'PRAGMA integrity_check')" == ok ]] ||
  die 'the staged Headscale SQLite database failed integrity_check'

pg_work=""
hs_work=""
record_work=""
pg_setup=0
pg_data_installed=0
tablespaces_installed=0
hs_installed=0
record_installed=0
cleanup() {
  local status=$?
  if (( status != 0 )); then
    if (( record_installed )); then rm -rf -- "${recovery_record}"; fi
    if (( hs_installed )); then rm -rf -- "${hs_data}"; fi
    if (( pg_data_installed )); then rm -rf -- "${pg_data}"; fi
    if (( tablespaces_installed )); then rm -rf -- "${pg_tablespaces}"; fi
    if (( pg_setup )); then rm -rf -- "${pg_config}"; fi
  fi
  local path
  for path in "${pg_work}" "${hs_work}" "${record_work}"; do
    if [[ -n "${path}" ]]; then rm -rf -- "${path}"; fi
  done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for parent in "${pg_parent}" "$(dirname "${recovery_record}")"; do
  if [[ ! -d "${parent}" ]]; then mkdir -m 0755 -- "${parent}"; fi
  [[ ! -L "${parent}" ]] || die "recovery destination has a symlink parent: ${parent}"
done
pg_work="$(mktemp -d "${pg_parent}/.headmesh-import.XXXXXXXX")"
hs_work="$(mktemp -d /var/lib/.headscale-import.XXXXXXXX)"
record_work="$(mktemp -d "$(dirname "${recovery_record}")/.recovery.XXXXXXXX")"
chown postgres:postgres "${pg_work}"
runuser -u postgres -- cp -a --reflink=auto "${stage}/postgres" "${stage}/tablespaces" "${pg_work}/"

# The source's mapped tablespaces live under the private stage. Keep every
# restored tablespace attached when moving the database to its final paths.
while IFS= read -r -d '' link; do
  [[ "$(dirname -- "${link}")" == "${pg_work}/postgres/pg_tblspc" ]] ||
    die 'unexpected path symlink in the staged PostgreSQL data'
  target="$(realpath -e -- "${stage}/postgres/${link#"${pg_work}/postgres/"}")"
  suffix="${target#"${stage}/tablespaces/"}"
  [[ "${suffix}" != "${target}" && -d "${pg_work}/tablespaces/${suffix}" ]] ||
    die 'staged PostgreSQL tablespace points outside the staged tablespaces directory'
  runuser -u postgres -- ln -sfnT -- "${pg_tablespaces}/${suffix}" "${link}"
done < <(find "${pg_work}/postgres" -type l -print0)
[[ -z "$(find "${pg_work}/tablespaces" -type l -print -quit)" ]] || die 'unexpected symlink inside a staged tablespace'

# Start with the standby's own configuration. Old ALTER SYSTEM and recovery
# commands must not execute against the original backup repository.
rm -f -- "${pg_work}/postgres/"{postgresql.auto.conf,postgresql.conf,pg_hba.conf,pg_ident.conf}
install -m 0600 -o postgres -g postgres /dev/null "${pg_work}/postgres/postgresql.auto.conf"
[[ "$(pg_shutdown_state "${pg_work}/postgres")" == 'shut down' ]] || die 'the copied PostgreSQL cluster is invalid'

chown headscale:headscale "${hs_work}"
install -m 0600 -o headscale -g headscale "${stage}/headscale.sqlite" "${hs_work}/db.sqlite"
install -m 0600 -o headscale -g headscale -- "${stage}/keys/"{noise_private.key,derp_server_private.key} "${hs_work}/"
install -m 0600 -- "${stage}/manifest.json" "${stage}/keys/config.yaml" "${record_work}/"

# Let postgresql-common create its normal unit configuration in disabled
# mode. Its empty data directory is temporary and never becomes the cluster.
pg_setup=1
pg_createcluster --datadir="${pg_work}/empty" --port=5432 --start-conf=disabled "${pg_version}" main -- \
  --auth-local=peer --auth-host=scram-sha-256 >/dev/null
pg_conftool "${pg_version}" main set data_directory "${pg_data}"
pg_conftool "${pg_version}" main set listen_addresses localhost
pg_conftool "${pg_version}" main set archive_mode off

mv -T -- "${pg_work}/tablespaces" "${pg_tablespaces}"
tablespaces_installed=1
mv -T -- "${pg_work}/postgres" "${pg_data}"
pg_data_installed=1
if [[ -d "${hs_data}" ]]; then rmdir -- "${hs_data}"; fi
mv -T -- "${hs_work}" "${hs_data}"
hs_installed=1
mv -T -- "${record_work}" "${recovery_record}"
record_installed=1
printf 'Installed restored PostgreSQL and Headscale state. Services remain stopped.\n'
