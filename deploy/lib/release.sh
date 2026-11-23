#!/usr/bin/env bash
# Shared local builds for regular deployment and recovery packaging.

release_id() {
  local sha dirty=""
  sha="$(git -C "${ROOT}" rev-parse --short=12 HEAD)"
  [[ -n "$(git -C "${ROOT}" status --porcelain)" ]] && dirty="-dirty"
  echo "$(date -u +%Y%m%dT%H%M%SZ)-${sha}${dirty}"
}

build_backend() {
  local out="$1" release="$2" arch="$3"
  install -d "${out}"
  log "build: headmesh-server ${release} (linux/${arch}, static)"
  (cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.version=${release}" -o "${out}/" ./cmd/headmesh-server ./cmd/headmesh-cellagent)
}

build_waitlist() {
  local out="$1" release="$2"
  log "build: waitlist service ${release}"
  # package.json "files" limits the copy to server/, shared/ and migrations/, plus production dependencies.
  pnpm --dir "${ROOT}" --filter @headmesh/marketing deploy --prod "${out}" >/dev/null
  [[ -f "${out}/server/main.ts" && -d "${out}/node_modules/zod" && -d "${out}/node_modules/resend" && ! -d "${out}/src" ]] || die 'waitlist bundle is incomplete'
}

build_site() {
  local env="$1" out="$2" build_env="$3" pkg="${ROOT}/web/marketing"
  log "build: headmesh.dev site for ${env}"
  (
    set -a
    # shellcheck source=/dev/null
    source "${build_env}"
    set +a
    [[ "$(hm_env "${env}" PRODUCTION)" == 1 ]] && export HEADMESH_PRODUCTION=1
    cd "${pkg}" && pnpm run build >/dev/null
  )
  install -d "${out}/site" "${out}/nginx"
  cp -a "${pkg}/dist/." "${out}/site/"
  install -m 0644 "${pkg}/build/nginx/security-headers.conf" "${out}/nginx/"
  [[ -f "${out}/site/index.html" ]] || die "site build for ${env} has no index.html"
}

# Publish one release directory after all the component builds succeed.
package_recovery_release() (
  umask 077
  local out="$1" build_env="$2" release commit parent scratch arch
  [[ ! -e "${out}" && ! -L "${out}" ]] || die 'use a new recovery release output directory'
  release="$(release_id)"
  [[ "${release}" != *-dirty ]] || die 'use a clean release checkout for recovery packaging'
  commit="$(git -C "${ROOT}" rev-parse HEAD)"
  parent="$(realpath -e -- "$(dirname -- "${out}")")"
  scratch="$(mktemp -d "${parent}/.headmesh-release.XXXXXX")"
  trap 'rm -rf -- "${scratch}"' EXIT
  umask 022
  install -d "${scratch}/release/backend"
  for arch in amd64 arm64; do
    build_backend "${scratch}/release/backend/${arch}" "${release}" "${arch}"
  done
  build_waitlist "${scratch}/release/waitlist" "${release}"
  build_site prod "${scratch}/release/marketing" "${build_env}"
  render_nginx prod "${scratch}/release/nginx"
  stage_toolkit "${scratch}/release/toolkit"
  jq -cn --arg release "${release}" --arg commit "${commit}" \
    '{release: $release, commit: $commit, architectures: ["amd64", "arm64"]}' >"${scratch}/release/release.json"
  mv -nT -- "${scratch}/release" "${out}"
  [[ ! -e "${scratch}/release" ]] || die 'the package destination appeared during the build'
  log "prepared standby release ${release}"
)
