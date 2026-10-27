# Host-side files for the agent sandbox (ASUS)

Everything here runs as root on the host, so Aaryn installs it with `sudo`.
Nothing in this folder is installed automatically.

| Folder | What | Plan |
|---|---|---|
| `firewall/` | Egress firewall for the sandbox bridge: nftables table `inet headmesh_sbx` | §6.5 |
| `backup/` | Nightly restic snapshot of `/home/aaryn`, monthly `restic check` | §6.12 |

Both write a small world-readable status file so `headmesh-sandbox status` and
`doctor` can report them without root:

- `/run/headmesh-sandbox-firewall.json` (`applied`, `hash`, `bridge`, `updated_at`)
- `/mnt/storage/headmesh/sandbox/state/backup-status.json` (`last_success_at`,
  `last_snapshot_id`, `last_check_ok_at`, …)

---

## 1. Egress firewall (`firewall/`)

### What a typical host already does

A host that runs Docker usually has several layers in `FORWARD` before any rule
of ours: its own forwarding rules (a tailnet client may accept traffic to its
interface first), Docker's `DOCKER-USER` and `DOCKER-FORWARD` chains, and a host
firewall such as ufw. A plain bridge container can therefore reach:

- the host's own tailnet, masqueraded as the host's node, when the host accepts
  forwarded traffic to its tailnet interface ahead of `DOCKER-USER`;
- host services that the host firewall opens to Docker bridges;
- the LAN behind the host.

A `DOCKER-USER` rule can't stop the first case when an earlier rule accepts it,
which is why this table hooks in ahead of all of them. Name the bridge so that
the host's own Docker rules give it egress (the contract uses `br-hm-sbx0`).
Docker's embedded DNS (`127.0.0.11`) normally forwards from the host namespace,
so the firewall needs no DNS hole; `ALLOW_DNS_TO` exists for hosts where it
forwards from the container namespace. The host-specific survey behind this
design is in the internal ops notes.

### Design

`headmesh-sandbox-firewall` loads one native nftables table, `inet
headmesh_sbx`, whose `input` and `forward` base chains hook at priority
`filter - 10`. They run **before** every chain above. In nftables a `drop` in
any base chain is final, and an `accept` only ends that chain, so the table can
only take traffic away: it never opens anything the existing rules block, and
it never edits a chain owned by Docker, ufw, tailscaled or Xcelsior.

For packets arriving from `br-hm-sbx0`:

| Destination | Verdict |
|---|---|
| The host itself (any local address: bridge gateway, LAN IPs, `tailscale0`, `docker0`, …) | drop, except replies to connections the host opened |
| Another container on the bridge | drop (the network also sets `enable_icc=false`) |
| UDP `41641–41650` to `10/8`, `192.168/16`, the host's LAN subnets | allow (Tailscale direct paths) |
| `ALLOW_DNS_TO` port 53 | allow (empty by default) |
| YOLO address `172.31.250.11` to the host's LAN subnets, if `YOLO_ALLOW_LAN=1` | allow |
| `0/8 10/8 100.64/10 127/8 169.254/16 172.16/12 192.0.0/24 192.0.2/24 192.168/16 198.18/15 198.51.100/24 203.0.113/24 224/4 240/4`, IPv6 ULA (incl. Tailscale `fd7a:115c:a1e0::/48`), link-local, multicast | drop, logged as `HM-SBX-DROP ` (10/min) |
| Everything else (the internet) | allow; the iptables rules still apply afterwards |

The sandbox's own tailnet traffic is unaffected: tailscaled in the container
sends WireGuard/DERP/STUN to the internet (or UDP 41641 to LAN peers), and
`100.64.0.0/10` inside the container is routed over its own `hm0`, never the bridge.

Regular and YOLO run one at a time on the same bridge with fixed addresses
(`.10` and `.11`), so `allow_lan` applies to the YOLO address only and a mode
switch needs no reload. The table stays loaded while Docker is stopped, and the
unit orders itself `Before=docker.service`, so a container Docker restarts on
its own is never briefly unfiltered. A timer re-applies the table every 5
minutes if it went missing or drifted from the config (for example after `nft
flush ruleset`).

### Files

| File | Installed as |
|---|---|
| `headmesh-sandbox-firewall` | `/usr/local/sbin/headmesh-sandbox-firewall` (`apply`, `ensure`, `remove`, `status`, `render`, `check [--docker IMAGE]`) |
| `sandbox-firewall.conf.example` | `/etc/headmesh/sandbox-firewall.conf` (root-owned, not group/world-writable; the script refuses otherwise) |
| `headmesh-sandbox-firewall.service` | `/etc/systemd/system/` |
| `headmesh-sandbox-firewall-ensure.service`, `.timer` | `/etc/systemd/system/` |
| `probe.sh` | not installed; run from the repo after install (no root) |
| `selftest.sh` | not installed; developer test in throwaway network namespaces (no root, no host changes) |

### Install (Aaryn)

```bash
cd /mnt/storage/projects/headmesh/sandbox/host
sudo install -d -m 0755 /etc/headmesh
sudo install -m 0644 -o root -g root firewall/sandbox-firewall.conf.example /etc/headmesh/sandbox-firewall.conf
sudo install -m 0755 -o root -g root firewall/headmesh-sandbox-firewall /usr/local/sbin/headmesh-sandbox-firewall
sudo install -m 0644 -o root -g root firewall/headmesh-sandbox-firewall.service firewall/headmesh-sandbox-firewall-ensure.service firewall/headmesh-sandbox-firewall-ensure.timer /etc/systemd/system/
sudo headmesh-sandbox-firewall render
sudo headmesh-sandbox-firewall check
sudo systemctl daemon-reload
sudo systemctl enable --now headmesh-sandbox-firewall.service headmesh-sandbox-firewall-ensure.timer
sudo headmesh-sandbox-firewall status
```

`render` prints the exact ruleset (check the `lan=[…]` line: it lists the LAN
subnets found on the physical interfaces). `check` validates it with `nft -c`
without loading it.

### Verify

```bash
sudo headmesh-sandbox-firewall status
cat /run/headmesh-sandbox-firewall.json
```

`status` exits non-zero and explains when the table is missing, when it differs
from the config, or when `DOCKER-USER` would drop the bridge.

Once the sandbox network exists (after the first `headmesh-sandbox up`), probe
it from the bridge without root:

```bash
/mnt/storage/projects/headmesh/sandbox/host/firewall/probe.sh
```

It starts a throwaway container on `headmesh-sandbox_hm-sbx` at
`172.31.250.250` and expects: internet allowed; the host (bridge gateway, every
host address, the ports in `HOST_PROBE_PORTS`), the LAN router, `100.64.0.1`,
`169.254.169.254` and `docker0` **filtered** (a timeout; "connection refused"
counts as a failure because the packet got through). `--yolo` probes as the
YOLO address (with the YOLO container down). `--in-sandbox` uses the running
sandbox, which is only meaningful before it joins the tailnet. Override the
image with `HM_PROBE_IMAGE=…`. Run it **after** installing the firewall: before
that, on a host that forwards to its own tailnet, the `100.64.0.1` probe gets through.

Watch drops live: `sudo journalctl -kf | grep HM-SBX-DROP`.

### Change settings

Edit `/etc/headmesh/sandbox-firewall.conf` (keep it in step with
`sandbox/compose/compose.yaml` and `sandbox.toml`), then
`sudo systemctl reload headmesh-sandbox-firewall`.
For `[mode.yolo] allow_lan = true` set `YOLO_ALLOW_LAN=1`.

### Uninstall

```bash
sudo systemctl disable --now headmesh-sandbox-firewall-ensure.timer headmesh-sandbox-firewall.service
sudo headmesh-sandbox-firewall remove
sudo rm -f /etc/systemd/system/headmesh-sandbox-firewall.service /etc/systemd/system/headmesh-sandbox-firewall-ensure.service /etc/systemd/system/headmesh-sandbox-firewall-ensure.timer
sudo rm -f /usr/local/sbin/headmesh-sandbox-firewall /etc/headmesh/sandbox-firewall.conf /run/headmesh-sandbox-firewall.json
sudo systemctl daemon-reload
```

(`disable --now` doesn't unload the table on purpose; `remove` does.)

### Tests run without touching the host (2026-10-27)

- `headmesh-sandbox-firewall check --docker <image>`: `nft -c` inside a
  throwaway container with `--network none` (its own netns). Passed for the
  Regular config and for `YOLO_ALLOW_LAN=1` + `ALLOW_DNS_TO`.
- `selftest.sh <image>`: a throwaway "router" container in its own netns
  applies the real generated rules between a fake sandbox network and a fake
  LAN, forwards and masquerades, and blackholes `100.64.0.0/10` and
  `192.168.0.0/16` so nothing leaks even if the rules were wrong. 24/24 probes
  passed with both `ubuntu:26.04` + nftables and Fork A's sandbox image:
  internet allowed; router addresses, LAN TCP, CGNAT, `192.168.1.254` and
  link-local dropped; LAN UDP 41641 delivered and UDP 5353 dropped;
  host→sandbox connection allowed; with `YOLO_ALLOW_LAN=1` only the YOLO
  address reaches the LAN. (The fake LAN target is an address Docker doesn't
  know: Docker's raw `PREROUTING` anti-spoofing drops bridged packets to a
  container's own IP when they arrive from another bridge.)

---

## 2. Home backup (`backup/`)

`headmesh-home-backup` wraps restic:

| Command | Does |
|---|---|
| `init` | Creates `/mnt/storage/backups/restic-home` (root `0700`) and the repository |
| `backup` | `restic backup /home/aaryn --exclude-caches --one-file-system --exclude-file /etc/headmesh/restic-excludes.txt --tag headmesh-home`, then `restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune` |
| `check` | `restic check --read-data-subset=5%` |
| `status` | Prints the status JSON |

Units: `headmesh-home-backup.timer` (nightly 03:15 ±15 min, `Persistent=true`
so a laptop that was asleep runs it at the next boot) and
`headmesh-home-backup-check.timer` (1st of the month, 04:30). Both services run
at idle I/O priority with no network (`PrivateNetwork=yes`), a read-only view of
`/home`, and write access only to the repository and the status directory.

The repository is encrypted with `/etc/headmesh/restic.pass` (root `0400`). The
sandbox never sees it: `agent` can't read root-only files, and the YOLO view must
mask `/etc/headmesh` and keep `/mnt/storage/backups` read-only (see open issues
in the T-7 ADR / parent report).

`restic` is installed by Aaryn as part of host prep (plan §5.3), pinned.

### Install (Aaryn)

```bash
cd /mnt/storage/projects/headmesh/sandbox/host
sudo install -d -m 0755 /etc/headmesh
sudo sh -c 'umask 0277; head -c 48 /dev/urandom | base64 -w0 > /etc/headmesh/restic.pass'
sudo chmod 0400 /etc/headmesh/restic.pass
sudo install -m 0755 -o root -g root backup/headmesh-home-backup /usr/local/sbin/headmesh-home-backup
sudo install -m 0644 -o root -g root backup/restic-excludes.txt /etc/headmesh/restic-excludes.txt
sudo install -m 0644 -o root -g root backup/headmesh-home-backup.service backup/headmesh-home-backup.timer backup/headmesh-home-backup-check.service backup/headmesh-home-backup-check.timer /etc/systemd/system/
sudo headmesh-home-backup init
sudo systemctl daemon-reload
sudo systemctl enable --now headmesh-home-backup.timer headmesh-home-backup-check.timer
sudo systemctl start headmesh-home-backup.service
```

Copy the password into your password manager now
(`sudo cat /etc/headmesh/restic.pass`, paste it there, don't put it in chat or
a file in the repo). Without it the backups can't be read.

The status directory `/mnt/storage/headmesh/sandbox/state` is created by
`headmesh-sandbox init`; until it exists, backups still run but no status file is
written. `/mnt/storage` is at 91%; check the first snapshot's size before
relying on the nightly run (`restic stats` below), and extend
`restic-excludes.txt` if needed.

Optional: let `headmesh-sandbox backup` start the unit without sudo (it starts
or stops only this one unit):

```bash
sudo install -m 0644 -o root -g root backup/50-headmesh-home-backup.rules /etc/polkit-1/rules.d/
```

### Verify

```bash
systemctl list-timers 'headmesh-home-backup*'
journalctl -u headmesh-home-backup.service -n 50
cat /mnt/storage/headmesh/sandbox/state/backup-status.json
sudo restic -r /mnt/storage/backups/restic-home --password-file /etc/headmesh/restic.pass snapshots
sudo restic -r /mnt/storage/backups/restic-home --password-file /etc/headmesh/restic.pass stats latest
```

`headmesh-sandbox up --yolo` requires `last_success_at` to be under 24 h old.

Restore one path into a scratch folder:

```bash
sudo restic -r /mnt/storage/backups/restic-home --password-file /etc/headmesh/restic.pass restore latest --target /mnt/storage/restore-$(date +%F) --include /home/aaryn/path/to/thing
```

### Uninstall

```bash
sudo systemctl disable --now headmesh-home-backup.timer headmesh-home-backup-check.timer
sudo rm -f /etc/systemd/system/headmesh-home-backup.service /etc/systemd/system/headmesh-home-backup.timer /etc/systemd/system/headmesh-home-backup-check.service /etc/systemd/system/headmesh-home-backup-check.timer
sudo rm -f /usr/local/sbin/headmesh-home-backup /etc/headmesh/restic-excludes.txt /etc/polkit-1/rules.d/50-headmesh-home-backup.rules
sudo systemctl daemon-reload
```

This keeps the repository and the password. Delete them deliberately, and only
if the snapshots are no longer wanted: `/mnt/storage/backups/restic-home`,
`/etc/headmesh/restic.pass`.
