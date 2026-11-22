# Headmesh backup configuration

These files configure the hs1 SQLite replica and the PostgreSQL 18 backup
stanza for Cloudflare R2. The ordinary Headmesh bootstrap leaves the backup
services disabled. Set up separate bucket-scoped R2 credentials for
`hm-hs1-litestream` and `hm-pg` before enabling either service.
[Cloudflare R2 S3 API reference](https://developers.cloudflare.com/r2/api/s3/api/)

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
   then `check` and `--type=full backup` with the same stanza. Verify the
   new backup in `pgbackrest --stanza=headmesh info` before enabling the
   timers under `systemd/`. The full backup runs on Sundays at 04:00 UTC.
   The differential runs at the same time on the other six days.

The workstation drill exercises both configs against a disposable,
TLS-enabled S3 endpoint with a different credential for each bucket.
It runs independently of GitHub Actions.

```sh
deploy/tests/object-storage-restore.sh
```

Production acceptance includes a restore from the actual R2 buckets on
an isolated recovery host and the checks in [plan §13.5](../../docs/plan.md#135-restore-drills-requirement-2).
