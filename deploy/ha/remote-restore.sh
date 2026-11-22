#!/usr/bin/env bash
# Invoked inside the temporary Hetzner VM after cloud-init and verified SSH.
set -euo pipefail
umask 077

payload="$1"
private="${payload}/private"
export PATH="${payload}/bin:/usr/lib/postgresql/18/bin:/usr/bin:/bin"
export AWS_SHARED_CREDENTIALS_FILE="${private}/litestream.aws"
export AWS_EC2_METADATA_DISABLED=true
export PGBACKREST_CONFIG="${private}/pgbackrest.conf"
HEADMESH_R2_ENDPOINT="$(<"${private}/endpoint")"
export HEADMESH_R2_ENDPOINT
mkdir "${payload}/work"
bash "${payload}/backup/restore-isolated.sh" --check-control-plane "${payload}/work" \
  "${payload}/backup/litestream-r2.yml" "${private}/manifest.netrc" "${private}/keys.netrc" "${private}/recovery.agekey"
