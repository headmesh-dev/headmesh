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

1. Deploy a release containing the backup commands and `deployctl` support
   for manifest publication. Keep the writer credentials on the primary.
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

1. Verify and install the Litestream v0.5.17 release package. The unit expects
   `/usr/bin/litestream`. [Release and checksums](https://github.com/benbjohnson/litestream/releases/tag/v0.5.17)
2. Install `litestream-r2.yml` as `/etc/headmesh/backup/litestream.yml`.
   Set `HEADMESH_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com`
   in `/etc/headmesh/backup/litestream.env`. Put the bucket key pair in the
   `[default]` profile of `/etc/headmesh/backup/litestream.aws` using
   `aws_access_key_id` and `aws_secret_access_key`. Restrict the credential
   file to root with mode `0600`. systemd loads it into the service's
   private credential directory.
3. Install `systemd/headmesh-litestream.service`, reload systemd, and
   enable the service after confirming `/var/lib/headscale/db.sqlite`
   belongs to the intended cell. Verify a fresh copy restores from the
   bucket and passes `PRAGMA integrity_check`.

## PostgreSQL 18

1. Install the Ubuntu `pgbackrest` package. Put the contents of
   `pgbackrest-r2.conf` in `/etc/pgbackrest/conf.d/headmesh.conf`.
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
3. Run `sudo -u postgres pgbackrest --stanza=headmesh stanza-create`,
   then `check` and `--type=full backup` with anza=headmesh info` before enabling the
   timers under `systemd/`. The full backup runs on Sundays at 04:00 UTC.
   The differential runs at the same time on the other six days.

The workstation drill exercises both database configs, the same stanza. Verify the
   new backup in `pgbackrest --stthe encrypted
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
The service uses `deployctl` to locate the active production release.
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

Once both backups are active, install the manifest service and timer under
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

The manual runner uses Python 3, Docker, the repository's Go toolchain and
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
