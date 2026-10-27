#!/usr/bin/env bash
# Probe the installed sandbox firewall from the sandbox bridge (run on the host as
# a docker-group user; no root needed). Plan §6.5: "tested from inside the
# container with nc probes".
#
# By default a throwaway probe container joins the sandbox network at
# PROBE_IP (Regular policy) and probes the host, the LAN, CGNAT/tailnet,
# link-local and the internet. A silent drop (timeout) is what the firewall
# does; "connection refused" means the packet reached its target, so it counts
# as a failure for deny targets.
#
#   probe.sh                       probe as a Regular-policy address
#   probe.sh --yolo                probe as SBX_YOLO_IP (YOLO container must be down)
#   probe.sh --in-sandbox [NAME]   docker exec into the running sandbox instead.
#                                  Only meaningful while its tailscaled is logged
#                                  out: once enrolled, 100.64.0.0/10 is routed over
#                                  the container's own tun device, not the bridge.
set -euo pipefail

CONF=${HEADMESH_SANDBOX_FIREWALL_CONF:-/etc/headmesh/sandbox-firewall.conf}
NETWORK=${HM_SBX_NETWORK:-headmesh-sandbox_hm-sbx}
IMAGE=${HM_PROBE_IMAGE:-ghcr.io/headmesh-dev/sandbox:dev}
PROBE_IP=${HM_PROBE_IP:-172.31.250.250}
SBX_YOLO_IP=172.31.250.11
YOLO_ALLOW_LAN=0
# shellcheck source=/dev/null
[ -r "$CONF" ] && . "$CONF"

mode=throwaway
container=
case "${1:-}" in
    --yolo) PROBE_IP=$SBX_YOLO_IP ;;
    --in-sandbox) mode=exec; container=${2:-headmesh-sandbox} ;;
    "") ;;
    *) echo "usage: $0 [--yolo | --in-sandbox [NAME]]" >&2; exit 2 ;;
esac

docker network inspect "$NETWORK" >/dev/null 2>&1 || {
    echo "network $NETWORK not found; start the sandbox once (headmesh-sandbox up) so Compose creates it" >&2
    exit 2
}
gateway=$(docker network inspect "$NETWORK" -f '{{(index .IPAM.Config 0).Gateway}}')
router=$(ip -4 route show default | awk '{print $3; exit}')
host_ips=$(ip -4 -o addr show scope global | awk '{split($4, a, "/"); print a[1]}' | grep -v "^$gateway$" | sort -u)

name=hm-fwprobe-$$
if [ "$mode" = throwaway ]; then
    docker run -d --rm --entrypoint "" --name "$name" --network "$NETWORK" --ip "$PROBE_IP" \
        --cap-drop ALL --security-opt no-new-privileges:true "$IMAGE" sleep 300 >/dev/null
    trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
    target=$name
else
    target=$container
fi

fail=0
probe() { # want(allow|deny) host port label
    local want=$1 host=$2 port=$3 label=$4 out got
    out=$(docker exec "$target" sh -c "nc -v -z -w3 $host $port 2>&1" || true)
    case "$out" in
        *succeeded*|*open*) got=reached ;;
        *refused*) got=refused ;;
        *) got=filtered ;;
    esac
    if { [ "$want" = allow ] && [ "$got" = reached ]; } || { [ "$want" = deny ] && [ "$got" = filtered ]; }; then
        printf 'PASS  %-5s %-22s %s (%s)\n' "$want" "$host:$port" "$label" "$got"
    else
        printf 'FAIL  %-5s %-22s %s (%s)\n' "$want" "$host:$port" "$label" "$got"
        fail=1
    fi
}

echo "probing from $target ($([ "$mode" = throwaway ] && echo "$PROBE_IP" || echo running sandbox)) on $NETWORK"
probe allow 1.1.1.1 443 "internet"
probe allow github.com 443 "internet by name (Docker DNS)"
for p in ${HOST_PROBE_PORTS:-22 80 443}; do probe deny "$gateway" "$p" "host via bridge gateway"; done
for ip in $host_ips; do probe deny "$ip" 22 "host address"; done
if [ "$PROBE_IP" = "$SBX_YOLO_IP" ] && [ "$YOLO_ALLOW_LAN" = 1 ]; then
    probe allow "$router" 80 "LAN router (YOLO allow_lan=1)"
else
    probe deny "$router" 80 "LAN router"
fi
probe deny 100.64.0.1 443 "tailnet/CGNAT via host tailscale0"
probe deny 169.254.169.254 80 "link-local"
probe deny 172.17.0.1 22 "docker0 gateway"
echo "UDP $([ -n "${TS_UDP_PORTS:-}" ] && echo "$TS_UDP_PORTS" || echo 41641-41650) to the LAN is allowed by design; check its counter with: sudo headmesh-sandbox-firewall status"
[ $fail = 0 ] && echo "probe: firewall behaves as configured" || echo "probe: FAILURES (is headmesh-sandbox-firewall applied? sudo headmesh-sandbox-firewall status)"
exit $fail
