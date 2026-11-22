#!/usr/bin/env bash
# Build the restore payload for the standby's architecture before uploading it.
set -euo pipefail
umask 077

[[ $# -ge 1 && $# -le 2 ]] || { echo 'Usage: package-restore.sh DIRECTORY [amd64|arm64]' >&2; exit 2; }
arch="${2:-amd64}"
[[ "${arch}" == amd64 || "${arch}" == arm64 ]] || { echo 'Restore payload architecture must be amd64 or arm64.' >&2; exit 2; }
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

# Use each platform's child digest so Docker can cache both architectures of
# the pinned image index on the same runner.
create_image_container() {
  local image="$1" digest
  shift
  digest="$(docker manifest inspect "${image}" | jq -er --arg arch "${arch}" '
    [.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest]
    | if length == 1 then .[0] else error("expected one image for linux/" + $arch) end
  ')"
  [[ "${digest}" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo "Invalid platform digest for ${image}." >&2; return 1; }
  docker create --platform "linux/${arch}" --network none "${image%@*}@${digest}" "$@"
}

headscale="$(create_image_container "${HEADSCALE}")"
containers+=("${headscale}")
docker cp "${headscale}:/ko-app/headscale" "${destination}/bin/headscale"
tailscale="$(create_image_container "${TAILSCALE}" /bin/true)"
containers+=("${tailscale}")
docker cp "${tailscale}:/usr/local/bin/tailscale" "${destination}/bin/tailscale"
docker cp "${tailscale}:/usr/local/bin/tailscaled" "${destination}/bin/tailscaled"
litestream="$(create_image_container "${LITESTREAM}" version)"
containers+=("${litestream}")
docker cp "${litestream}:/usr/local/bin/litestream" "${destination}/bin/litestream"

(cd "${repo}" && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -trimpath -o "${destination}/bin/" \
  ./cmd/headmesh-server ./cmd/headmesh-cellagent)
chmod 0755 "${destination}/bin/"*
cp -- "${repo}/deploy/ha/remote-restore.sh" "${destination}/remote-restore.sh"
cp -- "${repo}/deploy/backup/"{restore-isolated.sh,check-restored-cell.sh,check-restored-control.sh,litestream-r2.yml} \
  "${destination}/backup/"
