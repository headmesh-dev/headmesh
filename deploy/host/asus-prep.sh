#!/usr/bin/env bash
# ASUS host prep for the agent sandbox (plan §5.3). Idempotent; run with sudo.
#
#   sudo deploy/host/asus-prep.sh
#
# - never sleeps (blueprint pitfall 7): sleep targets masked, lid ignored
# - restic and age from Ubuntu (security updates via Pro/ESM)
# - gitleaks and sops pinned upstream releases, checked against the sha256 below
# - GPU smoke test through CDI
set -Eeuo pipefail

# Pinned upstream releases (plan §3.3). Bump version and checksum together.
readonly GITLEAKS_VERSION="8.30.1"
readonly GITLEAKS_SHA256="551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb"
readonly SOPS_VERSION="3.13.3"
readonly SOPS_SHA256="e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b"

export DEBIAN_FRONTEND=noninteractive

log() { printf '[asus-prep] %s\n' "$*"; }
die() { printf '[asus-prep] ERROR: %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

[[ "${EUID}" -eq 0 ]] || die "run with sudo"
# /tmp can be out of quota on this host; keep downloads on the big disk.
WORK="$(mktemp -d "${HM_WORK_DIR:-/mnt/storage/.claude-tmp}/asus-prep.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

never_sleep() {
  local targets=(sleep.target suspend.target hibernate.target hybrid-sleep.target)
  if [[ "$(systemctl is-enabled "${targets[0]}" 2>/dev/null || true)" != "masked" ]]; then
    systemctl mask "${targets[@]}" >/dev/null
    log "masked ${targets[*]}"
  fi
  install -d -m 0755 /etc/systemd/logind.conf.d
  local conf=/etc/systemd/logind.conf.d/headmesh.conf
  local want
  want=$'# Managed by headmesh deploy/host/asus-prep.sh\n[Login]\nHandleLidSwitch=ignore\nHandleLidSwitchExternalPower=ignore\nHandleLidSwitchDocked=ignore\n'
  if [[ ! -f "${conf}" ]] || [[ "$(cat "${conf}")"$'\n' != "${want}" ]]; then
    printf '%s' "${want}" >"${conf}"
    # Restarting logind can end the desktop session; it applies at next boot,
    # and the masked targets already stop suspend now.
    log "wrote ${conf} (applies at next boot)"
  fi
}

apt_tools() {
  local missing=()
  for p in restic age; do
    dpkg-query -W -f='${Status}' "${p}" 2>/dev/null | grep -q 'install ok installed' || missing+=("${p}")
  done
  if ((${#missing[@]})); then
    apt-get install -y --no-install-recommends "${missing[@]}" >/dev/null
    log "installed ${missing[*]}"
  fi
}

# $1 name, $2 version, $3 url, $4 sha256, $5 "tar" or "bin"
pinned_binary() {
  local name="$1" version="$2" url="$3" sha="$4" kind="$5" dest="/usr/local/bin/$1"
  if [[ -x "${dest}" ]] && "${dest}" --version 2>/dev/null | grep -q "${version}"; then
    return 0
  fi
  curl -fsSL "${url}" -o "${WORK}/${name}.download"
  echo "${sha}  ${WORK}/${name}.download" | sha256sum -c --quiet - || die "${name} checksum mismatch"
  if [[ "${kind}" == "tar" ]]; then
    tar -xzf "${WORK}/${name}.download" -C "${WORK}" "${name}"
    install -m 0755 -o root -g root "${WORK}/${name}" "${dest}"
  else
    install -m 0755 -o root -g root "${WORK}/${name}.download" "${dest}"
  fi
  log "installed ${name} ${version}"
}

gpu_smoke_test() {
  if ! command -v docker >/dev/null; then
    log "docker missing; GPU smoke test skipped"
    return 0
  fi
  if docker run --rm --device nvidia.com/gpu=all ubuntu:26.04 nvidia-smi -L; then
    log "GPU visible through CDI"
  else
    die "GPU smoke test failed (check /var/run/cdi/nvidia.yaml)"
  fi
}

never_sleep
apt_tools
pinned_binary gitleaks "${GITLEAKS_VERSION}" \
  "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz" \
  "${GITLEAKS_SHA256}" tar
pinned_binary sops "${SOPS_VERSION}" \
  "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64" \
  "${SOPS_SHA256}" bin
gpu_smoke_test
log "done: restic $(restic version | cut -d' ' -f2), age $(age --version), gitleaks $(gitleaks version), sops ${SOPS_VERSION}"
