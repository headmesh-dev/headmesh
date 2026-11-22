#!/usr/bin/env bash
# Create empty, private Headmesh credential files outside the checkout.
set -euo pipefail
umask 077

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(realpath -- "${here}/../..")"
directory="$(realpath -m -- "${1:-${XDG_CONFIG_HOME:-${HOME}/.config}/headmesh/recovery}")"
case "${directory}/" in
  "${repo}/"*) echo 'Choose a credential directory outside the Headmesh checkout.' >&2; exit 1 ;;
esac

for path in "${directory}" "${directory}/publish" "${directory}/drill"; do
  mkdir -p -- "${path}"
  if [[ -L "${path}" || ! -O "${path}" || "$(stat -c '%a' -- "${path}")" != 700 ]]; then
    printf 'Use a directory you own with mode 0700 at %s\n' "${path}" >&2
    exit 1
  fi
done

write_blank() {
  local path="$1"
  if [[ -e "${path}" || -L "${path}" ]]; then
    if [[ -L "${path}" || ! -f "${path}" || ! -O "${path}" || "$(stat -c '%a' -- "${path}")" != 600 ]]; then
      printf 'Expected a private regular file you own with mode 0600 at %s\n' "${path}" >&2
      return 1
    fi
    cat >/dev/null
  else
    (set -o noclobber; cat >"${path}")
  fi
}

write_blank "${directory}/recovery.env" <<'EOF'
HEADMESH_R2_ENDPOINT=''
HCLOUD_TOKEN=''
OWNER_RECIPIENT=''
RECOVERY_RECIPIENT=''
HEADMESH_DRILL_FIREWALL_ID=''
EOF

for role in publish drill; do
  write_blank "${directory}/${role}/litestream.aws" <<'EOF'
[default]
aws_access_key_id =
aws_secret_access_key =
EOF
  for name in manifest keys; do
    write_blank "${directory}/${role}/${name}.netrc" <<'EOF'
machine ""
  login ""
  password ""
EOF
  done
done

write_blank "${directory}/publish/manifest.env" <<'EOF'
HEADMESH_DATABASE_URL=''
EOF

write_blank "${directory}/publish/headmesh-r2.conf" <<'EOF'
[global]
repo1-s3-endpoint=
repo1-s3-key=
repo1-s3-key-secret=
repo1-cipher-pass=
EOF

awk '/^\[global\]$/ { print; print "repo1-s3-endpoint=\nrepo1-s3-key=\nrepo1-s3-key-secret=\nrepo1-cipher-pass="; next } { print }' \
  "${here}/pgbackrest-r2.conf" | write_blank "${directory}/drill/pgbackrest.conf"

printf 'Private credential templates are ready in %s\n' "${directory}"
