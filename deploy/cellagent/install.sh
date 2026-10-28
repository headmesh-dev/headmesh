#!/usr/bin/env bash
# Install headmesh-cellagent beside a Headscale (plan §9.4). Run as root on the
# cell host. Idempotent. Installs only; starting it is a separate, explicit step.
#
#   install.sh <path-to-headmesh-cellagent-binary> [--enable]
#
# The agent talks to Headscale over its unix socket (headscale group) and only
# connects outbound to Headmesh, so no port is opened on the host.
set -Eeuo pipefail

die() { printf '[cellagent] ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '[cellagent] %s\n' "$*"; }

[[ "${EUID}" -eq 0 ]] || die "run as root"
bin="${1:?path to the headmesh-cellagent binary}"
enable="${2:-}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

getent group headscale >/dev/null || die "no headscale group: install Headscale first"
"${bin}" -h >/dev/null 2>&1 || "${bin}" --help >/dev/null 2>&1 || die "${bin} does not run on this host"

if ! id headmesh-cellagent &>/dev/null; then
  useradd --system --no-create-home --home-dir /var/lib/headmesh-cellagent \
    --shell /usr/sbin/nologin headmesh-cellagent
  log "created system user headmesh-cellagent"
fi

install -m 0755 -o root -g root "${bin}" /usr/local/bin/headmesh-cellagent
install -d -m 0750 -o root -g headmesh-cellagent /etc/headmesh
if [[ ! -f /etc/headmesh/cellagent.json ]]; then
  install -m 0640 -o root -g headmesh-cellagent "${here}/cellagent.example.json" /etc/headmesh/cellagent.json
  log "wrote /etc/headmesh/cellagent.json from the example: edit it, starting in observe mode"
fi
install -m 0644 -o root -g root "${here}/headmesh-cellagent.service" /etc/systemd/system/headmesh-cellagent.service
systemctl daemon-reload

if [[ "${enable}" == "--enable" ]]; then
  systemctl enable --now headmesh-cellagent.service
  log "started; follow with: journalctl -u headmesh-cellagent -f"
else
  log "installed, not started. Start with: systemctl enable --now headmesh-cellagent"
fi
