# prod-hetzner Ubuntu 24.04 -> 26.04 upgrade - agent handoff

prod-hetzner (46.225.20.97, hs.xcelsior.ca) runs the production Headscale and the WireGuard backdoor hub; still on Ubuntu 24.04. The tower did this upgrade on 2026-10-26 - these steps carry its lessons. The agent has NO SSH access to prod-hetzner: give Aaryn paste-ready commands, he runs them.

Before:
1. Hetzner console: take a snapshot first (fast undo). Pick a window - the tailnet control plane is down around the reboot.
2. DONE (2026-10-26) - stale Google Coral repo removed.
3. `sudo apt-get update && sudo apt-get upgrade -y`; reboot if `/var/run/reboot-required` exists.
4. Save state: `dpkg --get-selections > ~/pre-upgrade-selections.txt` and `apt-mark showhold > ~/pre-upgrade-holds.txt`.
5. Check `apt-mark showhold` - held packages make do-release-upgrade abort with "held broken packages". Unhold them (or accept their removal) before starting.
6. Move the third-party sources out of /etc/apt/sources.list.d/ (docker, grafana, tailscale, nodesource, deadsnakes) - keep only `ubuntu.sources`. Otherwise the upgrader refuses with "install all available updates". Backup target: `sudo mkdir -p /root/apt-sources-backup-24.04`.

Upgrade:
7. In a `tmux` session: `sudo do-release-upgrade` (target is 26.04.1 LTS). The non-interactive frontend does NOT reboot at the end - reboot manually right after: `sudo systemctl reboot`. Do not debug session/service weirdness before that reboot; on the tower it was all just the un-rebooted upgrade (new userland, old loaded kernel module).

After:
8. Verify: `headscale nodes list`, `sudo wg show` (fresh handshakes), `docker ps`, `tailscale status`, `systemctl --failed` (expect none).
9. Restore the third-party sources with `noble` changed to `resolute`; `sudo apt-get update` must exit 0. Leave out any repo with no resolute build yet and note which one.
10. Attach Ubuntu Pro (free personal subscription, same as ASUS/tower): `sudo pro attach <token>` - Aaryn supplies the token at run time; never write it into this repo.

Paste-ready order (2026-10-27). Run on prod-hetzner over `ssh` to 46.225.20.97 unless marked ASUS:
1. Console: snapshot `pre-26.04-2026-10-27`; silence alerts for 90 minutes.
2. ASUS: `sudo systemctl stop headscale-failover-watchdog.timer headscale-replicate.timer`
3. `sudo docker builder prune -af --filter until=168h && sudo docker image prune -af --filter until=336h && df -h /`
4. Baseline: see the command in the 2026-10-27 chat or plan section 5.7; it saves to `/root/upgrade-baseline-2026-10-27/`.
5. `sudo find / -xdev -name pyvenv.cfg -not -path '/var/lib/docker/*' -exec grep -H '^version' {} + 2>/dev/null` - host venvs built on noble's Python 3.12 must be rebuilt with uv after the upgrade.
6. Handoff steps 3-7 (update, holds, sources out, tmux + do-release-upgrade, reboot). Keep local config files when asked.
7. Handoff step 8 plus: baseline node count matches, the loop volume is mounted, and `curl -fsS https://hs.xcelsior.ca/health` succeeds.
8. Restore sources: `sudo sh -c 'for f in /root/apt-sources-backup-24.04/*; do sed "s/noble/resolute/g" "$f" > "/etc/apt/sources.list.d/$(basename "$f")"; done' && sudo apt-get update`
9. `read -rs -p 'Ubuntu Pro token: ' PRO_TOKEN; echo; sudo pro attach "$PRO_TOKEN"; unset PRO_TOKEN`
10. ASUS: `sudo systemctl start headscale-failover-watchdog.timer headscale-replicate.timer`

Extra safeguards (docs/plan.md section 5.7, approved 2026-10-27): do the Docker reclaim first; silence alerts; pause the ASUS `headscale-failover-watchdog.timer` and `headscale-replicate.timer` before the reboot and restart them after; SSH over the public IP 46.225.20.97, not the tailnet or wg-backdoor; capture a baseline to `/root/upgrade-baseline-2026-10-27/`; answer No to removing obsolete packages if anything calls a deadsnakes Python by version; after the reboot also check the 100 GB loop volume is mounted and the 100.64.0.1/.3/.6 nodes are online; type the Pro token with `read -rs PRO_TOKEN; sudo pro attach "$PRO_TOKEN"; unset PRO_TOKEN`; rollback = rebuild from the snapshot if Headscale or WireGuard is not fixed within 30 minutes.

## Outcome (2026-10-27)
- Upgraded to Ubuntu 26.04.1, kernel 7.0.0-38, between 06:01 and 06:19 UTC, then rebooted. The upgrader skipped obsolete-package removal. A manual snapshot was taken first.
- Post-checks (verified 09:20 UTC):
  - **Headscale:** active with 6 nodes; the 5 online match the baseline. 100.64.0.1, .3 and .6 are online, and `hs.xcelsior.ca/health` returns 200.
  - **WireGuard:** the 2 active `wg0` peers have fresh handshakes. The other 2 were already idle for more than 30 hours before the upgrade.
  - **Docker:** 14 of 17 containers running, the same as the baseline (the 3 stopped ones are the blue rollback containers).
  - **Services:** every service from the baseline is running. The volume `/exports/volumes/vol-02ca52e5d40d` is mounted, and there are 0 failed units.
- **Sources:** Docker, Tailscale and deadsnakes point at `resolute`; nodesource (`nodistro`) and Grafana (`stable`) are unchanged; `apt-get update` is clean. Ubuntu Pro is attached (esm-apps, esm-infra, livepatch).
- **Python:** deadsnakes `resolute` builds of 3.9.25, 3.10.22 and 3.12.16 back the host venvs; PixelEnhance prod `.venv` runs on 3.12.16. All deadsnakes Python packages were `apt-mark manual`'d, because `apt autoremove` would otherwise remove them and break PixelEnhance. `backend/venv` and `onnx2tf_venv_py310` point at macOS paths; they were already broken and nothing uses them.
- **Cleanup:** `openipmi.service` disabled (no IPMI device on a cloud VM). The alert silence was expired and the ASUS failover timers resumed.
- **Follow-ups:**
  - PostgreSQL 16 (all 3 clusters) still runs the noble build `16.15-0ubuntu0.24.04.1`, which gets no more updates on 26.04. Either add the PGDG `resolute` repo to keep 16 patched, or `pg_upgradecluster` to 18. Both need a maintenance window.
  - Delete the snapshot after 7 stable days.
  - Rotate the Ubuntu Pro token.
