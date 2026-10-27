#!/usr/bin/env bash
# Headmesh host bootstrap for Ubuntu 26.04 (plan §5.8, §13.2).
#
# Runs as root on the target. Every step is idempotent, so it is safe to re-run.
#
#   init       base host: admin user, sshd, unattended-upgrades, swap, sysctl,
#              journald, Docker (official repo), Tailscale, WireGuard tools,
#              Ubuntu Pro (when HM_PRO_TOKEN_FILE is set)
#   lock-root  PermitRootLogin no. Run only after the admin login works.
#   restore    recovery-VM restore (plan §13.4); not implemented yet.
#
# Usage from the ASUS (first run as root; after lock-root, as the admin with sudo):
#   ssh -i ~/.ssh/hm-prod-1 root@<hm-prod-1 IPv4> 'bash -s -- init' < deploy/compose/bootstrap.sh
#   ssh -i ~/.ssh/hm-prod-1 aaryn@<hm-prod-1 IPv4> 'sudo bash -s -- init' < deploy/compose/bootstrap.sh
#
# Environment:
#   HM_ADMIN_USER      admin login (default: aaryn)
#   HM_PRO_TOKEN_FILE  file holding an Ubuntu Pro token; shredded after attach
set -Eeuo pipefail

readonly ADMIN_USER="${HM_ADMIN_USER:-aaryn}"
readonly PRO_TOKEN_FILE="${HM_PRO_TOKEN_FILE:-}"

# Pinned package versions (plan §3.3 "Package discipline"). Bump deliberately.
readonly DOCKER_CE_VERSION="5:29.9.2-1~ubuntu.26.04~resolute"
readonly CONTAINERD_VERSION="2.4.2-3~ubuntu.26.04~resolute"
readonly BUILDX_VERSION="0.38.1-1~ubuntu.26.04~resolute"
readonly COMPOSE_VERSION="5.6.1-1~ubuntu.26.04~resolute"
readonly TAILSCALE_VERSION="1.104.2"

export DEBIAN_FRONTEND=noninteractive

PRO_ATTACH_CFG=""

log() { printf '[bootstrap] %s\n' "$*"; }
die() { printf '[bootstrap] ERROR: %s\n' "$*" >&2; exit 1; }

# The Pro token never outlives the run, whether it succeeds or fails.
cleanup() {
  [[ -n "${PRO_ATTACH_CFG}" && -f "${PRO_ATTACH_CFG}" ]] && shred -u "${PRO_ATTACH_CFG}"
  [[ -n "${PRO_TOKEN_FILE}" && -f "${PRO_TOKEN_FILE}" ]] && shred -u "${PRO_TOKEN_FILE}"
  return 0
}
trap cleanup EXIT
trap 'die "failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

# Writes stdin to $1 with mode $2 only when the content changed.
# Returns 0 when the file changed, 1 when it was already up to date.
write_file() {
  local path="$1" mode="$2" tmp
  tmp="$(mktemp)"
  cat >"${tmp}"
  if [[ -f "${path}" ]] && cmp -s "${tmp}" "${path}"; then
    rm -f "${tmp}"
    return 1
  fi
  install -D -m "${mode}" "${tmp}" "${path}"
  rm -f "${tmp}"
  log "wrote ${path}"
}

preflight() {
  [[ "${EUID}" -eq 0 ]] || die "run as root"
  # shellcheck source=/dev/null
  . /etc/os-release
  [[ "${ID}" == "ubuntu" && "${VERSION_ID}" == "26.04" ]] \
    || die "expected Ubuntu 26.04, found ${PRETTY_NAME}"
}

admin_user() {
  if ! id "${ADMIN_USER}" &>/dev/null; then
    useradd --create-home --shell /bin/bash --groups sudo "${ADMIN_USER}"
    log "created user ${ADMIN_USER}"
  fi
  # Key-only account: no password, so sudo must not ask for one.
  local sudoers="/etc/sudoers.d/90-headmesh-${ADMIN_USER}"
  if printf '%s ALL=(ALL) NOPASSWD:ALL\n' "${ADMIN_USER}" | write_file "${sudoers}" 0440; then
    if command -v visudo &>/dev/null && ! visudo -c >/dev/null; then
      rm -f "${sudoers}"
      die "invalid sudoers drop-in"
    fi
  fi
  # The provider injects the operator keys into root; the admin gets the same set.
  local home ssh_dir
  home="$(getent passwd "${ADMIN_USER}" | cut -d: -f6)"
  ssh_dir="${home}/.ssh"
  [[ -s /root/.ssh/authorized_keys ]] || die "/root/.ssh/authorized_keys is empty"
  install -d -m 0700 -o "${ADMIN_USER}" -g "${ADMIN_USER}" "${ssh_dir}"
  if write_file "${ssh_dir}/authorized_keys" 0600 </root/.ssh/authorized_keys; then
    chown "${ADMIN_USER}:${ADMIN_USER}" "${ssh_dir}/authorized_keys"
  fi
}

sshd_hardening() {
  local root_login="$1"
  # Ubuntu reads sshd_config.d in order and the first value wins, so 10-
  # overrides cloud-init's 50-cloud-init.conf.
  if write_file /etc/ssh/sshd_config.d/10-headmesh.conf 0644 <<EOF
# Managed by headmesh deploy/compose/bootstrap.sh
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin ${root_login}
EOF
  then
    sshd -t || die "sshd config test failed"
    # ssh is socket-activated on 26.04; existing sessions survive either way.
    systemctl try-reload-or-restart ssh
    log "sshd reloaded (PermitRootLogin ${root_login})"
  fi
}

unattended_upgrades() {
  apt-get install -y --no-install-recommends unattended-upgrades >/dev/null
  write_file /etc/apt/apt.conf.d/20auto-upgrades 0644 <<'EOF' || true
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
  write_file /etc/apt/apt.conf.d/52headmesh-unattended 0644 <<'EOF' || true
// Security updates only (Ubuntu's default origins). Never reboot on our own:
// kernel fixes come from Livepatch, reboots are planned.
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
EOF
}

swap_file() {
  if ! swapon --show=NAME --noheadings | grep -qx /swapfile; then
    [[ -f /swapfile ]] || fallocate -l 2G /swapfile
    chmod 0600 /swapfile
    mkswap /swapfile >/dev/null
    swapon /swapfile
    log "enabled 2 GB swap"
  fi
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >>/etc/fstab
}

kernel_and_journal() {
  if write_file /etc/sysctl.d/90-headmesh.conf 0644 <<'EOF'
# Managed by headmesh deploy/compose/bootstrap.sh
vm.swappiness = 10
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
net.ipv4.tcp_syncookies = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv6.conf.all.accept_source_route = 0
# Loose reverse-path filtering, as Tailscale requires.
net.ipv4.conf.all.rp_filter = 2
net.ipv4.conf.default.rp_filter = 2
EOF
  then
    sysctl --system >/dev/null
  fi
  if write_file /etc/systemd/journald.conf.d/90-headmesh.conf 0644 <<'EOF'
[Journal]
SystemMaxUse=500M
EOF
  then
    systemctl restart systemd-journald
  fi
}

apt_repo() {
  local name="$1" key_url="$2" key_path="$3" uri="$4" suite="$5" component="$6"
  if [[ ! -s "${key_path}" ]]; then
    install -d -m 0755 "$(dirname "${key_path}")"
    curl -fsSL "${key_url}" -o "${key_path}"
    chmod 0644 "${key_path}"
  fi
  write_file "/etc/apt/sources.list.d/${name}.sources" 0644 <<EOF || true
Types: deb
URIs: ${uri}
Suites: ${suite}
Components: ${component}
Signed-By: ${key_path}
EOF
}

docker_engine() {
  apt_repo docker https://download.docker.com/linux/ubuntu/gpg /etc/apt/keyrings/docker.asc \
    https://download.docker.com/linux/ubuntu resolute stable
  # Stay on the pinned minor series; patch releases only by a deliberate upgrade.
  write_file /etc/apt/preferences.d/90-headmesh-docker 0644 <<'EOF' || true
Package: docker-ce docker-ce-cli docker-ce-rootless-extras
Pin: version 5:29.9.*
Pin-Priority: 1001

Package: containerd.io
Pin: version 2.4.*
Pin-Priority: 1001
EOF
  apt-get update -qq
  if ! dpkg-query -W -f='${Version}' docker-ce 2>/dev/null | grep -q '^5:29\.9\.'; then
    apt-get install -y --no-install-recommends \
      "docker-ce=${DOCKER_CE_VERSION}" "docker-ce-cli=${DOCKER_CE_VERSION}" \
      "containerd.io=${CONTAINERD_VERSION}" \
      "docker-buildx-plugin=${BUILDX_VERSION}" "docker-compose-plugin=${COMPOSE_VERSION}" >/dev/null
    log "installed docker-ce ${DOCKER_CE_VERSION}"
  fi
  # Bounded container logs and BuildKit GC (plan §5.4).
  if write_file /etc/docker/daemon.json 0644 <<'EOF'
{
  "log-driver": "local",
  "log-opts": { "max-size": "20m", "max-file": "5" },
  "builder": { "gc": { "enabled": true, "defaultKeepStorage": "5GB" } }
}
EOF
  then
    systemctl restart docker
  fi
  systemctl enable --now docker >/dev/null
}

tailscale_client() {
  apt_repo tailscale https://pkgs.tailscale.com/stable/ubuntu/resolute.noarmor.gpg \
    /usr/share/keyrings/tailscale-archive-keyring.gpg \
    https://pkgs.tailscale.com/stable/ubuntu resolute main
  write_file /etc/apt/preferences.d/90-headmesh-tailscale 0644 <<'EOF' || true
Package: tailscale
Pin: version 1.104.*
Pin-Priority: 1001
EOF
  apt-get update -qq
  if [[ "$(dpkg-query -W -f='${Version}' tailscale 2>/dev/null || true)" != "${TAILSCALE_VERSION}" ]]; then
    apt-get install -y --no-install-recommends "tailscale=${TAILSCALE_VERSION}" >/dev/null
    log "installed tailscale ${TAILSCALE_VERSION} (not logged in; enrollment is a later step)"
  fi
}

wireguard_tools() {
  # Break-glass interface config comes from sops later (plan §13.7).
  dpkg-query -W -f='${Status}' wireguard-tools 2>/dev/null | grep -q 'install ok installed' \
    || apt-get install -y --no-install-recommends wireguard-tools >/dev/null
}

ubuntu_pro() {
  if pro status --format json 2>/dev/null | grep -q '"attached": *true'; then
    log "Ubuntu Pro already attached"
  elif [[ -n "${PRO_TOKEN_FILE}" && -s "${PRO_TOKEN_FILE}" ]]; then
    # --attach-config keeps the token out of the process list.
    PRO_ATTACH_CFG="$(mktemp)"
    chmod 0600 "${PRO_ATTACH_CFG}"
    printf 'token: %s\n' "$(tr -d '[:space:]' <"${PRO_TOKEN_FILE}")" >"${PRO_ATTACH_CFG}"
    pro attach --attach-config "${PRO_ATTACH_CFG}" >/dev/null
    log "Ubuntu Pro attached"
  else
    log "Ubuntu Pro skipped (set HM_PRO_TOKEN_FILE to attach)"
  fi
}

cmd_init() {
  preflight
  apt-get update -qq
  apt-get install -y --no-install-recommends ca-certificates curl gnupg jq sqlite3 >/dev/null
  admin_user
  sshd_hardening prohibit-password
  unattended_upgrades
  swap_file
  kernel_and_journal
  docker_engine
  tailscale_client
  wireguard_tools
  ubuntu_pro
  log "init complete. Verify '${ADMIN_USER}' can log in and sudo, then run lock-root."
}

cmd_lock_root() {
  preflight
  [[ -s "$(getent passwd "${ADMIN_USER}" | cut -d: -f6)/.ssh/authorized_keys" ]] \
    || die "${ADMIN_USER} has no authorized_keys; refusing to lock root out"
  sshd_hardening no
}

case "${1:-}" in
  init) cmd_init ;;
  lock-root) cmd_lock_root ;;
  restore) die "restore is not implemented yet (plan §13.4)" ;;
  *) die "usage: bootstrap.sh init|lock-root|restore" ;;
esac
