#!/usr/bin/env bash
# Build the amd64 restore payload before provisioning a server.
set -euo pipefail
umask 077

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
destination="$(realpath -e -- "$1")"
# shellcheck source=deploy/backup/tool-images.sh
source "${repo}/deploy/backup/tool-images.sh"
mkdir "${destination}/bin" "${destination}/backup"

containers=()
cleanup() {
  if (( ${#containers[@]} )); then docker rm -v "${containers[@]}" >/dev/null; fi
}
trap cleanup EXIT

headscale="$(docker create --platform linux/amd64 --network none "${HEADSCALE}")"
containers+=("${headscale}")
docker cp "${headscale}:/ko-app/headscale" "${destination}/bin/headscale"
tailscale="$(docker create --platform linux/amd64 --network none "${TAILSCALE}" /bin/true)"
containers+=("${tailscale}")
docker cp "${tailscale}:/usr/local/bin/tailscale" "${destination}/bin/tailscale"
docker cp "${tailscale}:/usr/local/bin/tailscaled" "${destination}/bin/tailscaled"
litestream="$(docker create --platform linux/amd64 --network none "${LITESTREAM}" version)"
containers+=("${litestream}")
docker cp "${litestream}:/usr/local/bin/litestream" "${destination}/bin/litestream"

(cd "${repo}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "${destination}/bin/" \
  ./cmd/headmesh-server ./cmd/headmesh-cellagent)
chmod 0755 "${destination}/bin/"*
cp -- "${repo}/deploy/ha/remote-restore.sh" "${destination}/remote-restore.sh"
cp -- "${repo}/deploy/backup/"{restore-isolated.sh,check-restored-cell.sh,check-restored-control.sh,litestream-r2.yml} \
  "${destination}/backup/"
