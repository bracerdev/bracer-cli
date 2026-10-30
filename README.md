# Bracer

Encrypted Postgres backups that are proven to restore, run on your own server, written to your own bucket. Bracer never holds your credentials, your keys or your data.

- `bracer init <app>` finds Postgres on the machine (a Docker container, or Postgres installed on the machine through its Unix socket) or takes a connection URL, checks permissions, asks for the bucket and the schedule, proves the setup with a real backup that it test-restores, and installs the nightly timer.
- Every night `backup` runs `pg_dump` per database, encrypts the stream with [age](https://age-encryption.org), uploads it to any S3-compatible bucket, and ends the run with a `run.json` holding checksums, table row counts, encoding/locale and extensions. For a container or a local Postgres it also saves the users, their privileges and an environment record (version, settings, preload libraries, image or packages). Nothing touches the disk.
- The same run then restores what it just wrote into a throwaway Postgres built like the source, with no network, the same way `restore` would on a real target. It compares tables and row counts with the run, undoes the restore and checks it got back to where it started. A run exits 0 only when its backup was restored. A backup that was never restored is a guess.
- `bracer restore` gets the data back. It starts from the bucket, compares the target with the backup, writes the SQL that makes the target match, and loads the data into new databases only once it does and your app's URL connects. It never changes users, settings or existing databases on its own.

One static binary, no agent, no daemon. Linux with systemd, Postgres 12 to 18.

On its own, the CLI reports to your bucket and nothing else. With a Bracer account ([app.bracer.dev](https://app.bracer.dev)), `bracer init <app> --web` picks the Postgres in the browser and every run reports to your dashboard, which tells you when a backup stops coming.

Two moments need you: `init` and `restore`. Everything else runs unattended and reports to the bucket.

## What Bracer promises

Bracer only promises what it can check.

| Source in `app.yaml` | What a backup holds | What restore brings back |
|---|---|---|
| `container: <name>`: Postgres in a Docker container on this machine | Every database, the users and privileges, the environment | The app as it was: data, owners, privileges, users with their passwords, database settings |
| `local: {port: 5432}`: Postgres installed on this machine (apt, PGDG), reached through its socket as `postgres` | The same as a container | The same as a container |
| `url_env: NAME`: any other Postgres through a connection string (managed, another machine) | The databases only | The data, owned by the user that connects |

A container or local source is always backed up as its superuser: `init` finds it (`POSTGRES_USER`, `postgres` for Bitnami and local clusters) and never asks for a user. If that user stops being a superuser, `backup` dumps nothing and fails with a `Fix:` line, so every backup it does keep is complete. Restoring an app from a container or local backup needs a superuser on the target. A backup from a URL never has users or privileges, even when the URL's user is a superuser: to get a full backup, run Bracer on the machine that runs Postgres.

## Requirements

- Linux with systemd, on the machine where Postgres runs. For a URL source, any such machine that can reach it.
- `sudo` for the user who runs Bracer: `init` installs the systemd timer.
- Docker, usable by that user. Every Postgres tool runs in a container of the same major as the server: `docker exec` into the source container, otherwise `docker run postgres:<major>`. The test-restore runs its temporary Postgres as a container.
- An S3-compatible bucket: AWS S3, Cloudflare R2, Backblaze B2, MinIO, Garage, SeaweedFS, and others. Create a dedicated access key for it.

`init` checks all of this first and stops with a `Fix:` line when something is missing.

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

### 1. Create the app

Run `init` on the server, in a terminal, as the user that will run the backups:

```bash
bracer init shop
```

In order, `init`:

1. checks the machine (Docker, systemd) and asks for your sudo password once;
2. asks for the bucket (endpoint, bucket, region, access key, secret key) and checks it can read and write there. Every app gets its own folder under `bracer/` in the bucket; one bucket holds as many apps as you like;
3. creates an age key pair in `age.key`, or takes a public key whose private key you put in `age.key`;
4. lists the Postgres containers and local clusters it can see, or takes an address you type (always used as a URL source), checks the user, and lets you pick databases;
5. asks whether to test-restore on this machine every night, the test-restore's memory limit, the schedule (daily, weekly or monthly, at which times, on which days, and how many days, weeks or months to keep every backup of), and an optional heartbeat URL (see [Know when it stops](#know-when-it-stops));
6. writes a draft, checks it the way the nightly job will run it (`backup --dry-run`), then runs a real backup, test-restored. With the test-restore on, it stops if Docker's disk has less than 5 × the size of the databases free;
7. only then replaces `app.yaml`, installs the systemd timer (the unit files are written through `sudo`), and asks you to copy `age.key` off the machine.

With a Bracer account, `bracer init shop --web` runs the same steps but lets you pick the Postgres and the databases in the browser: it prints a link and an 8-character code, you enter the code there, and the terminal keeps the bucket, the keys, the schedule and the final `y`. The app then reports every backup to your dashboard; nothing else changes.

Nothing is written until the check passes. Every file of the app goes to `~/.config/bracer/shop/`, and every command takes the app name. `init` on an app that already exists stops: `bracer remove shop`, then `bracer init shop`.

Bracer backs up exactly the databases you pick, written out by name. A database created on the server later is not backed up until you set the app up again (`bracer remove shop`, then `bracer init shop`); Bracer does not watch for new ones.

What `init` writes:

| File | Contents | Mode |
|---|---|---|
| `~/.config/bracer/<app>/` | the app directory | 700 |
| `app.yaml` | instances, databases, bucket, public key, paths, the schedule, how the job runs. No secrets. | 644 |
| `age.key` | the age private keys. Copy it off the machine: without it the backups cannot be read. | 600 |
| `secrets.env` | bucket access key and secret, a Postgres password if one is needed, the heartbeat URL if you gave one | 600 |

`init` never prints the private key. Secrets are referenced by variable name in `app.yaml` and read only from `secrets.env`; the environment of the shell is never read, so a run by hand and a run by the timer do the same thing.

A finished `app.yaml` looks like this:

```yaml
app: shop
secrets_file: /home/me/.config/bracer/shop/secrets.env
instances:
  - name: db
    connection:                   # how bracer reaches the instance
      container: shop-postgres    # or local: {port: 5432}, or url_env: NAME
      user: postgres
      pg_major: 17
      system_id: "7421339870293418123"
    databases:                    # what you chose to back up
      - name: shop
      - name: auth
storage:
  endpoint: https://<account>.r2.cloudflarestorage.com
  bucket: shop-backups
  folder: shop-7f3k2q             # this app's own folder: bracer/shop-7f3k2q/ in the bucket
  region: auto
  access_key_env: BRACER_SHOP_S3_ACCESS_KEY
  secret_key_env: BRACER_SHOP_S3_SECRET_KEY
  path_style: true                # MinIO, Garage, R2
encryption: age                   # none | age
age_recipient: age1...
age_identity_file: /home/me/.config/bracer/shop/age.key
heartbeat_url_env: BRACER_SHOP_HEARTBEAT_URL   # optional
drill:                            # the test-restore at the end of every backup
  memory: 2g
schedule:
  every: daily                    # daily | weekly | monthly
  at: ["03:00"]                   # one or more HH:MM on this machine
  days: []                        # weekly: [mon, thu]; monthly: [1, 15] or [last]
  keep: 7                         # keep every backup of the last 7 days (weeks, months)
runtime:                          # what init ran with; the timer uses the same
  user: me
  path: /usr/local/bin:/usr/bin:/bin
  docker: /usr/bin/docker
```

Only `init` writes it, and it is read strictly: an unknown field is an error.

### 2. Change it

There is no `edit`. To change anything about an app (its databases, schedule, heartbeat, keys, encryption), remove it and set it up again:

```bash
bracer remove shop
bracer init shop
```

`remove` keeps `age.key`, so the new `init` encrypts with the same key and the old backups stay readable from this machine. The new app gets a new folder in the bucket; the old folder is left as it is, `bracer restore` still lists it, and you delete it at your provider when you no longer need it. Every message that asks you to change something says exactly this: `Fix: bracer remove shop, then bracer init shop`.

### 3. Every night

The timer runs `bracer backup shop` as you, with the `PATH` and Docker `init` found. A run missed while the machine was off is made up at boot. Each run is one folder `<ts>/` in the bucket, ended by its `run.json`; the newest `run.json` is the state of the app:

| Exit | Result | Means |
|---|---|---|
| 0 | ok | every database dumped and uploaded, the test-restore passed. The run may still print warnings: a new cluster or major version, a database that lost more than half of its rows since the last run, something that depends on things outside Postgres. Each says what to do; none changes the result. |
| 1 | failed | a connection, dump or upload failed, or the test-restore of what was uploaded failed |

```bash
bracer status          # every app on this machine: where it stores, schedule, last run, next run
bracer status shop
journalctl -t bracer   # the logs
```

`status` reads the newest `run.json` from the bucket and the timer from systemd: `last run <time>: ok` or `failed` with the reasons, then the warnings of that run. A run that could not reach the bucket shows as "did not report to the bucket", with its exit code.

#### Know when it stops

A dead machine or a stopped timer reports nothing, so something off the machine has to notice. Give `init` a Healthchecks-style URL, such as a check on [healthchecks.io](https://healthchecks.io) or your own Healthchecks:

- it gets **one signal per backup run** (by the timer or by hand): `<url>/start` when the run begins, `<url>/<exit code>` when it ends, the test-restore included. A run that passed with warnings sends them as the body of `<url>/0`, so Healthchecks keeps them in the check's log without alerting; nothing else is sent;
- `init` checks the URL with `<url>/log`, which records an event without changing the check;
- the service behind the URL does the watching: set its schedule to what `init` prints (a cron expression when one says it), this machine's time zone, and a grace time longer than the backup and its test-restore take. Then it tells you when a signal fails, or does not come at all;
- a failed send only shows in the journal; the job's result does not change. The URL stays in `secrets.env` and is never printed or logged.

What went wrong is in `bracer status shop` and `journalctl -t bracer`.

Retention follows the schedule: every backup of the last N days, weeks or months that have one stays (7 days, 4 weeks or 3 months by default, counted in this machine's time zone), whatever else ran in them; everything older goes, as do runs that dumped nothing (Postgres was down) and runs that never finished. Only a run that passed deletes anything, so the newest good backup is never removed, however many nights fail after it. Running more often, or by hand, adds backups without shortening the history. When a database has less than half the rows it had in the last run that dumped it, the run warns and names that run, so you can restore from it while retention keeps it.

By hand:

```bash
bracer backup shop --dry-run   # connections, permissions, bucket, a schema-only test-restore: dumps nothing
bracer backup shop
```

Bucket layout under `bracer/<folder>/<ts>/`: `<instance>/<db>.dump.age` per database, `<instance>/globals.sql.age` (users, memberships, role settings; container and local only), `<instance>/env.json.age` (the environment record), `app.yaml.age` (the `app.yaml` of that run) and `run.json`, written last. A folder without `run.json` is a run that did not end. The folder belongs to one machine and one app directory: a copy of the app directory elsewhere refuses to write to it.

The test-restore's temporary Postgres is built like the source:

- **Container source:** the same image, by ID. It must be the official `postgres` image or one built on it.
- **Local source:** an image built from `postgres:<major>` with the extension packages and locales of your cluster, rebuilt when the packages change. The first build needs network access and a Debian or Ubuntu host.
- **URL source:** the official image that carries the source's extensions (PostGIS, TimescaleDB, pgvector), or `postgres:<major>-alpine`.

When the test-restore of an instance fails at `init`, you can give another image or turn it off for that instance.

### 4. Get data back

```bash
bracer restore
```

Restore always starts from the bucket, on this machine or a new one, so every restore walks the path you would take after losing the server. Have `age.key`, the bucket's access keys and your app's stack (only Postgres needs to run). Then, in one session:

1. Enter the bucket. Restore lists the app folders in it: app, the machine that writes it, the newest run and its result. Pick one, then the path of `age.key`.
2. Pick the instance and the run: a numbered list with time, rows, size, whether its test-restore passed. Restore never picks one for you.
3. Pick the target: a container or local Postgres it finds, or `[u]` for a URL.
4. Restore compares the target with the backup: version, extensions, preload libraries, locales, settings that change how SQL behaves, users, privileges, disk space. If anything differs, it writes `env-up.sql` and `env-down.sql` into `./bracer-restore-<app>-<instance>-<ts>/` with the command to apply each, prints what SQL cannot fix (an extension package, a locale), and waits: press Enter to check again.
5. Enter your app's database URL, as your app uses it. It is not echoed.
6. Confirm. Each database `<db>` is loaded into a new database `<db>_restored_<YYYYMMDD_HHMM>` with `pg_restore --single-transaction`; your existing databases are never touched. Restore then checks tables and row counts against the run and connects with your app's URL to read what the app can read.

At the end it prints how to switch your app over (rename the databases, or point the app at the new name) and how to undo (`DROP DATABASE`, then `env-down.sql`). Users that already exist on the target keep the target's password; the app check shows whether the app still gets in. The `env-up.sql` of a full backup holds password hashes: it is written with mode 600; delete the directory when you are done.

To look at a dump yourself, download it with your provider's tools, then `age -d -i age.key <file>.dump.age | pg_restore -l`.

### 5. Rotate the key

```bash
bracer rotate-key shop
```

A new key pair is appended to `age.key`, and `app.yaml` points at the new public key. Old backups stay readable with the old key, which stays in the file. Nothing in the bucket is touched. It runs in a terminal only, and asks you to copy `age.key` off the machine again. It refuses when `age.key` is missing or does not hold the current key.

### 6. Remove an app from this machine

```bash
bracer remove shop
```

Removes the timer (through `sudo`), then `app.yaml` and `secrets.env`. `age.key` stays in the app directory, and `remove` prints where: it opens the backups in the bucket, so delete it yourself when you no longer need them. The bucket is not touched: the backups stay, `bracer restore` still opens them with that key, and you delete the folder at your provider when you no longer need it.

## Read this before trusting it

- **Whoever can write to the bucket can run SQL on the target when you restore.** Dumps are not signed yet. Use a dedicated access key per server, turn on bucket versioning and object lock where the provider offers it, and before restoring from a bucket you do not fully trust, look at the dump with `pg_restore -l`. Restore changes nothing on the target until you confirm.
- **A full backup reads the password hashes of every Postgres user**, because `pg_dumpall --globals-only` needs them to bring users back. A local source is backed up as `postgres`, with the same rights as `sudo -u postgres`. Anyone in the `docker` group is equivalent to root on the machine anyway.
- **Losing `age.key` means losing the backups.** It is the one thing that cannot be recreated: the bucket's keys can be made again at your provider. Keep a copy off the server, and copy it again after `rotate-key`. Encryption protects the bucket, not someone who is already on the machine.
- **Backup never changes your database.** It reads through `pg_dump` with a lock timeout. It does not install anything in Postgres, create extensions, or change settings.
- `pg_dump` is logical and consistent per database, not point-in-time across databases. If you need PITR, use WAL archiving in addition.

## Supported and unsupported

Supported:

| Setup | Notes |
|---|---|
| PostgreSQL 12 to 18 | Tools of the same major as the server; `run.json` records tool version and locale |
| Docker container: official `postgres` and images built on it (`postgis/postgis`, `timescale/timescaledb`, `pgvector/pgvector`, custom `FROM postgres`), Bitnami | `docker exec`; Bitnami asks for a password even on its socket: `init` tries `POSTGRESQL_POSTGRES_PASSWORD` (or its `_FILE`) first and keeps the password that works in `secrets.env`. Without a password for `postgres` a Bitnami container is not supported |
| Postgres installed on the machine (apt, PGDG, Laravel Forge, Ploi…) | `local:` through the socket in `/var/run/postgresql`, peer authentication as `postgres` |
| Managed or remote Postgres through a connection string (`url_env`) | Databases only; restored as data owned by the connecting user |
| Contrib extensions (`uuid-ossp`, `pgcrypto`, `pg_trgm`, …) | Every official image has them |
| Extensions of the running image, or installed from packages on the machine | The test-restore uses that image, or builds one with those packages |
| URL source with PostGIS, TimescaleDB or pgvector | The test-restore picks the matching official image |
| Any encoding and locale (libc, ICU, builtin) | The target must have the locale; restore checks it first |

Refused up front rather than failing at restore time:

| Setup | Why |
|---|---|
| Kubernetes and k3s in any form | Too many moving parts whose failures only show at run time; run Postgres in Docker, or back it up through a URL to its Service |
| A machine without Docker or without systemd | Every Postgres tool and the test-restore run in Docker; the nightly job is a systemd timer |
| A managed Postgres without superuser (RDS, Supabase, Cloud SQL, Neon) as the target of a full backup | It cannot create the users and privileges, so the app is not guaranteed to work. A URL backup restores there |
| A Bitnami image, or any image not built on the official one, as the test-restore image | Its entrypoint cannot be driven like the official one |
| PgBouncer in transaction mode as the dump or restore target | Dump and restore need a direct connection |
| Functions in untrusted languages (`plpython3u`, `plperlu`), C functions outside `$libdir`, `file_fdw`, subscriptions | They depend on files or processes outside Postgres; `init` asks you to accept them, restore says it cannot guarantee them |
| URL source with extensions beyond contrib and the three above (`pg_cron`, `pg_partman`, self-compiled) | No image can be picked automatically; give one when `init` asks, or turn the test-restore off for that instance |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success (warnings, if any, are printed and do not change it) |
| 1 | the run failed |
| 2 | usage or config error |

`--json` prints the full result of `backup` on stdout; logs go to stderr. No command ever prints a connection string, a password, a private key or dump contents.

## License

Apache License 2.0. See [LICENSE](LICENSE).
