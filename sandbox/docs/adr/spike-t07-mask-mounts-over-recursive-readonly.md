# Spike T-7: secret masks over a recursive read-only `/host` bind

- Status: done (2026-10-27), decision below changes the YOLO mount design
- Plan: §6.4 (YOLO `/` → `/host` with `bind.recursive: readonly`, masks for denylisted paths), §17 T-7
- Environment: ASUS, Ubuntu 26.04.1, kernel 7.0.0-38, Docker 29.1.3 (data-root `/mnt/storage/docker`), Compose 2.40.3

## Question

Do tmpfs masks (directories) and `/dev/null` binds (files) apply over a
`bind.recursive: readonly` parent bind of the host root? Does Docker order
mounts by destination depth regardless of how they are listed? Is the parent
recursively read-only, submounts included, and does it fail closed?

## Test

Throwaway trees with fake secrets, one on the root filesystem
(`/home/aaryn/.cache/hm-spike-t7`) and one on the `/mnt/storage` submount
(scratchpad `t7tree`). Compose services built from `ubuntu:26.04@sha256:19fcd1bf…`
bind `/` → `/host` read-only and list the four masks **before** the parent.
Variants: `bind.recursive: readonly` with propagation `rprivate`, `rslave`,
`rshared` and unset; plain `read_only: true`; `bind.recursive: writable`. Each
container prints `/proc/self/mountinfo` for `/host`, lists masked paths, and
tries to write as container root (default capabilities).

## Evidence

| Variant | Result |
|---|---|
| `recursive: readonly`, `propagation: rprivate` | **Refused by Docker:** `invalid mount config: must use either propagation mode "rslave" or "rshared" when mount source is within the daemon root, daemon root: "/mnt/storage/docker", bind mount source: "/"` |
| `recursive: readonly`, `rslave` or unset | Starts. `/host`, `/host/mnt/storage`, `/host/boot/efi` all `ro`. Writes on the root fs **and** the submount fail with `Read-only file system`. Masks: both masked dirs list 0 entries, both masked files read empty, unmasked siblings readable. `umount` of a mask from inside fails (`must be superuser`, no CAP_SYS_ADMIN). |
| `recursive: readonly`, `rshared` | Same view inside, **but the four mask mounts propagated back into the host mount namespace and stayed after the container exited** (`findmnt` on the host shows tmpfs/`udev[/null]` on the test paths, propagation `shared`). Re-running `rslave` and unset on fresh paths leaked nothing. |
| plain `read_only: true` | Also recursively read-only on this kernel (Docker ≥ 25 default), but Docker silently falls back to non-recursive on kernels without `mount_setattr`; not fail-closed. |
| `recursive: writable` | Top level `ro`, but `/host/mnt/storage` is `rw` and the write to the submount **succeeded**. |

Mount order: in every variant mountinfo lists `/host` first, then its submounts,
then the masks, although the masks were listed first in Compose. Docker sorts
mounts by destination depth.

**Late mounts (the important finding).** With the `/` bind on `rslave` (the only
propagation Docker accepts for `/` here), mounts the host creates *after* the
container starts are propagated into `/host` **read-write**: recursive
read-only is applied once at setup. Reproduced by starting a second throwaway
container while the YOLO-like container ran: its overlay rootfs appeared at
`/host/mnt/storage/docker/overlay2/<id>/merged` as `rw`, and `touch` from the
YOLO-like container created a file visible inside the other container. The same
applies to anything mounted later on the host: USB drives under `/media`,
`/run/user/1000` at login, an autofs remount of `/mnt/storage`.

Mitigations tested:
- A tmpfs mask over `/host/mnt/storage/docker` hides the data-root and every
  overlay mounted under it later (`ls` gives 0 entries; the late overlay path
  does not exist in the container).
- Per-subtree binds (`/home`, `/etc`, `/mnt/storage/.claude-tmp` as a stand-in
  for project trees) with `bind.recursive: readonly` and `propagation:
  rprivate` are accepted (their sources don't contain the daemon root),
  report propagation `private`, are read-only including submounts, and masks
  apply on top.

## Outcome

- Masks over a recursive read-only parent work, and Docker orders by depth,
  so listing order in the generated override does not matter. ✅
- `bind.recursive: readonly` is the fail-closed choice (Docker returns an error
  instead of downgrading where recursive read-only is unsupported); keep it on
  every `/host` bind. ✅
- A single `/` → `/host` bind is **not** acceptable for YOLO as planned:
  `rprivate` is refused, `rshared` leaks mounts onto the host, and `rslave`
  exposes later host mounts read-write (including other containers' rootfs).

## Decision

`headmesh-sandbox render` builds the YOLO host view from **per-subtree binds**,
not one `/` bind (this is the plan's T-7 fallback, plus masks):

1. For each entry of `/` except `proc`, `sys`, `dev`, `run`, `tmp`,
   `lost+found` and swap files, bind `/X` → `/host/X` with `read_only: true`,
   `bind.recursive: readonly`, `bind.propagation: rprivate`.
2. For a directory on the path to the Docker data-root (`/mnt/storage` here;
   also `/var/lib/docker`, `/var/lib/containerd` if present), descend one level
   and bind its children individually, **skipping the data-root itself**, so
   no bind contains it and `rprivate` is accepted. The data-root (all other
   containers' layers and volumes) is then never visible.
3. Layer the denylist masks (tmpfs for directories, `/dev/null` for files) on
   top, as planned. `render` refuses `propagation: rshared` anywhere.
4. Never use `rshared`; never bind a path that contains the data-root.

With `rprivate`, nothing the host mounts later appears inside the container.
Paths created on the host later at the top level of `/` won't appear either,
which is acceptable for a time-limited YOLO session.

## Cleanup note

The `rshared` run left four inert mounts on the host (throwaway paths only).
They disappear at reboot; to remove them now:

```bash
sudo umount /home/aaryn/.cache/hm-spike-t7/secretdir /home/aaryn/.cache/hm-spike-t7/netrc
sudo umount /mnt/storage/.claude-tmp/claude-1000/-mnt-storage-projects-headmesh/f19d13e1-7180-46e2-8230-f77cd8019b9b/scratchpad/forkb/t7tree/secretdir /mnt/storage/.claude-tmp/claude-1000/-mnt-storage-projects-headmesh/f19d13e1-7180-46e2-8230-f77cd8019b9b/scratchpad/forkb/t7tree/creds.json
rm -rf /home/aaryn/.cache/hm-spike-t7
```
