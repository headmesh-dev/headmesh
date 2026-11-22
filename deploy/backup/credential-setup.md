# Set up Headmesh backup credentials

Start with the private templates. The script creates empty files at mode
`0600` outside the checkout and preserves files you already filled in.

```sh
deploy/backup/init-credentials.sh
```

The default directory is `~/.config/headmesh/recovery/`, or
`$XDG_CONFIG_HOME/headmesh/recovery/` when `XDG_CONFIG_HOME` is set. It
contains `recovery.env`, `publish/` for the primary, and `drill/` for the
temporary restore VM. Save secrets there or in your password manager.

## Why R2 for Headmesh

Headmesh [ADR 0006](../../docs/adr/0006-one-vm-with-on-demand-standby.md)
chose R2 for the continuous SQLite and PostgreSQL backups. The plan
estimated about 300,000 monthly write-class requests against R2 Standard's
included 1 million, with free direct egress for restore drills. Xcelsior's
existing B2 object storage is a separate deployment. [R2 pricing](https://developers.cloudflare.com/r2/pricing/)

The Headmesh manifest and encrypted key publisher also rely on atomic
`If-Match` and `If-None-Match` writes to protect the latest recovery evidence
from stale writers. Cloudflare documents this behavior for R2. Backblaze's
S3 upload reference leaves those headers unspecified. A move to B2 needs a
provider compatibility drill and a publishing strategy that preserves the
same protection. [Cloudflare S3 compatibility](https://developers.cloudflare.com/r2/api/s3/api/#object-level-operations) · [Backblaze S3 upload reference](https://www.backblaze.com/apidocs/s3-put-object)

## 1. Create the private buckets

Open the Headmesh Cloudflare account. Go to **Storage & databases → R2 →
Overview**. Enable R2 if prompted. Use **Create bucket** to create these
three Standard buckets with private access.

| Bucket | Stores |
| --- | --- |
| `hm-hs1-litestream` | Headscale's SQLite replica |
| `hm-pg` | Encrypted PostgreSQL backups, WAL and `manifest.json` |
| `hm-keys` | The age-encrypted Headscale config and private keys |

Copy the account's **S3 API endpoint** from R2 Overview into
`HEADMESH_R2_ENDPOINT` in `recovery.env`. This field takes the full HTTPS
URL. The `machine` field in netrc files and `repo1-s3-endpoint` in pgBackRest
files take only the hostname, for example `<account-id>.r2.cloudflarestorage.com`.
[Open R2 Overview](https://dash.cloudflare.com/?to=/:account/r2/overview) · [Cloudflare bucket setup](https://developers.cloudflare.com/r2/get-started/s3/)

## 2. Create the six scoped S3 keys

In R2 Overview, select **Manage API Tokens → Create Account API token**.
For each row choose **Apply to specific buckets only** and select exactly
the named bucket. Copy the **Access Key ID** and **Secret Access Key**
when Cloudflare displays them. Use the S3 credentials shown on that page.
[Cloudflare R2 S3 token walkthrough](https://developers.cloudflare.com/r2/get-started/s3/#2-generate-api-credentials)

| Bucket | Permissions | Fill in these files |
| --- | --- | --- |
| `hm-hs1-litestream` | Object Read & Write | `publish/litestream.aws` |
| `hm-hs1-litestream` | Object Read only | `drill/litestream.aws` |
| `hm-pg` | Object Read & Write | `publish/headmesh-r2.conf` and `publish/manifest.netrc` |
| `hm-pg` | Object Read only | `drill/pgbackrest.conf` and `drill/manifest.netrc` |
| `hm-keys` | Object Read & Write | `publish/keys.netrc` |
| `hm-keys` | Object Read only | `drill/keys.netrc` |

- In `.aws` files paste the ID and secret into `aws_access_key_id` and `aws_secret_access_key`.
- In `.netrc` files fill `machine` with the endpoint hostname, `login` with the ID, and `password` with the secret.
- In pgBackRest files fill `repo1-s3-endpoint`, `repo1-s3-key` and `repo1-s3-key-secret`.

## 3. Set the encryption secrets

Generate one random pgBackRest passphrase. A password manager or
`openssl rand -hex 32` can generate it. Fill `repo1-cipher-pass` in both
pgBackRest files with that same value and keep a recovery copy of it.

Use your existing owner and recovery age identities if you have them.
Otherwise, generate two identities. Each command creates a private file
and prints its public recipient. Run these commands in a trusted terminal.

```sh
credentials="${XDG_CONFIG_HOME:-$HOME/.config}/headmesh/recovery"
age-keygen -o "$credentials/owner.agekey"
age-keygen -o "$credentials/drill/recovery.agekey"
```

Fill `OWNER_RECIPIENT` and `RECOVERY_RECIPIENT` in `recovery.env` with the
two `age1…` public values. The primary uses only those public values. Keep
the private identities in independently accessible recovery storage. The
drill reads the private recovery identity from `drill/recovery.agekey`.
[age command-line documentation](https://github.com/FiloSottile/age#usage)

## 4. Create the Hetzner token

Open the **Headmesh** project in Hetzner Console. Select **Security → API
tokens → Generate API token**. Give it a name such as `headmesh-restore-drill`
and choose **Read & Write** so the runner can create and remove the
temporary server. Save it in `HCLOUD_TOKEN` in `recovery.env`.
`HEADMESH_DRILL_FIREWALL_ID` is optional and takes the ID of a firewall for
that drill VM. [Hetzner Console](https://console.hetzner.com/) · [Hetzner token walkthrough](https://docs.hetzner.com/cloud/api/getting-started/generating-api-token/)

## 5. Activate the primary and verify the drill

`publish/manifest.env` has a blank `HEADMESH_DATABASE_URL`. It belongs to
the PostgreSQL role created by [manifest-role.sql](manifest-role.sql) on
the primary. The database connection is provided during the production
rollout. It comes from PostgreSQL on the server.

Check the drill's credentials before a VM is created. This command lists
each backup bucket using the configured read credentials.

```sh
credentials="${XDG_CONFIG_HOME:-$HOME/.config}/headmesh/recovery"
source "$credentials/recovery.env"
export HEADMESH_R2_ENDPOINT
python3 deploy/ha/restore_drill.py preflight --credentials "$credentials/drill"
```

After the buckets and credentials are ready, follow the production rollout
in the [backup runbook](README.md#production-rollout). The rollout installs
the tools and credential files, starts the replica, enables PostgreSQL WAL
archiving in a maintenance window, and verifies the first published
manifest and key bundle. Use the manual cloud drill for the first acceptance
run. It works independently of GitHub Actions.

```sh
credentials="${XDG_CONFIG_HOME:-$HOME/.config}/headmesh/recovery"
source "$credentials/recovery.env"
export HCLOUD_TOKEN HEADMESH_R2_ENDPOINT
args=(--credentials "$credentials/drill")
if [[ -n "$HEADMESH_DRILL_FIREWALL_ID" ]]; then args+=(--firewall-id "$HEADMESH_DRILL_FIREWALL_ID"); fi
python3 deploy/ha/restore_drill.py run "${args[@]}"
```

Success returns JSON with `server_deleted: true`, `rto_seconds` and
`recovery_point_age_seconds`. When GitHub Actions is ready, use the same
contents in the `recovery` environment. See the credential mapping in the
[Hetzner restore runbook](README.md#hetzner-restore-drill).
[GitHub recovery environment settings](https://github.com/headmesh-dev/headmesh/settings/environments) · [GitHub environment secrets](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/use-secrets)
