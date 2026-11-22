# Headmesh backup configuration

These files configure the hs1 SQLite replica and the PostgreSQL 18 backup
stanza for Cloudflare R2. The ordinary Headmesh bootstrap leaves the backup
services disabled. Set up separate bucket-scoped R2 credentials for
`hm-hs1-litestream` and `hm-pg` before enabling either service.
[Cloudflare R2 S3 API reference](https://developers.cloudflare.com/r2/api/s3/api/)

**Start here.** The [credential setup guide](credential-setup.md) has the
dashboard links, exact file fields and the reason the Headmesh plan chose
R2. Run `deploy/backup/init-credentials.sh` to create the empty private
files outside this repository.

## Production rollout

1. Install the backup commands from a verified Headmesh build. The active
   backend release or a backup-only copy can publish the encrypted Headscale
   bundle. The default manifest unit locates the active backend through
   `deployctl`. Keep the writer credentials on the primary.
2. Copy `publish/litestream.aws`, `publish/keys.netrc`,
   `publish/manifest.netrc` and `publish/manifest.env` to
   `/etc/headmesh/backup/` with mode `0600`, owned by root. Put the
   `HEADMESH_R2_ENDPOINT` line in `/etc/headmesh/backup/litestream.env`.
   Use the matching public age recipients when publishing the key bundle.
3. Install the tools and start Litestream, then verify a fresh SQLite
   restore. Add `publish/headmesh-r2.conf` as
   `/etc/pgbackrest/conf.d/headmesh-r2.conf` with mode `0640`, owned by
   `root:postgres`. Install the base pgBackRest config and enable WAL
   archiving during the PostgreSQL maintenance window. Verify the first
   full backup and its WAL before enabling the timers.
4. Publish the encrypted Headscale bundle and the first recovery manifest.
   Read back the manifest and verify the isolated cloud drill with the
   read-only credentials. Enable the scheduled drill after the manual run
   succeeds.

## Headscale SQLite

1. Install the Litestream v0.5.17 binary from the pinned image in
   [tool-images.sh](tool-images.sh). The unit expects `/usr/bin/litestream`.
2. Install `litestream-r2.yml` as `/etc/headmesh/backup/litestream.yml`.
   Set `HEADMESH_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com`
   in `/etc/headmesh/backup/litestream.env`. Put the bucket key pair in the
   `[default]` profile of `/etc/headmesh/backup/litestream.aws` using
   `aws_access_key_id` and `aws_secret_access_key`. Restrict the credential
   file to root with mode `0600`. systemd loads it into the service's
   private credential directory. The unit also loads `litestream.yml` into
   that directory so the service user can read it from the private backup
   directory.
3. Install `systemd/headmesh-litestream.service`, reload systemd, and
   enable the service after confirming `/var/lib/headscale/db.sqlite`
   belongs to the intended cell. Verify a fresh copy restores from the
   bucket and passes `PRAGMA integrity_check`.

## PostgreSQL 18

1. Install the Ubuntu `pgbackrest` package and copy `pgbackrest-r2.conf`
   to `/etc/pgbackrest/conf.d/pgbackrest-r2.conf`.
   Use `root:postgres` ownership and mode `0750` for `/etc/pgbackrest`
   and its `conf.d` directory so the backup process can read its settings.
   Add a root-owned drop-in in the same directory, readable by the
   `postgres` group with mode `0640`. Its `[global]` section supplies
   `repo1-s3-endpoint` (hostname only), `repo1-s3-key`,
   `repo1-s3-key-secret`, and `repo1-cipher-pass`. Keep an encrypted
   recovery copy of the passphrase.
2. Create `/var/lib/pgbackrest` for the scheduled-backup lock, writable by
   `postgres`. Confirm the stanza points to the host's intended
   PostgreSQL 18 cluster. Install `postgresql-archive.conf` in that cluster's
   `conf.d` directory during a maintenance window. Enabling `archive_mode`
   requires a PostgreSQL restart.
3. As `postgres`, run `pgbackrest --stanza=headmesh stanza-create`. After
   enabling WAL archiving, run `pgbackrest --stanza=headmesh check`, then
   `pgbackrest --stanza=headmesh --type=full backup`. Verify the completed
   backup and its WAL range with `pgbackrest --stanza=headmesh info` before
   enabling the timers. The full backup runs on Sundays at 04:00 UTC and
   the differential runs at the same time on the other six days.

The workstation drill exercises both database configs, the encrypted
key bundle and the isolated restore script against a disposable TLS-enabled
S3 endpoint. Each bucket uses a separate credential. The drill runs
independently of GitHub Actions.

```sh
deploy/tests/object-storage-restore.sh
```

Production acceptance includes a restore from the actual R2 buckets on
an isolated recovery host and the checks in [plan §13.5](../../docs/plan.md#135-restore-drills-requirement-2).

## Headscale keys and configuration

Install `age` on the publisher and recovery host. Supply two distinct
public age recipients, one for the owner and one for recovery. Keep the
private identities separately available during an outage. The command
reads the active Headscale configuration and the Noise and DERP keys
from their normal paths. Use `-config`, `-noise-key`, and `-derp-key` for
a cell with custom paths.

Set `HEADMESH_R2_ENDPOINT` and create a root-owned `0600` netrc file with
a credential scoped to `hm-keys`. Use the endpoint hostname in the
`machine` field and the access key and secret in `login` and `password`.
The publisher needs object read and write access. The recovery host can
use its own read-only credential for this bucket.

```sh
headmesh-server backup-keys publish \
  -recipient "$OWNER_RECIPIENT" -recovery-recipient "$RECOVERY_RECIPIENT" \
  -netrc /etc/headmesh/backup/keys.netrc
```

The object is `hm-keys/hs1/<noise-fingerprint>.tar.age`. Publish on first
setup and after configuration, key or recipient changes. If the Noise key
stays the same, a publish replaces that fingerprint's bundle using an
ETag condition. A new Noise key gets a separate object.

To keep an encrypted copy in `deploy/secrets/`, install SOPS and write a
SOPS YAML envelope. The three source files are archived in memory and the
command writes ciphertext to stdout. [SOPS documentation](https://getsops.io/docs/)

```sh
mkdir -p deploy/secrets
bundle=$(mktemp deploy/secrets/.hs1-keys.XXXXXX)
headmesh-server backup-keys pack -format sops \
  -recipient "$OWNER_RECIPIENT" -recovery-recipient "$RECOVERY_RECIPIENT" \
  > "$bundle" && mv "$bundle" deploy/secrets/hs1-keys.enc.yaml
```

On the isolated recovery host, fetch the published manifest and stage the
keys using either private identity. Keep the identity file at mode `0600`
and choose an output directory that does not exist.

```sh
headmesh-server backup-keys stage -manifest candidate-manifest.json \
  -identity /path/to/recovery.agekey -netrc /path/to/keys.netrc \
  -output-dir /path/to/restore/keys
```

For the SOPS copy, use `-format sops -bundle /path/to/hs1-keys.enc.yaml`
in place of `-netrc`. Staging validates the archive and matches the Noise
fingerprint against the manifest before writing. It creates the directory
at mode `0700` and all three files at mode `0600`. Use those keys in the
manifest verification below. Treat the staged configuration and keys as
private recovery files and remove them when an isolated drill ends.

## Production waitlist snapshot

`backup-waitlist publish` captures the live SQLite database with SQLite's
online backup API. It validates the snapshot, encrypts it to both existing
age recipients and conditionally updates `hm-keys/waitlist/prod.db.age`.
The local waitlist backup timer continues to serve deploy rollback.
[SQLite online backup](https://www.sqlite.org/backup.html)

Use `/etc/headmesh/backup/keys.netrc` for the existing `hm-keys` writer.
Set the two public recipient fields in the root-owned
`/etc/headmesh/backup/waitlist-recipients.env` file. The service reads
`HEADMESH_R2_ENDPOINT` from the existing `litestream.env`.

```dotenv
OWNER_RECIPIENT=
RECOVERY_RECIPIENT=
```

Install `headmesh-backup-waitlist.service` and its timer from `systemd/`.
Run the service once and stage its result using the read-only key below.
After a successful readback, enable the timer for a snapshot every 15 minutes.
For a backup-only `headmesh-server` under `/usr/local/lib/headmesh/backup/`,
use this service drop-in when that build includes `backup-waitlist`.

```ini
[Service]
ExecStart=
ExecStart=/usr/local/lib/headmesh/backup/headmesh-server backup-waitlist publish -netrc %d/keys-netrc
```

## Recovery manifest

Capture the primary's recovery evidence with the deployed `headmesh-server`
binary. Its PostgreSQL connection comes from `HEADMESH_DATABASE_URL` or the
libpq environment. Use a role that can read the grant audit log at system
scope and query `pg_control_system()`. Capture and verification use
read-only transactions.

```sh
headmesh-server backup-manifest capture > candidate-manifest.json
```

Keep the manifest with the corresponding backups. On the isolated recovery
host, connect to the restored PostgreSQL database and point the verifier at
the restored SQLite database and decrypted keys before starting Headscale.

```sh
headmesh-server backup-manifest fetch -netrc /path/to/manifest.netrc \
  > candidate-manifest.json
headmesh-server backup-manifest verify -manifest candidate-manifest.json \
  -sqlite /path/to/restore/db.sqlite \
  -noise-key /path/to/restore/noise_private.key \
  -derp-key /path/to/restore/derp_server_private.key
```

The verifier requires SQLite integrity, live node and user counts at least
as large as the manifest's, and matching Noise and policy fingerprints.
For PostgreSQL it checks the physical cluster ID, database name, replayed
WAL position, and the recorded grant audit event. Use the manifest that
matches the restored policy and falls within the replayed WAL.

### Publish every five minutes

The publisher restores a temporary SQLite copy over TLS and checks that it
covers the candidate counts and policy. It then checks for a completed
pgBackRest full backup on the current timeline and fetches the exact WAL
segment that contains the candidate position. If the segment is still
open, the publisher writes a named restore point, asks PostgreSQL to switch
WAL once, and waits for the normal archiver. The restore-point record lets
the switch proceed when the database is idle. The service account needs
EXECUTE on `pg_create_restore_point(text)` and `pg_switch_wal()`. Scratch
files are removed at exit.

Deploy a release with the publisher command before enabling the timer.
The service uses `deployctl` to locate the active production release. For
a backup-only build installed under `/usr/local/lib/headmesh/backup/`, add
this drop-in to `headmesh-backup-manifest.service`.

```ini
[Service]
ExecStart=
ExecStart=/usr/local/lib/headmesh/backup/headmesh-server backup-manifest publish -netrc %d/manifest-netrc
```

Create the publisher's restricted login once with `manifest-role.sql`,
run as the PostgreSQL administrator in the Headmesh database. In a local
`psql` session, set its password with `\password headmesh_backup_manifest`.
Add its PostgreSQL connection settings to
`/etc/headmesh/backup/manifest.env` and keep that file root-owned with mode
`0600`. Use a `HEADMESH_DATABASE_URL` for `headmesh_backup_manifest`, for
example `postgresql://headmesh_backup_manifest:<url-encoded-password>@127.0.0.1:5432/headmesh_prod`.
Create `/etc/headmesh/backup/manifest.netrc` with the hostname of
the R2 endpoint and a key scoped to the `hm-pg` bucket. Use the netrc
`machine`, `login`, and `password` fields and the same permissions.
The service reads SQLite with the existing Litestream credential.

Once both backups are active and Headscale has a registered node, install the manifest service and timer under
`systemd/`. Reload systemd and run `headmesh-backup-manifest.service` once.
Check that `hm-pg/manifest.json` can be read back and verified in the recovery
environment, then enable `headmesh-backup-manifest.timer`. The command uses
conditional object writes to prevent stale replacements. Schedule the full
restore drill separately to verify the complete backup and WAL chain.

## Isolated restore

Run on a disposable host with enough free space for both databases and the
PostgreSQL WAL replay. Install `headmesh-server`, Litestream, `age`, `curl`,
`jq`, `sqlite3`, pgBackRest and PostgreSQL 18. Add the PostgreSQL 18 binaries
to `PATH`, and run as an unprivileged user with a private working parent.

Set `HEADMESH_R2_ENDPOINT` and Litestream's `AWS_SHARED_CREDENTIALS_FILE`.
Configure a read-only pgBackRest repository in `PGBACKREST_CONFIG` with the
`hm-pg` credential and its encryption passphrase. Supply separate scoped
netrc files for the manifest and keys, and either of the private age
identities. The SQLite config uses the primary path as its replica name.

```sh
deploy/backup/restore-isolated.sh /path/to/private-work /path/to/litestream.yml \
  /path/to/manifest.netrc /path/to/keys.netrc /path/to/recovery.agekey
```

The script downloads and validates the manifest, stages the keys, restores
SQLite, and replays PostgreSQL to the end of the archived WAL stream. It
starts PostgreSQL with a private Unix socket and archive writes disabled.
After manifest verification it stops the temporary database and cleans the
restored files. It reports the elapsed time for backup verification and the
age of the manifest at download.

Use `--stage-for-recovery` before the working directory to keep a verified
restore for the standby bootstrap. The working parent must be owned by the
caller with mode `0700`. The final JSON line gives `staged_directory`, which
contains the manifest, both databases and the decrypted Headscale keys.
PostgreSQL is stopped before the path is returned. Remove the private
directory after consuming the files.

### Allocate and boot a standby on Hetzner

Run from the recovery runner after independent outage confirmation and
fencing. Hold the recovery workflow's exclusive lock across these steps.
Use `HCLOUD_TOKEN` for the Hetzner project. Get the pinned primary server
and Primary IP IDs, the approved standby firewall and SSH key IDs, and the
project server limit from Hetzner. Keep the same `run_id` throughout recovery.
Set `primary_ipv6_id` to `0` if the original server had no Primary IPv6.
Supply restore-mode cloud-init with admin access and no secrets. Hetzner
retains the cloud-init data. [Hetzner Cloud Console](https://console.hetzner.cloud/)

```bash
umask 077
primary=(
  -primary-id "$primary_id" -primary-name "$primary_name"
  -primary-ipv4-id "$primary_ipv4_id" -primary-ipv6-id "$primary_ipv6_id"
  -run-id "$run_id" -firewall-id "$standby_firewall_id"
)
go run ./cmd/headmesh-recover create-standby "${primary[@]}" \
  -server-limit "$server_limit" -ssh-key-ids "$ssh_key_ids" \
  -cloud-init "$cloud_init" >"$allocation"
standby_id="$(jq -er .server_id "$allocation")"
go run ./cmd/headmesh-recover start-standby "${primary[@]}" \
  -standby-id "$standby_id" >"$standby_record"
```

Allocation leaves the VM powered off and checks the assigned addresses and
the applied firewall. Startup checks the same run and server, both public
addresses and the firewall, and rechecks the primary fence before and after
booting. The JSON record includes the server ID, power state, restore payload
architecture and addresses. Verify SSH and cloud-init before copying any
credentials or state. [Hetzner server addressing](https://docs.hetzner.com/cloud/servers/getting-started/connecting-to-the-server/)

If allocation returns an ambiguous result, find the matching recovery server
in Hetzner and reconcile its identity before continuing with the same run ID.
If a boot response is lost, retry `start-standby` with the same inputs. It
accepts an already running standby only after the provider checks pass.

### Install the staged files on a standby

From the matching checkout, package the deployment toolkit and transfer it
over verified SSH to a fresh Ubuntu 26.04 standby. Use the standby's admin
SSH destination in `standby_ssh`.

```sh
toolkit="$(mktemp -d)"
deploy/deploy.sh --package-recovery-toolkit "$toolkit"
tar -C "$toolkit" -cf - . | ssh "$standby_ssh" '
  sudo -n install -d -m 0755 /usr/local/lib/headmesh &&
  sudo -n tar -xf - --no-same-owner -C /usr/local/lib/headmesh &&
  sudo -n /usr/local/lib/headmesh/bootstrap.sh prepare-recovery'
```

Preparation installs PostgreSQL 18, pgBackRest, Headscale, nginx, Node,
certbot and the service units and users. It generates `en_US.UTF-8` for the
restored PostgreSQL databases and disables automatic cluster creation before
installing PostgreSQL. nginx and Headscale stay persistently masked. Use
the restore payload to supply Litestream and `headmesh-server` for staging.
Build it with Docker, `jq` and `deploy/ha/package-restore.sh "$payload_dir" "$architecture"`,
using an existing empty directory and the standby's `amd64` or `arm64`
architecture.
[PostgreSQL cluster startup control](https://manpages.debian.org/unstable/postgresql-common/pg_createcluster.1.en.html)

Run the isolated restore with `--stage-for-recovery` as `postgres`. Allow
room for the stage and the installed copy. Run the installer as root with
the reported directory.

```sh
/usr/local/lib/headmesh/bootstrap.sh restore "$staged_directory"
```

The installer refuses an existing cluster or existing Headscale state. It
installs PostgreSQL under `/var/lib/postgresql/18/main`, remaps any staged
tablespaces and replaces the old PostgreSQL configuration. It puts the
SQLite database and both keys in `/var/lib/headscale`, and retains the verified
manifest and original Headscale configuration in `/var/lib/headmesh/recovery`.
The PostgreSQL cluster is disabled and Headscale stays masked until the standby's
configuration and recovery startup are ready.

### Start the restored backend and cell agent

Install the matching `headmesh-server` and `headmesh-cellagent` release under
`/srv/headmesh/prod/backend/releases/`. Restore the original private backend configuration
under `/etc/headmesh/prod/`, including `backend.env`, `db.env`, `secrets.env`,
the operation-signing key, the cell CA certificate and any OAuth secrets that
configuration references. Install the backend's TLS certificate and key under
`/etc/headmesh/tls/`. Restore the local agent's configuration, mTLS client
certificate and key under `/etc/headmesh-cellagent/` with the original trust
and guardrails. Keep nginx masked on the standby.

```sh
/usr/local/lib/headmesh/deployctl.sh start-recovery "$release"
```

The command checks the installed PostgreSQL identity and WAL position, then
starts Headscale on loopback and checks its served Noise identity and node
count. It starts the API in recovery mode, confirms readiness and writes are
blocked, and connects the agent directly to its loopback mTLS listener. If
the checks fail, the candidate stops and the databases stay installed for
retry. After readiness succeeds, the command records the release in
`/var/lib/headmesh/recovery/backend-ready`. nginx stays masked until the
ingress step. Always check live readiness again before promotion.

### Verify standby ingress before DNS

Stage the off-host waitlist snapshot from `hm-keys` with either private age
identity and the bucket's read-only `keys.netrc`. Select a new file under
a private directory. A standalone SQLite backup from `deployctl.sh backup
prod` can also be supplied to the standby installer.

```sh
headmesh-server backup-waitlist stage \
  -identity "$recovery_identity" -netrc "$keys_netrc" -output "$waitlist_snapshot"
```

Install the matching waitlist release under
`/srv/headmesh/prod/waitlist/releases/` and restore the original
`/etc/headmesh/prod/waitlist.env`, including its original `WAITLIST_SECRET`
so outstanding confirmation and unsubscribe links still work. The
`headmesh` service account must be able to read its release. After backend
recovery, start the restored waitlist with nginx still masked.

```sh
/usr/local/lib/headmesh/deployctl.sh start-recovery-waitlist "$waitlist_release" "$waitlist_snapshot"
```

The command validates the database and starts it on loopback. It refuses to
replace an existing database. Once it returns successfully, remove the
private staged copy.

Install the DNS-01 certificate and key under `/etc/headmesh/tls/` before
starting nginx. Install the production marketing release under
`/srv/headmesh/prod/marketing/releases/`, including `site/` and `nginx/`.
Render the production nginx configuration from the matching checkout.
[Let's Encrypt DNS-01](https://letsencrypt.org/docs/challenge-types/#dns-01-challenge)

```sh
deploy/deploy.sh --render-recovery-nginx "$rendered_nginx"
```

Transfer that directory to the standby. With nginx still masked, run this
as root with the directory you transferred and the assigned standby addresses
from `start-standby`.

```sh
/usr/local/lib/headmesh/deployctl.sh prepare-recovery-ingress \
  "$rendered_nginx" "$site_release" "$standby_ipv4" "$standby_ipv6"
```

The command checks live backend and waitlist readiness, that both addresses are assigned
to the standby and that the certificate is trusted for the production names.
After validating the nginx config it switches Headscale's advertised DERP
addresses to the standby and starts native STUN on UDP 3478. It waits for the
cell agent to reconnect, starts nginx and checks local HTTPS. If a check fails,
it masks nginx and restores Headscale's loopback STUN listener.
[Headscale embedded DERP and STUN](https://headscale.net/stable/ref/derp/#enable-embedded-derp)

Run the external proof from an independent runner with IPv4 and IPv6
connectivity. Pass the addresses assigned by Hetzner and the manifest
used for the restore.

```sh
go run ./cmd/headmesh-recover prove-ingress \
  -ipv4 "$standby_ipv4" -ipv6 "$standby_ipv6" -manifest "$verified_manifest"
```

The command connects directly to both addresses. It verifies TLS, Headscale
health and Noise identity, the HTTPS DERP probe, API readiness, the site and
its `www` redirect, and both the waitlist route and database readiness.
It also requires a STUN Binding Success response with a usable client address
and port from each address on UDP 3478. Use a standby firewall that admits
the runner over TCP 443 and UDP 3478 on both address families.
[Direct HTTPS resolution and SNI](https://everything.curl.dev/usingcurl/connections/name.html)

Keep DNS pointed at the existing records during verification.

### Make the verified standby bootable

With the primary fenced and the recovery lock held, run this as root on the
standby. Pause ordinary deploys to the standby until recovery and cutover
are complete.

```sh
/usr/local/lib/headmesh/deployctl.sh finalize-recovery
```

The command rechecks trusted loopback HTTPS and the restored Noise identity.
It reconnects the restored cell agent through nginx's mTLS proxy and waits
for a fresh Hello. A connection failure restores the direct recovery route.
On success, it records the active Blue slots, marketing release and restored
schema version in deploy state. It enables PostgreSQL, Headscale, the Blue
API and waitlist, nginx and the cell agent at boot. The Blue API runs its
recovery policy and acknowledgement checks on startup.

Reboot the standby before changing DNS. After SSH returns and the API is
ready, run `finalize-recovery` again to check the saved state and boot
links. Repeat `headmesh-recover prove-ingress` from the dual-stack runner
to check both standby addresses after the reboot. Schedule certificate
renewal and offsite backup publishers for the replacement primary after
cutover.

### Review and apply the Cloudflare DNS cutover

Use the same primary and standby pins as allocation and keep the recovery
lock held. Set `HCLOUD_TOKEN`, `CLOUDFLARE_DNS_API_TOKEN` scoped to the
`headmesh.dev` zone, and `zone_id` from the Cloudflare dashboard. An inventory
only needs DNS Read. Cutover needs DNS Write. Keep the preview in the private
recovery directory because it includes the origin addresses.
[Cloudflare dashboard](https://dash.cloudflare.com/)

```bash
umask 077
go run ./cmd/headmesh-recover plan-dns "${primary[@]}" \
  -standby-id "$standby_id" -zone-id "$zone_id" >"$dns_preview"
```

After approval for the cutover, run with the manifest used to restore the
standby. The command repeats the ingress proof and verifies the fence and
provider records before sending the batch. It then reads back the resulting
records. A retry for the same verified standby reconciles an ambiguous write
from Cloudflare's current records. An unexpected origin stops the cutover.

```bash
go run ./cmd/headmesh-recover promote-dns "${primary[@]}" \
  -standby-id "$standby_id" -zone-id "$zone_id" -manifest "$verified_manifest"
```

The batch preserves the web records' proxy modes and TTLs. It moves `hs1`
as DNS-only with a 60-second TTL and adds its AAAA record if needed. The
managed hosts are `headmesh.dev`, `www.headmesh.dev`, `api.headmesh.dev`
and `hs1.headmesh.dev`. Cloudflare applies the batch in one transaction.
DNS propagation can occur at different times for each record. Before a
scheduled game day, lower the live `hs1` TTL and allow the previous TTL to
expire. Confirm the public resolve and client checks before declaring recovery.
[Cloudflare batch DNS updates](https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/batch/)

### Headscale startup and registration

Add `--check-cell` before the working directory to check the restored cell.
Install the deployed versions of `headscale`, `tailscale`, and `tailscaled`,
plus `ip` and `openssl`. The host needs `unshare` and support for unprivileged
user and network namespaces.

```sh
deploy/backup/restore-isolated.sh --check-cell /path/to/private-work /path/to/litestream.yml \
  /path/to/manifest.netrc /path/to/keys.netrc /path/to/recovery.agekey
```

This check starts Headscale on loopback with the restored SQLite database,
Noise key and DERP key. It creates a drill CA, verifies that Headscale
serves the manifest's Noise identity, checks the exact node count, and
validates the recovered policy with `headscale policy check`. A userspace
Tailscale client registers over TLS with an ephemeral key and in-memory
state. The network namespace has no path to other networks. All processes
and temporary credentials are removed afterward.
[Tailscale userspace networking](https://tailscale.com/docs/concepts/userspace-networking)

### Headmesh recovery and audit

Use `--check-control-plane` to include the restored cell and Headmesh API.
Install `headmesh-cellagent` from the same release as `headmesh-server`.
The drill restores one Headscale database and can exercise one enforce-mode
cell. Recovery with multiple enforce-mode cells needs each cell's backup.

```sh
deploy/backup/restore-isolated.sh --check-control-plane /path/to/private-work /path/to/litestream.yml \
  /path/to/manifest.netrc /path/to/keys.netrc /path/to/recovery.agekey
```

The drill starts `headmesh-server serve -recovery` on the isolated network
with throwaway signing and cell credentials. It checks that readiness and
writes are gated while the enforce-mode cell is offline, connects the cell
agent over mutual TLS, and verifies a recorded policy acknowledgement.
The sweep records `grant.expired_late` events for overdue grants. An expiry
sweep error or a cell timeout keeps writes gated while recovery retries.

`headmesh-server backup-manifest audit` recomputes every restored
organization's audit chain and the tenant-less chain, up to the stored
chain heads. The script also compares an authenticated `GET /v1/grants`
response to one organization's restored grant set. The API runs with
row-level security and the sample key expires after five minutes.
It reports elapsed service verification time. Record cloud VM creation
and deletion and production backup freshness in the full
[restore drill](../../docs/plan.md#135-restore-drills-requirement-2).

## Hetzner restore drill

The cloud runner creates a disposable `cx23` in `fsn1` and runs the full
isolated verification. It uses Ubuntu 26.04, labels the VM as a drill, and
authenticates SSH against a host key generated for that run. It verifies
cloud-init and the required network namespace before uploading the backup
credentials. The runner starts the recovered services inside the namespace
and confirms the VM is deleted before emitting the result. [Hetzner Cloud API](https://docs.hetzner.cloud/reference/cloud)

The manual runner uses Python 3, Docker, `jq`, the repository's Go toolchain and
OpenSSH. Set `HCLOUD_TOKEN` for the Hetzner recovery project and
`HEADMESH_R2_ENDPOINT` for its backups. Provide these private files with mode
`0600` in the credentials directory.

| File | Contents |
| --- | --- |
| `litestream.aws` | Read-only access to `hm-hs1-litestream` |
| `manifest.netrc` | Read-only access to the `hm-pg` manifest |
| `keys.netrc` | Read-only access to `hm-keys` |
| `recovery.agekey` | One approved recovery identity |
| `pgbackrest.conf` | Recovery repository configuration, read-only access to `hm-pg`, and its encryption passphrase |

```sh
python3 deploy/ha/restore_drill.py run --credentials /path/to/private-drill-credentials
```

`--firewall-id` attaches a provider firewall configured to admit the SSH
runner and allow the VM to reach R2. RTO measures provider creation through
service verification. Recovery point age measures the age of the verified
manifest when the VM fetched it. The runner emits those values in JSON.

The `restore-drill.yml` workflow runs Mondays at 06:00 UTC and on demand.
Its `recovery` environment uses secrets `HCLOUD_TOKEN`,
`DRILL_LITESTREAM_AWS`, `DRILL_MANIFEST_NETRC`, `DRILL_KEYS_NETRC`,
`DRILL_AGE_IDENTITY` and `DRILL_PGBACKREST_CONFIG`. Set the environment
variable `HEADMESH_R2_ENDPOINT`. `HEADMESH_DRILL_FIREWALL_ID` is optional.
The workflow publishes the JSON metrics as a build artifact.

The hourly janitor deletes drill servers older than two hours. It verifies
the drill's name and ownership labels before every deletion. The same
command runs manually with `HCLOUD_TOKEN`.

```sh
python3 deploy/ha/restore_drill.py janitor
```
