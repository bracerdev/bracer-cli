# Bracer

Encrypted Postgres backups that are proven to restore, run on your own server, written to your own bucket. Bracer never holds your credentials, your keys or your data.

- `bracer init` finds Postgres on the machine (Docker container, Kubernetes pod, host package, or a connection URL), checks permissions, creates a key pair and writes `app.yaml`.
- `bracer backup` runs `pg_dump` per database, encrypts the stream with [age](https://age-encryption.org), uploads it to any S3-compatible bucket, and writes a plaintext manifest with checksums, table row counts, encoding/locale and extensions. Nothing touches the disk.
- `bracer drill` restores the latest backup into a throwaway Postgres container with no network, compares tables and row counts with the manifest, runs your SQL checks, and removes the container. A backup that was never restored is a guess.
- `bracer fetch` and `bracer restore` get the data back, into a file or straight into a database that does not exist yet. Restore never drops or alters anything that already exists.

One static binary, no agent, no daemon. Postgres 12 to 18.

## Requirements

- Linux or macOS on the machine where Postgres runs (the backup machine). For a managed or remote Postgres, any machine that can reach it.
- Postgres tools of the same major version as the server. Bracer uses what already runs Postgres: `docker exec` for a container, `kubectl exec` for a pod, the host `pg_dump` for a host install. If none fits, it runs the official `postgres:N` image through Docker.
- Docker, for `drill` only. `drill` can run on another machine than `backup`: it needs the bucket, the key file and `app.yaml`, not the database.
- An S3-compatible bucket: AWS S3, Cloudflare R2, Backblaze B2, MinIO, Garage, SeaweedFS, and others. Create a dedicated access key for it.

## Install

Download the archive for your platform from the [releases page](https://github.com/bracerdev/bracer-cli/releases). Verify it against `checksums.txt`, then put the binary on your `PATH`:

```bash
tar xzf bracer_*_linux_amd64.tar.gz
sudo install -m 755 bracer /usr/local/bin/bracer
bracer version
```

Or build from source with Go 1.27 or newer:

```bash
git clone https://github.com/bracerdev/bracer-cli
cd bracer-cli
go build -o /usr/local/bin/bracer .
```

## Quickstart

### 1. Create `app.yaml`

Run `init` on the server, as the user that will run the backups. It lists the Postgres servers it can see, lets you pick databases, checks that the chosen user can read them, asks for the bucket, generates an age key pair and writes the config:

```bash
mkdir -p ~/bracer && cd ~/bracer
bracer init
```

What `init` writes:

| File | Contents | Mode |
|---|---|---|
| `app.yaml` | instances, databases, bucket, public key, paths. No secrets. | 644 |
| `~/.config/bracer/<app>.key` | the age private key. Back it up somewhere safe: without it the backups cannot be read. | 600 |
| `~/.config/bracer/<app>.env` | bucket access key and secret, and a Postgres password if one is needed | 600 |

The private key is shown once on the terminal and never logged. Secrets are only ever referenced by environment variable name in `app.yaml`; at run time they come from the environment, from `--env-file`, or from `secrets_file`, in that order.

A finished `app.yaml` looks like this:

```yaml
app: shop
secrets_file: ~/.config/bracer/shop.env
instances:
  - name: db
    container: shop-postgres      # or pod: ns/pod[/container], socket: /var/run/postgresql, host: + port:, url_env: NAME
    user: bracer_backup
    pg_major: 17
    databases: all                # or a list; each entry may set its own retention
storage:
  endpoint: https://<account>.r2.cloudflarestorage.com
  bucket: shop-backups
  prefix: bracer
  region: auto
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
  path_style: true                # MinIO, Garage, R2
encryption: age                   # none | age
age_recipient: age1...
age_identity_file: ~/.config/bracer/shop.key
retention:
  keep: 7                         # dumps kept per database (default 7)
drill:
  image: postgres:17-alpine       # set by init when the source is a container or pod; optional otherwise
  checks:
    - name: orders exist
      sql: select count(*) > 0 from orders
      databases: [shop]
```

### 2. Check, then back up

```bash
bracer backup --dry-run     # connections, permissions, bucket: dumps nothing
bracer backup               # dump, encrypt, upload, write manifest, apply retention
bracer fetch --list         # what is in the bucket
```

Bucket layout: `<prefix>/<app>/<instance>/<db>/<ts>.dump.age`, `<prefix>/<app>/<instance>/globals/<ts>.sql.age` (users and their settings, from `pg_dumpall --globals-only`) and `<prefix>/<app>/manifest.json`.

`backup` takes a lock per `app.yaml`, waits at most one minute for table locks (`backup.lock_timeout`), and gives up on a database after six hours (`backup.timeout`). A failed database does not stop the others. Dumps from this run are never deleted by retention, and retention only runs after the manifest has been written.

### 3. Prove it restores

```bash
bracer drill                # latest backup of every database, in a temporary container
bracer drill --keep         # keep the container to look inside (it holds your real data)
```

Output, one line per database:

```
✓ db/shop: restored 109.8 KB in 4.0 s, 19/19 tables, checks 1/1
```

Globals (users) are restored first and only reported when they fail.

The container runs with `--network none` and `--rm`, as the image that the source runs (container or pod), or the official image that carries the source's extensions (PostGIS, TimescaleDB, pgvector), or `postgres:N-alpine`. Before restoring, `drill` creates the database with the source's encoding and locale and verifies that every extension the database uses is available in the image. If one is missing it stops before restoring and tells you which `drill.image` to set.

### 4. Schedule it

```bash
bracer cron-example                 # paste into crontab -e
bracer cron-example --format systemd
```

The default schedule is `backup` at 03:00 and `drill` at 04:00, output to syslog under the tag `bracer`. `drill` needs Docker; if the backup machine has none, schedule `drill` on a machine that has it, with the same `app.yaml` and key file.

### 5. Get data back

```bash
bracer fetch --database shop                      # decrypted .dump file, sha256 checked against the manifest
bracer fetch --database shop --ts 2026-10-17T030000Z -o - | pg_restore -l   # inspect the table of contents
bracer restore --database shop --into db/shop_restored --dry-run
bracer restore --database shop --into db/shop_restored
bracer restore --database shop --into-url-env TARGET_URL --with-globals
```

`restore` creates the target database with the source's encoding and locale, checks that the target has the extensions the dump needs, and refuses a database name that already exists. `--with-globals` creates users that are missing on the target; existing users are never changed.

### 6. Rotate the key

```bash
bracer rotate-key
```

A new key pair is generated, `app.yaml` points at the new public key, and the new private key is appended to the key file. Old backups stay readable with the old key, which stays in the file. Nothing in the bucket is touched.

## Heartbeats for any job

`bracer ping` sends a heartbeat to the Bracer server so a missing or failing cron job raises an alert:

```bash
bracer ping <id>                        # the job ran
bracer ping <id> -- /path/to/job.sh     # wrap: sends start, then the exit code
```

Hosted alerting is not available yet; without `hosted:` in `app.yaml`, `backup` and `drill` run fully offline and only report on the terminal and the exit code.

## Read this before trusting it

- **Whoever can write to the bucket can run SQL on the target when you restore.** Dumps are not signed yet. Use a dedicated access key per server, turn on bucket versioning and object lock where the provider offers it, and before restoring from a bucket you do not fully trust, `fetch` the dump and look at `pg_restore -l`, or run `restore --dry-run`.
- **The backup user can read password hashes of every Postgres user**, because `pg_dumpall --globals-only` needs that. Use a dedicated user: `init` prints the SQL for one (`LOGIN`, `BYPASSRLS`, `GRANT pg_read_all_data`) and never runs SQL that changes your database. On Postgres 12 and 13 there is no `pg_read_all_data`, so the backup user must be a superuser.
- **Losing the key file means losing the backups.** Keep a copy of `~/.config/bracer/<app>.key` outside the server. Encryption protects the bucket, not someone who is already on the machine.
- **Backup never changes your database.** It reads through `pg_dump` with a lock timeout. It does not install anything in Postgres, create extensions, or change settings.
- `pg_dump` is logical and consistent per database, not point-in-time across databases. If you need PITR, use WAL archiving in addition.

## Supported and unsupported

Supported, and tested end to end (backup, drill, restore with intact data):

| Setup | Notes |
|---|---|
| PostgreSQL 12 to 18 | `pg_dump` of the same major as the server; the manifest records tool version and locale |
| Docker container: official `postgres`, images built on it (`postgis/postgis`, `timescale/timescaledb`, custom `FROM postgres`), Bitnami | Bitnami needs `password_env` |
| Kubernetes or k3s pod reached with `kubectl exec` without a password (CloudNativePG, Zalando, a StatefulSet on the official image) | |
| Host package: Unix socket in `/var/run/postgresql` or `/tmp`, or TCP | Needs the client package of the same major, or Docker |
| Managed or remote Postgres through a connection string (`url_env`) | |
| Contrib extensions (`uuid-ossp`, `pgcrypto`, `pg_trgm`, …) | Every official image has them |
| Extensions of the running image (container or pod): PostGIS, TimescaleDB, pgvector, custom builds | `init` uses that image for `drill` |
| Host Postgres with PostGIS, TimescaleDB or pgvector | `drill` picks the matching official image |
| Any encoding and locale (libc, ICU, builtin) | The target must have the locale; Alpine images accept any name |

Refused up front rather than failing at restore time:

| Setup | Why |
|---|---|
| Pod that requires a password over the socket (Bitnami chart) | `kubectl exec` cannot pass it without exposing it; use a URL through the Service |
| Socket outside `/var/run/postgresql` and `/tmp` | Rare; enable TCP on loopback or add the standard directory |
| Bitnami image as `drill.image` | Its entrypoint starts a temporary server on TCP that cannot be told apart from the real one |
| PgBouncer in transaction mode as the dump or restore target | Dump and restore need a direct connection |
| Supabase image for `drill` | Not known to run empty |
| Host Postgres with extensions beyond contrib and the three above (`pg_cron`, `pg_partman`, self-compiled) | No image can be chosen automatically; `init` asks for one, `drill` checks `pg_available_extensions` and stops before restoring |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | the run failed (a database failed to dump, a drill check failed, the bucket was unreachable) |
| 2 | usage or config error |

`--json` prints the full result of `backup`, `drill`, `restore` and `rotate-key` on stdout for scripting; logs go to stderr. No command ever prints a connection string, a password, a private key or dump contents.

## License

Apache License 2.0. See [LICENSE](LICENSE).
