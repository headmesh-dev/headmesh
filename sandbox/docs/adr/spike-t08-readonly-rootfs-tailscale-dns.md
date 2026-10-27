# Spike T-8: read-only rootfs with tailscaled managing DNS

- Status: done (2026-10-27). Decision: **writable rootfs** (the plan's fallback)
- Plan: §6.6 (DNS, pitfall 3), §17 T-8; contract: environment-sourced Compose secrets
- Environment: ASUS, kernel 7.0, Docker 29.1.3, Compose 2.40.3, Tailscale 1.104.2, Headscale v0.29.6

## Question

Can the sandbox run with `read_only: true` while tailscaled (`--accept-dns=true`)
rewrites `/etc/resolv.conf` for MagicDNS? And do the environment-sourced
Compose secrets the contract relies on (spike T-1) still work on a read-only
rootfs?

## Test

A throwaway tailnet, isolated from production: `headscale/headscale:v0.29.6@sha256:cabe84f0…`
on a private Docker network (`server_url: http://hs.spike.test:8080`, MagicDNS
`base_domain: mesh.test`, embedded DERP region 999, no external DERP map,
`disable_check_updates`). Nodes run `tailscale/tailscale:v1.104.2` binaries on
`ubuntu:26.04@sha256:19fcd1bf…` with the §6.3 security options (`cap_drop: ALL`,
`cap_add` NET_ADMIN NET_RAW SETUID SETGID CHOWN DAC_OVERRIDE KILL,
`no-new-privileges`, `/dev/net/tun`) and the §6.7 tailscaled flags
(`--tun=hm0 --port=41641`). Variants:

1. writable rootfs (baseline);
2. `read_only: true` with tmpfs on `/run`, `/tmp`, `/var/lib/tailscale`;
3. `read_only: true` plus an environment-sourced Compose secret.

Each node ran `tailscale up --login-server=… --auth-key=file:… --accept-dns=true
--accept-routes=false --ssh --netfilter-mode=on` with a tagged single-use key,
then `getent hosts laptop.mesh.test`, `getent hosts github.com`, `tailscale ping`.

## Evidence

| Variant | Result |
|---|---|
| 1. writable | `/etc/resolv.conf` → `nameserver 100.100.100.100`, `search mesh.test`. tailscaled log: `rename of "/etc/resolv.conf" … failed (… device or resource busy)` (Docker bind-mounts the file), then it falls back to copy: writes `/etc/resolv.pre-tailscale-backup.conf` and rewrites `/etc/resolv.conf` in place. `laptop.mesh.test` and short `laptop` resolve; `github.com` resolves (forwarded to Docker's `127.0.0.11`). |
| 2. read-only | Docker mounts `/etc/resolv.conf` **`ro`** too. tailscaled: `health(warnable=dns-set-os-config-failed): … writing to "/etc/resolv.pre-tailscale-backup.conf" … read-only file system`. `resolv.conf` stays `127.0.0.11`; `getent hosts laptop.mesh.test` **fails**. Only an explicit `dig @100.100.100.100` answers. Tailnet traffic itself works (`tailscale ping` pong, direct path). |
| 3. read-only + env secret | **Compose refuses to start:** `cannot create secret "…ts_authkey_ro" in read-only service sbx-ro-secret: 'file' is the sole supported option`. File-sourced secrets are host-owned bind mounts that `agent` (UID 1000) can read, so they defeat T-1. |

The image built by Fork A (`headmesh-sandbox:dev-stub`, writable rootfs)
behaved like variant 1: MagicDNS active, `laptop.mesh.test` resolves, container
`healthy`.

Rejected workarounds:
- tmpfs on `/etc`: hides `passwd`, `group`, PAM and everything else in `/etc`.
- Bind a host-writable `resolv.conf`: tailscaled still needs to create the
  backup file in `/etc`, which stays read-only (the failure in variant 2 is on
  the backup write).
- `--accept-dns=false` plus Compose `dns: [100.100.100.100]`: on a user-defined
  network the container still talks to Docker's `127.0.0.11`, which forwards
  upstream. For the default upstream (systemd-resolved on loopback) we saw it
  forward from the **host** namespace (conntrack check, `sandbox/host/README.md`).
  Not tested for a non-loopback upstream; if Docker also forwards that from the
  host, `100.100.100.100` is the host's `tailscaled-xcelsior` (the host node's view of
  the tailnet, wrong identity). Too fragile to rely on, and it gives up the
  search domain.

## Decision

Keep the rootfs **writable** (plan fallback). Every other hardening item stays:
`cap_drop: ALL` plus the listed capabilities, `no-new-privileges`, no published
ports, agents as UID 1000 without sudo. The env-sourced secrets (T-1) need a
writable rootfs anyway. Recreating the container (`headmesh-sandbox recreate`)
discards rootfs drift; all state that matters is on bind mounts.

## Other findings from this tailnet (relevant to §6.6 and the image)

1. **Tagged pre-auth key + `--advertise-tags` is rejected by Headscale v0.29.6.**
   With `tagOwners: {"tag:sandbox": ["spike@"]}` and a key created with
   `--tags tag:sandbox`, `tailscale up … --advertise-tags=tag:sandbox` failed:
   `backend error: handling register with auth key: creating new node: requested
   tags [tag:sandbox] are invalid or not permitted`. The same key **without**
   `--advertise-tags` enrolled the node as `tag:sandbox` (owner
   `tagged-devices`). Reproduced with Fork A's `tailscale-up` script when
   `TS_TAGS` is set. Recommendation: the tagged key carries the tags; drop
   `--advertise-tags` from `tailscale up` (plan §6.6 flags) and use `TS_TAGS`
   only to verify the tags after enrollment. Recheck against the production
   cell's policy before the M1 enrollment.
2. **tailscaled uploads logs to Tailscale Inc. by default.** In Fork A's image
   tailscaled held a TCP connection to `199.165.136.100:443` =
   `log.tailscale.com` (its log lines show `logpolicy`), although the control
   server is our Headscale. The logs carry node names, tailnet IPs and peers.
   Fix: run tailscaled with `--no-logs-no-support` (coordinator has asked
   Fork A to add it). The host's `tailscaled-xcelsior` should be checked too.
3. After a failed registration tailscaled forces control dials to port 443 for
   a while (`HealthTracker.LastNoiseDialWasRecent`), so an immediate retry
   against an `http://…:8080` login server fails with `TLS forced: no port 80
   dialed`. Restart tailscaled before retrying.
4. When the control host name doesn't resolve, tailscaled asks the built-in
   Tailscale DERP servers (`derp*.tailscale.com/bootstrap-dns`) to resolve it.
   Harmless for `hs.xcelsior.ca`, but it does leak the control-server name to
   Tailscale; Docker DNS working (the firewall leaves it intact) avoids it.

## Cleanup

All spike containers, networks, volumes, keys and the two spike images were
removed. The pulled `headscale/headscale:v0.29.6` image is kept for later tests.
