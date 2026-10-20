# Bracer

Encrypted Postgres backups that are proven to restore, run on your own server, written to your own bucket. Bracer never holds your credentials, your keys or your data.

- `bracer init` finds Postgres on the machine (a Docker container, or Postgres installed on the machine through its Unix socket) or takes a connection URL, checks permissions, creates a key pair and writes `app.yaml`.
- `bracer backup` runs `pg_dump` per database, encrypts the stream with [age](https://age-encryption.org), uploads it to any S3-compatible bucket, and writes a manifest with checksums, table row counts, encoding/locale and extensions. For a container or a local Postgres it also saves the users, their privileges and an environment record (version, settings, preload libraries, image or packages). Nothing touches the disk.
- `bracer drill` restores the latest backup into a throwaway Postgres built like the source, with no network, the same way `restore` would on a real target. It compares tables and row counts with the manifest, runs your SQL checks, undoes the restore and checks it got back to where it started. A backup that was never restored is a guess.
- `bracer fetch` and `bracer restore` get the data back. Restore compares the target with the backup, writes the SQL that makes the target match, and loads the data only once it does. It never changes users, settings or existing databases on its own.

One static binary, no agent, no daemon. Linux, Postgres 12 to 18.

## What Bracer promises

Bracer only promises what it can check.

| Source in `app.yaml` | What a backup holds | What restore brings back |
|---|---|---|
| `container: <name>`: Postgres in a Docker container on this machine | Every database, the users and privileges, the environment | The app as it was: data, owners, privileges, users with their passwords, database settings |
| `local: {port: 5432}`: Postgres installed on this machine (apt, PGDG), reached through its socket as `postgres` | The same as a container | The same as a container |
| `url_env: NAME`: any other Postgres through a connection string (managed, another machine) | The databases only | The data, owned by the user that connects |

Restoring an app from a container or local backup needs a superuser on the target. A backup from a URL never has users or privileges, even when the URL's user is a superuser: to get a full backup, run Bracer on the machine that runs Postgres.

## Requirements

- Linux, on the machine where Postgres runs. For a URL source, any Linux machine that can reach it.
- Docker, usable by the user who runs Bracer. Every Postgres tool runs in a container of the same major as the server: `docker exec` into the source container, otherwise `docker run postgres:<major>`. `drill` runs its temporary Postgres as a container. `bracer check` tells you what is missing.
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

Run `init` on the server, as the user that will run the backups. It checks Docker, lists the Postgres containers and local clusters it can see, lets you pick databases, checks that the chosen user can read them, asks for the bucket, generates an age key pair and writes the config:

```bash
mkdir -p ~/bracer && cd ~/bracer
bracer init
```

If you type a URL to `127.0.0.1` and a local cluster listens on that port, `init` offers to back it up through its socket instead, so the backup is full.

What `init` writes:

| File | Contents | Mode |
|---|---|---|
| `app.yaml` | instances, databases, bucket, public key, paths. No secrets. | 644 |
| `~/.config/bracer/<app>.key` | the age private key. Back it up somewhere safe: without it the backups cannot be read. | 600 |
| `~/.config/bracer/<app>.env` | bucket access key and secret, and a Postgres password if one is needed | 600 |

The private key is shown once on the terminal and never logged; `init` waits until you type `copied`. Secrets are only ever referenced by environment variable name in `app.yaml`; at run time they come from the environment, from `--env-file`, or from `secrets_file`, in that order.

A finished `app.yaml` looks like this:

```yaml
app: shop
secrets_file: ~/.config/bracer/shop.env
instances:
  - name: db
    container: shop-postgres      # or local: {port: 5432}, or url_env: NAME
    user: app
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

Every run has one timestamp for the whole instance. Bucket layout under `<prefix>/<app>/<instance>/`: `<db>/<ts>.dump.age` per database, `globals/<ts>.sql.age` (users, memberships, role settings, from `pg_dumpall --globals-only`; container and local only), `env/<ts>.json.age` (the environment record), and `<prefix>/<app>/manifest.json`.

`backup` takes a lock per `app.yaml`, waits at most one minute for table locks (`backup.lock_timeout`), and gives up on a database after six hours (`backup.timeout`). A failed database does not stop the others. Dumps from this run are never deleted by retention, and retention only runs after the manifest has been written.

### 3. Prove it restores

```bash
bracer drill                # latest backup of every database, in a temporary Postgres
bracer drill --keep         # keep the container after loading, to look inside (it holds your real data)
```

The temporary Postgres is built like the source:

- **Container source:** the same image, by ID. It must be the official `postgres` image or one built on it.
- **Local source:** an image `bracer-drill/<app>:<hash>` built from `postgres:<major>` with the extension packages and locales of your cluster. It is built once and reused; the first build needs network access and a Debian or Ubuntu host.
- **URL source:** the official image that carries the source's extensions (PostGIS, TimescaleDB, pgvector), or `postgres:<major>-alpine`.

`drill.image` overrides the choice. The container runs with `--network none`, as the source's bootstrap superuser. `drill` then does what a restore does: it compares the temporary Postgres with the backup, applies the generated environment SQL, loads every database in one transaction, and checks tables, row counts and your SQL checks. Then it applies the generated undo SQL and checks the server is back to its state before the restore. Settings that only the real target has (`ssl`, `pg_hba.conf`) are noted, not failed; `restore` checks them on the real target.

Output, one line per database:

```
✓ db/shop: restored 109.8 KB in 4.0 s, 19/19 tables, checks 1/1
```

### 4. Schedule it

```bash
sudo bracer schedule install        # shows the units, asks, then writes and starts them
bracer schedule status              # last run, result, next run
```

`install` writes `bracer-<app>-backup` and `bracer-<app>-drill` service and timer units to `/etc/systemd/system`, runs them as the user who ran `sudo` (so they read your key file), with a `PATH` that includes `docker`, and starts the timers. The default schedule is `backup` at 03:00 and `drill` at 04:00 in the machine's time zone (`--backup-at 02:30`, `--drill-at "0 4 * * 1"` to change it). A run missed while the machine was off is made up at boot (`Persistent=true`). Logs: `journalctl -t bracer`. `sudo bracer schedule remove` stops and deletes the units.

`drill` can run on another machine than `backup`: it needs Docker, the bucket, the key file and `app.yaml`, not the database.

Without systemd, print a crontab and paste it yourself:

```bash
bracer schedule print               # crontab, paste into crontab -e
bracer schedule print --format systemd
```

### 5. Get data back

```bash
bracer fetch --database shop                      # decrypted .dump file, sha256 checked against the manifest
bracer fetch --database shop --ts 2026-10-17T030000Z -o - | pg_restore -l   # inspect the table of contents
bracer restore --into-container new-db --dry-run  # compare only: write and change nothing
bracer restore --into-container new-db            # every database of the latest run
bracer restore --into-local 5432 --app-url        # then check the app connects
bracer restore --into-url-env TARGET_URL --preview   # data to look at only
```

The target is given each time and never stored: `--into-container <name>`, `--into-local <port>`, `--into-url` (asked for, not echoed) or `--into-url-env NAME`. Set up the target first (a new compose stack, a fresh Postgres), then restore into it:

1. Restore compares the target with the backup: version, extensions, preload libraries, locales, settings that change how SQL behaves, users, privileges and databases.
2. If anything differs, it stops and writes `env-up.sql` (users, settings and databases to create) and `env-down.sql` (to undo it) into `./bracer-restore-<app>-<instance>-<ts>/`, with the command to apply each and whether Postgres needs a restart. What SQL cannot fix (a missing extension package, a locale, an image) is printed with the fix. You read the files and apply them yourself.
3. Run the same restore again. When the target matches, each database is loaded with `pg_restore --single-transaction`: if it fails, the database is left empty as before.
4. Restore then proves the result: it compares the target again, checks tables and row counts against the manifest, runs `drill.checks`, and with `--app-url` connects as your app and reads the tables it has `SELECT` on. It writes `data-down.sql` to remove what it loaded.

A database name that already holds data is restored next to it as `<db>_restored_<time>`, with the statements to swap them. `--preview` needs only CREATEDB: data without owners, privileges or users, into `<db>_preview_<time>`. Backups made before Bracer recorded the environment can only be previewed. The env-up file of a full backup holds password hashes: it is written with mode 600; delete the directory when you are done.

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
- **A full backup reads the password hashes of every Postgres user**, because `pg_dumpall --globals-only` needs them to bring users back. A local source is backed up as `postgres`, with the same rights as `sudo -u postgres`. Anyone in the `docker` group is equivalent to root on the machine anyway.
- **Losing the key file means losing the backups.** Keep a copy of `~/.config/bracer/<app>.key` outside the server. Encryption protects the bucket, not someone who is already on the machine.
- **Backup never changes your database.** It reads through `pg_dump` with a lock timeout. It does not install anything in Postgres, create extensions, or change settings.
- `pg_dump` is logical and consistent per database, not point-in-time across databases. If you need PITR, use WAL archiving in addition.

## Supported and unsupported

Supported:

| Setup | Notes |
|---|---|
| PostgreSQL 12 to 18 | Tools of the same major as the server; the manifest records tool version and locale |
| Docker container: official `postgres` and images built on it (`postgis/postgis`, `timescale/timescaledb`, `pgvector/pgvector`, custom `FROM postgres`), Bitnami | `docker exec`; Bitnami needs `password_env` (its socket asks for a password) |
| Postgres installed on the machine (apt, PGDG, Laravel Forge, Ploi…) | `local:` through the socket in `/var/run/postgresql` (or `socket_dir`), peer authentication as `postgres` |
| Managed or remote Postgres through a connection string (`url_env`) | Databases only; restored in data mode |
| Contrib extensions (`uuid-ossp`, `pgcrypto`, `pg_trgm`, …) | Every official image has them |
| Extensions of the running image, or installed from packages on the machine | `drill` uses that image, or builds one with those packages |
| URL source with PostGIS, TimescaleDB or pgvector | `drill` picks the matching official image |
| Any encoding and locale (libc, ICU, builtin) | The target must have the locale; restore checks it first |

Refused up front rather than failing at restore time:

| Setup | Why |
|---|---|
| Kubernetes and k3s in any form | Too many moving parts whose failures only show at run time; run Postgres in Docker, or back it up through a URL to its Service |
| A machine without Docker | Every Postgres tool and the drill run in Docker |
| A managed Postgres without superuser (RDS, Supabase, Cloud SQL, Neon) as the target of a full backup | It cannot create the users and privileges, so the app is not guaranteed to work. A URL backup restores there in data mode; `--preview` always works |
| A Bitnami image, or any image not built on the official one, as the drill image | Its entrypoint cannot be driven like the official one |
| PgBouncer in transaction mode as the dump or restore target | Dump and restore need a direct connection |
| Functions in untrusted languages (`plpython3u`, `plperlu`), C functions outside `$libdir`, `file_fdw`, subscriptions | They depend on files or processes outside Postgres; backup warns, restore says it cannot guarantee them |
| URL source with extensions beyond contrib and the three above (`pg_cron`, `pg_partman`, self-compiled) | No image can be picked automatically; set `drill.image`, and `drill` stops before loading if an extension is missing |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | the run failed (a database failed to dump, a drill check failed, the bucket was unreachable) |
| 2 | usage or config error |

`--json` prints the full result of `backup`, `drill`, `restore` and `rotate-key` on stdout for scripting; logs go to stderr. `schedule status` exits 1 when the last run of a job failed or nothing is installed. No command ever prints a connection string, a password, a private key or dump contents.

## License

Apache License 2.0. See [LICENSE](LICENSE).
