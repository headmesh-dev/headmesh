# A stand-in for hm-prod-1 for deploy/tests/e2e.sh: Ubuntu 26.04 (the same pinned image as the sandbox) with systemd
# as PID 1, so deploy.sh, bootstrap.sh and deployctl.sh run unchanged. Only what the base host bootstrap would
# already have installed is added here; the server bootstrap installs everything else itself.
FROM ubuntu:26.04@sha256:19fcd1bf268f02b60aca3bd30bd8a6d9a11759a430e32921833e65f16d87348a
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus ca-certificates curl gnupg jq sqlite3 \
      openssl xz-utils procps iproute2 \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-firstboot.service systemd-udevd.service getty.target console-getty.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
