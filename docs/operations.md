# Karta operations (Stage 4)

This is the operator's reference for running Karta beyond a development
host: what is deployed and who can reach what, how long a publication may
run, what is monitored and how to respond to each alert, backup and
restore, credential and key rotation, and how capacity is measured. The
commands themselves are also listed in the [runbook](runbook.md);
decisions are in [ADR 0005](adr/0005-stage4-operations.md).

**Status.** Everything here was exercised with the committed fixtures
(evidence tier A) and the supplied Chitgar extract (tier B). Neither tier
says anything about the selected target host, the full Iran snapshot or a
production update source (tier D), and **no production-readiness claim is
made**. The open owner inputs are listed at the end ("Owner inputs"); until
they are decided, every number below marked *provisional* is an
engineering default, not an objective.

| Tier | Where and with what | What it can establish |
| --- | --- | --- |
| A | CI fixtures, the local controlled HTTPS source, throwaway keys and tokens | correctness, failure handling, alert logic, restore and rotation mechanics |
| B | the supplied Chitgar extract in the implementation environment | the real-data path and small-scale resource figures |
| C | optional, owner's choice: the owner's VM under an approved plan | procedures on a real host |
| D | the owner-selected Iran snapshot, host and objectives | capacity, limits, margins, publication, recovery time and data loss |

A result never counts toward a higher tier.

## Deployment and trust inventory

### Topology

One Docker host runs one Compose project (`compose.yaml`). Services,
images, users and limits:

| Service | Image (pinned) | User | Networks | Published port (default bind 127.0.0.1) | Limits (memory / CPU / PIDs) |
| --- | --- | --- | --- | --- | --- |
| `db` | `postgis/postgis:18-3.6@sha256:60f6ad1d…` | `postgres` | `backend` (internal) | none | 2 GiB / 2 / 512 |
| `api` | `karta-api` (built locally, distroless static) | 65532 | `backend`, `frontend` | 8080 (public API), 9464 (metrics) | 512 MiB / 1 / 256 |
| `publisher` | `karta-importer` (Ubuntu 24.04 + osm2pgsql 1.11.0) | 10001, `docker-init` as PID 1 | `backend`, `operator` (no NAT) | 8081 (operator API) | 4 GiB / 2 / 256 |
| `fetcher` (opt-in, profile `online`) | `karta-api` | 65532 | `egress` only | none | 256 MiB / 0.5 / 64 |
| `importer` (one-off) | `karta-importer` | 10001, `docker-init` | `backend` | none | 4 GiB / – / 256 |
| `operator-cli` (one-off) | `karta-api` | 65532 | `operator` | none | – |
| `postgres-exporter` (opt-in, profile `monitoring`) | `prometheuscommunity/postgres-exporter:v0.20.1@sha256:ac5ec343…` | nobody | `backend`, `monitoring` (no NAT) | 9187 | 128 MiB / 0.25 / 32 |
| `prometheus` (opt-in, profile `monitoring`) | `prom/prometheus:v3.15.0@sha256:efd719c9…` | nobody | `backend`, `monitoring` (no NAT) | 9090 | 512 MiB / 0.5 / 64 |

Every container except `db` runs with a read-only root filesystem and
drops every capability (the database image keeps the ones its entrypoint
needs to switch to the `postgres` user); all of them set
`no-new-privileges`. Images not built here are
pinned by digest; the two Karta images are built from this repository with
their base images pinned by digest (`deploy/Dockerfile`).

### Interfaces, TLS and egress

| Interface | Listener | Who may reach it | Protection |
| --- | --- | --- | --- |
| Public API and demo | api :8080 | clients | read-only methods, strict input validation; put a TLS reverse proxy (with rate limits) in front of any non-loopback exposure (`KARTA_HTTP_BIND`) |
| API metrics | api :9464, a separate listener (`KARTA_METRICS_LISTEN_ADDR`) | monitoring only | bearer credential with scope `status` from `secrets/metrics_tokens` (hashes only); never on the public listener; TLS proxy for non-loopback exposure (`KARTA_METRICS_BIND`) |
| Operator API, including `/v1/operator/metrics` | publisher :8081 | operators (all scopes), monitoring (`status`) | scoped bearer tokens; TLS proxy with access control for non-loopback exposure (`KARTA_OPERATOR_BIND`) |
| PostgreSQL exporter | :9187 | monitoring | statistics only (role `karta_monitor`), loopback by default; TLS proxy if exposed |
| Prometheus | :9090 | operators | no authentication of its own: keep it on loopback or behind an authenticating proxy |
| PostgreSQL | none | the stack's containers on `backend` | SCRAM-SHA-256; no published port |

Only the fetcher has a route out of the host (network `egress`), and only
when online updates are configured. The `operator` and `monitoring`
networks are bridges without IP masquerading, and `backend` is internal.
The publisher, which parses untrusted snapshots, therefore has no NAT route
out. The exporter and Prometheus have none either. Losing internet
connectivity affects only the fetcher. Serving, inbox delivery and
command-line import continue (tested: `TestOnline`, "a broken transfer
resumes; network loss backs off; manual publication keeps working";
`make test-offline`).

### Volumes, host paths and permissions

| Data | Location | Writers | Readers |
| --- | --- | --- | --- |
| PostgreSQL cluster (registry, release databases, roles) | volume `pgdata` | `db` | `db` |
| staging copies of snapshots | volume `staging` (0700) | publisher | publisher |
| fetcher outbox (verified deliveries, fetcher state) | volume `online` | fetcher | publisher (read-only) |
| inbox | `./data/inbox` (`KARTA_INBOX_HOST_DIR`) | **any host identity that can write the directory** | publisher (read-only) |
| region and source configuration | `./config/regions`, `./config/sources` | host identities with write access to the checkout | publisher, importer, fetcher (read-only) |
| secrets | `./secrets` (0700, files 0644) | the host user that ran `make secrets` | each container gets only the secrets it needs (below) |
| Prometheus data | volume `prometheus` | prometheus | prometheus |
| backups | `BACKUP_DEST` (default `./backups`, 0700, files 0600) | the identity running `scripts/backup.sh` | the restoring identity |

Writing the inbox submits a file but authorizes nothing: only a digest
pinned in the region file or authorized through the operator API is
imported. Writing `config/regions` *does* change what is authorized (the
pinned digest), and writing `config/sources` changes which signing keys are
trusted. Treat both as reviewed configuration owned by the same people who
hold the operator token.

### Who can read credentials

| Secret | Mounted into | Also readable by |
| --- | --- | --- |
| `db_superuser_password` | db | the backup and restore scripts (host) |
| `db_importer_password` | db (initialisation), publisher, importer | – |
| `db_api_password` | db (initialisation), api | – |
| `db_monitor_password` | postgres-exporter | `scripts/create-monitor-role.sh`, `scripts/rotate-db-password.sh` (host) |
| `operator_token` (raw, all scopes) | operator-cli | operators |
| `operator_monitor_token` (raw, scope `status`) | prometheus | monitoring |
| `operator_tokens`, `metrics_tokens` (hashes only) | publisher, api | – |
| an online source's bearer token (optional) | fetcher | – |

**Root and the `docker` group on the host can read every secret and every
volume.** Docker access is equivalent to root. Grant it only to the
people who operate the host, and remember that the backup, restore and
rotation scripts need it.

### Least-privilege audit

| Identity | Can | Cannot | Notes |
| --- | --- | --- | --- |
| API process, role `karta_api` | connect; `SELECT`/`EXECUTE` on release databases and the serving part of the registry | write anything (read-only role, database and session defaults; 3 s / 5 s statement timeouts); read submissions, authorizations or the audit log | tested: `TestStack` "database roles are least-privilege" |
| publisher and importer, role `karta_importer` | `CREATEDB`; own the registry and the release databases it creates | superuser actions; reach the internet (no NAT); write its root filesystem | it can drop the audit trigger it owns (documented residual risk) |
| fetcher | write its outbox; reach the configured source over HTTPS | reach the database (not on `backend`), publish anything (the publisher re-verifies every delivery) | tested: `TestOnline` "a compromised fetcher cannot publish …" |
| operator token | every operator action, audited | anything outside the operator API | keep it with operators; the publisher holds only its hash |
| monitoring token (`status`) | read operator status and both metrics endpoints | publish, roll back, authorize, clean up | status names submissions, digests and audit reasons, but no secret; a narrower metrics-only scope is a possible later refinement |
| exporter, role `karta_monitor` | `pg_monitor` statistics and settings; 3 connections; read-only sessions; `CONNECT` to `postgres` only | read table data | `scripts/create-monitor-role.sh` |
| backup and restore | the superuser password and Docker access on the host | – | a dedicated `REPLICATION`-only backup role would not narrow this while the scripts need Docker access, which is root-equivalent. Choose it with the backup destination if backups move to a separate backup host |

### Production secret distribution

`make secrets` writes random secrets into `./secrets` on the host: a
development convenience. The files are 0644 inside a 0700 directory so that
the containers' non-root users can read the bind mounts. For a production
host:

* Create the secrets on the host from a secret store or an offline copy,
  never from Git and never through `.env` (`.env` holds no secret).
* Keep the directory owned by root (or a dedicated operator account), mode
  0700. Alternatively, give each service its own file with the owning UID
  (65532 for the API, 10001 for the publisher and importer, 999 for
  PostgreSQL, 65534 for Prometheus and the exporter) and mode 0400, through
  the orchestrator's secret mechanism.
* Keep raw operator tokens with the operators. The host needs only
  `operator_tokens` and `metrics_tokens` (hashes) unless `make op` is run
  there.
* Keep an offline copy of `./secrets` separate from the backups. A backup
  records only each secret's SHA-256 fingerprint.

The network, TLS and operator-access arrangement, and the production secret
store, are owner inputs.

### Images and upgrades

1. `make backup` (the restore point if the upgrade fails).
2. `git pull` to the new reviewed revision; `make build`.
3. `make up` (and `make up-monitoring` / `make up-online` where used). The
   publisher migrates the registry schema at start. The API keeps serving
   the active release throughout; `/health/ready` answers 200 again within
   seconds.
4. `make op-status`, `make smoke`.

A **failed upgrade** is undone by restoring the backup from step 1 with the
previous revision's images: check out the `karta_commit` recorded in the
backup's `MANIFEST`, `make build`, then
`make restore BACKUP=backups/karta-… REPLACE=1`. Tested in tier A: a
registry left at a schema version the build cannot run is refused by the
publisher and undone by restoring. Upgrading the PostgreSQL/PostGIS image
changes the serving toolchain: see the runbook, "Upgrading the database
image".

## Bounded publication

Every publication (inbox, online delivery and `karta import`) runs under one
deadline, `KARTA_PUBLISH_TIMEOUT` (default **6h, provisional**: set it from
the tier D publication time plus a recorded margin). The deadline covers
staging, verification, osm2pgsql, the post-import SQL and validation. When
it passes:

* every database statement of the job is cancelled. osm2pgsql is killed
  together with every process it started (its own process group), and the
  build stops waiting for its output even if a stray descendant still holds
  the pipe (`exec.Cmd.WaitDelay`). The publisher and importer run under
  `docker-init`, which reaps orphans;
* the candidate database is dropped, the active release keeps serving, and
  the submission ends `failed` with reason code **`publication_timeout`**
  (audited). The reason names the phase it was in;
* it is **not retried automatically**: it does not count toward
  `KARTA_PUBLISH_MAX_ATTEMPTS` the way an interrupted publication does,
  because a build that does not fit the budget would only time out again.
  Find the cause (`make logs`, the reason's phase), raise the deadline if the
  build legitimately needs longer, then submit again by touching the ready
  marker (inbox), with `online-retry` (online), or by running the import
  again;
* `karta import` exits with code **8**.

The switch itself (the pointer transaction) is not cut off by the deadline
or by a shutdown. It has its own bound (pointer lock timeout + 30 s), so a
release validated in time is not refused at the last moment, and the
pointer still moves only in that committed transaction. A shutdown during a
build (Compose stop grace period 30 s) records the publication
`interrupted` and the next start retries it.

Tier A evidence (`TestOperations`): a hung import stopped at a 20 s
deadline together with a child holding the output pipe (stopped after
21.8 s, no importer process left); the same snapshot published afterwards
while a stray child kept the pipe open (12.7 s); a blocked post-import SQL
statement cancelled at the deadline; a shutdown during a build recorded
`interrupted` and retried after restart; `karta import` exit code 8.
Unit tests cover process-group cancellation, a `setsid`-escaped child and
bounded output reading. Tier B: the Chitgar publication ran under a
configured 10-minute deadline (see the PR). Tier D (the full Iran
publication time, and the budget derived from it) is open.

## Monitoring

### What is exported

* **Publisher**, `GET /v1/operator/metrics` on the operator API (scope
  `status`): active release and data age (`karta_active_data_age_seconds`,
  `karta_data_stale`, `karta_data_stale_after_seconds` when configured),
  releases and submissions by state, storage use and budget, staging and
  database-volume free space, publication in progress
  (`karta_publication_running_seconds`, `karta_publication_timeout_seconds`),
  outcomes (`karta_publications_total{source,state}`), the duration
  histogram (`karta_publication_duration_seconds{source}`), timeouts
  (`karta_publication_timeouts_total`), the online source and fetcher state,
  and the process start time.
* **API**, a separate metrics listener (`KARTA_METRICS_LISTEN_ADDR`, `:9464`
  in Compose, never the public listener): readiness and its reason, the
  loaded release, requests by route and status class, the request duration
  histogram by route, in-flight requests, heap, goroutines and start time.
  Labels are bounded: route names come from a fixed list (unmatched paths
  are `not_found`, refused requests `refused`), and status codes are
  grouped by class. There are no paths, queries, client addresses or
  release-specific URLs.
* **PostgreSQL**, `postgres-exporter`: `pg_up`, connections, database sizes,
  WAL size and `max_wal_size`, and replication and checkpoint statistics.
* **Host and containers** (disk, memory against limits): not shipped.
  Provide node_exporter and cAdvisor on the host and add them to the scrape
  configuration (commented in `deploy/monitoring/prometheus.yml`). The disk
  and container-memory alerts need them.

### Running it

```bash
make up-monitoring    # creates/updates role karta_monitor, starts the exporter and Prometheus (profile monitoring)
make test-alerts      # promtool: check the configuration and run the rule unit tests
open http://127.0.0.1:9090/alerts
```

A Prometheus elsewhere can scrape the same endpoints through the loopback
ports (`127.0.0.1:9464/metrics` and `127.0.0.1:8081/v1/operator/metrics`
with `Authorization: Bearer <operator_monitor_token>`; `127.0.0.1:9187`).
Load `deploy/monitoring/alerts.yml` together with
`deploy/monitoring/thresholds.yml`. Where alerts are delivered
(Alertmanager, on-call) is an owner decision and is not configured.

### Thresholds

`thresholds.yml` holds every number the alerts compare against, as
recording rules in one place:

* **engineering guards** (*provisional*, to be set from tier D): free disk
  below 10 %, container memory above 90 % of its limit, release storage
  above 80 % of `KARTA_RELEASE_STORAGE_BUDGET_MB`, a publication running
  600 s past its deadline, and the fetcher 900 s late for its own next
  check;
* **owner objectives**: API error ratio and latency. None is set, so the
  error and latency alerts cannot fire and `KartaObjectivesUnset` says
  so.

The acceptable active-data age is the publisher's own
`KARTA_DATA_STALE_AFTER` (exported as `karta_data_stale_after_seconds`), so
operator status, metrics and alerts agree. It is unset by default, and
`KartaDataAgeThresholdUnset` makes that visible. Data age is computed from
the active release's OSM data timestamp, never from the time of the last
successful check: a source that answers with nothing new does not make old
data fresh.

Tier A evidence: `deploy/monitoring/tests/alerts_test.yml` (synthetic
series: fresh to stale to fresh, an answering source with old data, an unset
threshold, paused activation, refused and failing sources, a stopped
fetcher, missing targets, publication timeouts, failures and overruns,
objectives set and unset, disk, memory and WAL). The live check in
`TestOperations` runs the monitoring profile against the test stack: every
target is up, the rules load, and `KartaAPINotReady` and `KartaPostgresDown`
fire when the database stops and clear when it returns. Tier B: the profile
scraped the Chitgar deployment with every target up and 31 rules healthy.
Tier D (alerts firing and clearing on the target host, to the chosen
destination) is open.

### Alert responses

Every alert links here. First steps for all of them: `make op-status`,
`make logs`, `docker compose ps`.

#### KartaAPIDown

The API's metrics listener does not answer. Check `docker compose ps api`
and its logs. If the container is up but the listener is not, check
`KARTA_METRICS_LISTEN_ADDR` and the port binding. If the public API answers
(`make smoke`), only monitoring is affected.

#### KartaAPINotReady

`/health/ready` is not 200. The `reason` label tells why:
`database_unavailable` (see KartaPostgresDown), `no_active_release` (see
KartaNoActiveRelease), or `release_incompatible` (after a database image
upgrade: publish the snapshot again, see the runbook "Upgrading the database
image").

#### KartaPublisherDown

The publisher does not answer: publication, cleanup and every
publisher-derived alert (data age, publication, online) are blind. Serving
is unaffected. Check `docker compose ps publisher` and its logs. A
publisher refusing to start logs why (for example a registry schema newer
than the build: a failed upgrade, see "Images and upgrades").

#### KartaScrapeTargetMissing

A Karta job is missing from the scrape configuration, so its alerts cannot
fire. Restore the `karta-api` and `karta-publisher` jobs in the Prometheus
configuration.

#### KartaNoActiveRelease

Nothing is being served. On a new deployment, publish the first snapshot.
Otherwise the active release was lost: check `make op-status` and restore
from backup if the registry is damaged ("Backup and restore").

#### KartaRestartLoop

A process restarted more than twice in an hour. Check its logs for the exit
reason; common causes are memory limits (`docker inspect` shows
`OOMKilled`), an unreadable secret or configuration, and an unreachable
database.

#### KartaDataStale

The active data is older than `KARTA_DATA_STALE_AFTER`. Publish a newer
snapshot: deliver one to the inbox with its digest pinned or authorized
(runbook, "Publish a snapshot", "Authorize a new snapshot"), or, with online
updates, check the fetcher and the source (the alerts below). Serving
continues with the old data.

#### KartaDataAgeThresholdUnset

No acceptable data age is configured, so stale data cannot alert. Set
`KARTA_DATA_STALE_AFTER` in `.env` once the owner has decided it, then
`make up`.

#### KartaFetcherOverdue

The fetcher is long past its own scheduled next check: it is stopped or
stuck. Run `docker compose --profile online ps fetcher` and check its logs;
`make up-online` restarts it. Its reports are not authenticated, so data
age (KartaDataStale) remains the authoritative signal.

#### KartaFetcherNoState

Online updates are configured but the fetcher has never written its state.
It is not running, or it refuses to start (runbook, "The fetcher refuses to
start: state cannot be read").

#### KartaOnlineSourceRefused

The source failed a trust check: an untrusted or invalid signature, a
replayed or conflicting manifest, a wrong region, a digest or size mismatch,
or a refused URL, destination or redirect. Nothing was published. Treat it
as a possible attack or a broken producer. Contact the producer, and if a
key may be compromised follow the runbook's key-compromise steps.

#### KartaOnlineSourceFailing

Checks have failed for two hours (the `code` label tells why: network,
timeouts, HTTP errors). Serving and manual delivery are unaffected. Fix the
connectivity or the source, or deliver the snapshot manually.

#### KartaOnlineActivationPaused

Validated online snapshots are kept `ready` instead of being activated:
paused by an operator, by a rollback or by a restore. When the reason is
resolved (for a restore: the producer's current serial checked, see
"Backup and restore"), run `make op CMD='online-resume --reason "..."'`.

#### KartaPublicationTimedOut

A publication was stopped at `KARTA_PUBLISH_TIMEOUT` (reason code
`publication_timeout`; the reason names the phase). See "Bounded
publication" for what to check and how to resubmit.

#### KartaPublicationFailed

A submission ended `failed` or `rejected`. Its reason code and reason are
in `make op-status`; the runbook's "Rules" and failure-code tables say what
each code means. The active release is unchanged.

#### KartaPublicationOverrun

A publication has run far beyond its deadline, so the deadline did not stop
it. Serving is not affected, but a bug has been hit. Collect the
publisher's logs and `docker top` of the publisher container, then restart
the publisher (`docker compose restart publisher`): the publication is
recorded `interrupted` and its candidate is dropped at the next start.
Report it.

#### KartaReleaseStorageNearBudget

Release databases are close to `KARTA_RELEASE_STORAGE_BUDGET_MB`, so the
next build may be refused (`insufficient_storage`). Run
`make op CMD='cleanup --dry-run --reason "check"'`. Then clean up or lower
`KARTA_RETAIN_RELEASES`, or raise the budget if the disk allows (see
"Capacity").

#### KartaAPIErrorRatioHigh

The share of 5xx answers is above the owner's objective. Check readiness
and the database first. Then check the API's logs for the route, and
saturation (CPU of `db` and `api`, latency).

#### KartaAPILatencyHigh

The objective quantile of request time is above the owner's bound. Check
the database's CPU (in tier B the database container's CPU limit saturated
first), the request rate and in-flight requests, and whether a publication
is building (it competes for the database).

#### KartaObjectivesUnset

No API error or latency objective is configured, so the two alerts above
cannot fire. Fill in the `karta-objectives` group in `thresholds.yml` once
the owner has decided them.

#### KartaPostgresExporterDown

The exporter does not answer, so database alerts are blind. Check
`docker compose --profile monitoring ps postgres-exporter`, the
`karta_monitor` role (`make monitoring-role`) and `db_monitor_password`.

#### KartaPostgresDown

PostgreSQL does not answer. The API reports `database_unavailable` and
recovers by itself when the database returns. Check `docker compose ps
db`, its logs, and the disk (KartaDiskLow). If the data is lost or
corrupt, restore ("Backup and restore").

#### KartaPostgresWALGrowing

WAL is over twice `max_wal_size`. Without archiving or replication slots it
should be recycled at checkpoints. Check for a forgotten replication slot
(`SELECT * FROM pg_replication_slots`), a long-running transaction
(`pg_stat_activity`), and checkpoint errors in the logs.

#### KartaDiskLow

A filesystem has less than the guard's free fraction left. Find what grew:
`make op-status` (release storage, staging), `docker system df`, backups,
logs. Run cleanup (`make op CMD='cleanup --reason "disk"'`) and move
backups off the host. In tier B, with the database disk 100 % full, serving
continued without errors, a build failed `insufficient_storage` with the
active release unchanged, and after space was freed a new marker published
the snapshot. A sustained write load on a full database disk can still stop
PostgreSQL.

#### KartaDiskFillingUp

At the current rate a filesystem fills within a day. Act as for
KartaDiskLow before it is full.

#### KartaContainerMemoryHigh

A container's working set is near its Compose limit. For the publisher
during a build this can be expected (osm2pgsql's cache is
`KARTA_OSM2PGSQL_CACHE_MB`). Otherwise check for growth over time. Raise the
limit or lower the cache from capacity measurements ("Capacity").

## Backup and restore

### What a backup holds, and what it does not

`make backup` (`scripts/backup.sh`) writes `backups/karta-<UTC time>/`:

* `db/`: a **physical base backup of the whole PostgreSQL cluster**
  (`pg_basebackup`: tar, gzip, WAL streamed, SHA-256 manifest), verified with
  `pg_verifybackup` before the backup is complete. It holds the registry
  (active pointer, releases, submissions, authorizations, audit log, online
  anti-replay state), every release database consistent with it, and the
  database roles;
* `online.tar`: the fetcher's outbox (its state, with the highest verified
  serial, and complete deliveries; partial downloads are excluded), copied
  while the fetcher is paused;
* `registry.before.json`, `registry.after.json`: what the registry held
  before and after the base backup (`karta registry-summary`), which
  `restore-check` compares against;
* `config/`: region and source files, `.env`, the Compose files and the
  monitoring thresholds;
* `MANIFEST` (ids, Karta commit, image ids, PostgreSQL version, secret
  fingerprints, duration) and `SHA256SUMS`.

**Not in the backup:** secrets (keep `./secrets` offline, separately) and
the snapshots themselves. The PBF files and their sidecars are the
operator's input. Archive the active and retained snapshots where they came
from or next to the backups.

**Why release databases, not just snapshots.** A backup that holds the built
release databases restores in the time it takes to copy them. It keeps
release ids, and therefore immutable URLs and pinned clients, exactly as
they were, because the toolchain is part of a release id. Rebuilding from
snapshots instead takes the import time of every retained release (Iran's
is not measured yet). It needs the same toolchain to reproduce the same
release ids, and it loses the audit and anti-replay state unless the
registry is backed up anyway. Rebuilding from archived snapshots remains
the fallback if every backup is lost. Re-decide when the tier D import time
and backup size are known.

### Protection

The backup directory is 0700 and its files 0600. It holds password
verifiers, the audit log (operator names and reasons) and the data: store
it encrypted, with access limited like the secrets, and copy it off the
host. The destination, its encryption and its retention are owner inputs.
`SHA256SUMS` and `pg_verifybackup` detect damage before a restore changes
anything (tested: a damaged file, and a damaged base backup whose
`SHA256SUMS` was rewritten to match, are both refused).

### Restore

```bash
# on the target host: the checkout at the backup's karta_commit, images built,
# ./secrets restored from offline storage (or new ones: make secrets)
make restore BACKUP=backups/karta-20261002T150153Z            # empty host or project
make restore BACKUP=backups/karta-20261002T150153Z REPLACE=1  # deletes this project's database and outbox first
```

`scripts/restore.sh` restores in this order, and stops at the first
failure:

1. verify `SHA256SUMS` and `pg_verifybackup`; tablespace archives are
   refused (see below);
2. compare `./secrets` with the backup's fingerprints (a difference only
   warns: step 5 applies the current secrets);
3. refuse to overwrite a database or a non-empty outbox without `--replace`;
4. extract the base backup and its WAL into a new `pgdata` volume. PostgreSQL
   replays to the backup's consistent end when it starts;
5. set every role's password to the current `./secrets`
   (`rotate-db-password.sh --current`), because a physical backup carries
   the old cluster's roles;
6. restore the fetcher's outbox;
7. `karta restore-check --finalize`. The checks: registry schema; every
   retained release database present and read-only; the active release
   loadable; the audit table append-only; the active release, the
   retained/pinned releases and the audit history as backed up; the
   anti-replay floor not lowered. Notes report changes during the backup
   and a fetcher serial above the registry's. Only if every check passes, it
   **pauses automatic online activation** and **audits the restored active
   pointer** (action `restore`, like a rollback) in one transaction. The
   report is saved in `backups/restore-reports/`;
8. start the API and the publisher and wait until the API is ready.

The fetcher is not started. After a restore:

* **Anti-replay.** The registry's verified serial is the one from the
  backup. Before resuming online activation, confirm the producer's current
  serial. If the restored fetcher state holds a higher verified serial,
  restore-check reports it ("verified serial N, above the registry's M"):
  that serial was verified but never delivered. It is never reset silently;
  the fetcher keeps it as its floor. Then `make up-online` and
  `make op CMD='online-resume --reason "serial checked after restore"'`.
* **Fetcher state lost or unreadable.** An unreadable state file stops the
  fetcher from starting (runbook, "The fetcher refuses to start"). A lost
  one makes it start from nothing, but the registry's serial still refuses
  anything older than it. Serials between the registry's and the lost one
  are exactly why activation stays paused until an operator has checked the
  producer.
* **Publications after the backup** are not in it: the recovery point is
  the backup's time. Submit their snapshots again (touch the ready
  markers), and authorize again any digest authorized after the backup.
* `make op-status`, `make smoke`.

**Tablespaces.** With `KARTA_RELEASE_TABLESPACE`, `pg_basebackup` writes one
more archive per tablespace (`<oid>.tar.gz`). `restore.sh` refuses such a
backup. Restore by hand: extract each archive into an empty directory on
the tablespace's volume, owned by `postgres` with mode 0700, and point
`$PGDATA/pg_tblspc/<oid>` at it before starting PostgreSQL.

### Drills and measurements

| Drill | Tier | Evidence |
| --- | --- | --- |
| Backup while serving; restore into the same project after the database and outbox volumes are deleted (database loss): pointer, retained releases, audit history, anti-replay serial and fetcher state, read-only release databases, every credential | A | `TestOperations` "a backup restores …": backup 7.4 s, restore 17.2 s |
| A publication after the backup is absent after the restore (recovery point = backup time) | A | same test |
| Failed upgrade (registry schema newer than the build) undone by restoring with `--replace` | A | same test, "a failed upgrade is undone …" |
| A damaged backup, and a restore over a database without `--replace`, refused before anything changes | A | same test |
| Disk exhaustion before a build, in staging and in the database during an import | A | `TestPublication` "disk exhaustion fails safely" |
| Rollback, pinned clients, cleanup, crash at every transition | A | `TestPublication`, `TestOnline` |
| Chitgar: backup under load (8,301 requests, 0 errors), restore into an isolated project | B | backup 10 s (16 MB), restore 16.7 s, all checks passed |
| Chitgar: restore onto a small database filesystem; a second real-data release from the same file (default view changed); rollback under load with a client pinned to the replaced release | B | 0 errors; pinned release still served; rollback audited |
| Chitgar: database disk 100 % full while serving | B | 0 errors, readiness 200; build failed `insufficient_storage`, active unchanged; published after space was freed |

Recovery time and data loss against the owner's objectives (tier D) are
open. The restore time scales with the cluster size. The data loss is
everything published or authorized since the last backup, so backup
frequency follows from the owner's data-loss objective.

## Credentials and signing keys

Rotation that keeps data. Rehearsed with throwaway material in tier A.

### Database passwords

```bash
make rotate-db-password ROLE=api        # or importer, monitor, superuser
```

A new random password is set with `ALTER ROLE` (passed on standard input,
with statement logging off for that session), then written **in place**
into `secrets/db_<role>_password` (the bind mount keeps seeing the same
file), then verified with a TCP login. Every service reads its password
file for each new connection, so **no restart** is needed: open connections
keep working and new ones use the new password. Tested in tier A under load
(3,480 requests, 0 errors, no container restarted), with the old password
refused afterwards. This replaces "delete `./secrets` and `make reset`",
which deleted all data.

### Operator and monitoring tokens

```bash
make rotate-operator-tokens     # new operator_token and operator_monitor_token, hashes rewritten, publisher recreated
```

The publisher reads `operator_tokens` at start, so the rotation recreates
it. A publication running at that moment is recorded `interrupted` and
retried. The API's metrics listener re-reads `metrics_tokens` when it
changes, so it needs no restart. After the rotation the old monitoring
token is refused by both, and Prometheus reads the new one from its file at
the next scrape. Tested in tier A.

**Overlap, for other credentials.** Add the new credential's line
(`NAME SCOPES SHA256`) to `operator_tokens` and restart the publisher, move
the clients over, then remove the old line and restart again. **Revocation**
is the second half alone. After a suspected leak, revoke first; actions
taken with the token are in the audit log under its name.

### Signing keys (online updates)

Rotation with an overlap, revocation, compromise and `not_after` are in the
runbook ("Rotate or revoke a signing key"). Both the fetcher and the
publisher read the source file for every check, so no restart is needed.
Tier A tests: rotation with both keys during the overlap and expiry by
`not_after` (`TestVerifyKeyRotation`); revocation, loss of the private key
(a replacement key is refused until the reviewed source file trusts it),
and a revoked key that cannot borrow the trusted key's id
(`TestVerifyKeyLossAndRevocation`); and a removed key stopping a build
already running (`TestOnline`).

**Loss of the private key** (as opposed to compromise): nothing published is
affected, but no new manifest can be signed. Have the custodian generate a
new key, add its public key to the source file (reviewed change), and have
the producer sign with it. **Rollback** of a bad key change is the previous
reviewed source file.

There is no production signing key, provider or custodian: those are owner
decisions, and online updates stay off until they are made. Manual delivery
needs no key. Full-Iran snapshots arrive in the inbox with a pinned or
operator-authorized digest whether or not online updates are activated. A
distributor's checksum or a raw OSM download is not a signed manifest.
When a production source is chosen, record whether the fetcher reaches it
directly over HTTPS. The fetcher has no proxy support, so a proxy path
would have to be implemented and tested first.

## Capacity

### Measuring

`scripts/capacity-run.sh LABEL [karta-load flags]` measures a running
deployment. It records the host, versions, images, limits, the active
release, the database sizes and PGDATA bytes, samples every container's CPU
and memory every 2 s, and runs `karta-load` (shipped in the importer image)
against the API. Each run is written as one report directory under
`artifacts/capacity/`.

```bash
scripts/capacity-run.sh tier-b-chitgar-closed-c16 -duration 60s -concurrency 16            # closed loop
scripts/capacity-run.sh tier-d-iran-rate200 -rate 200 -duration 10m -concurrency 64        # open loop at a fixed rate
```

`karta-load` pins one release by default (as clients do). It mixes tiles,
searches, manifest and style requests by weight
(`-mix tile=70,search=20,manifest=5,style=5`) over a zoom range and query
list, and reports per-kind latency percentiles, errors, the achieved rate
and, in open loop, how many requests started late (the rate was not
reached). A run is repeatable for the same `-seed` and data.

Tier D scenarios to run on the selected host, each recorded with its
label: sustained map and search load at the owner's traffic objective and
above it to saturation; the same load during a full-Iran publication
(slow download, build and switch) with pinned clients; a network outage;
restarts of the API, publisher and database; a rollback; and a backup and
restore. Record hardware, Docker and PostGIS versions, the PBF digest and
provenance, the configured limits and concurrency, peak disk, RAM and CPU,
import time, candidate, active, retained and pinned storage, API rate,
latency distribution, errors and availability.

### Defaults to re-derive from tier D

Chosen before any full-Iran measurement, all *provisional*:

| Setting | Default | Note |
| --- | --- | --- |
| osm2pgsql cache, processes, mode | 800 MB, 2, non-slim | `KARTA_OSM2PGSQL_CACHE_MB`, `KARTA_OSM2PGSQL_PROCESSES` |
| memory limits | db 2 GiB, publisher 4 GiB, api 512 MiB | `.env` |
| release storage budget | 10240 MB | `KARTA_RELEASE_STORAGE_BUDGET_MB` |
| candidate estimate | 40 × snapshot + 32 MiB | `KARTA_CANDIDATE_SIZE_FACTOR`; for the 229,580,914-byte Iran file the Chitgar sidecar claims, about **8.6 GiB** for the candidate alone |
| publication deadline | 6 h | `KARTA_PUBLISH_TIMEOUT` |
| API database pool | 8 connections | `KARTA_DB_MAX_CONNS` |
| alert guards | see "Thresholds" | `thresholds.yml` |

Derive each from the measurements with a recorded margin. Tier B figures
(in the PR) show the database container's CPU limit (2 CPUs) saturating
first under map and search load. That is a hint for where to look, not an
Iran figure.

### The Iran region

The Iran region file cannot be written before the snapshot is chosen: its
box must equal the snapshot's header or sidecar box (to 1e-7°), and its
acceptance checks must come from a measured import of that snapshot. Never
copy them from Chitgar or scale them.

```bash
# in the importer image, with the chosen snapshot and its sidecar under data/local/
docker compose run --rm --no-deps -v "$PWD/data/local:/in:ro" --entrypoint /usr/local/bin/karta importer \
  region-draft --snapshot /in/iran.osm.pbf --id iran --name "Iran" > config/regions/iran.json
```

`region-draft` reads the box from the snapshot's header, or from its
sidecar after verifying that the sidecar describes this file (it refuses a
mismatch). It pins the SHA-256, centres the default view, and leaves
`min_counts`, `max_drop_fraction`, searches and tiles empty. Then:

1. import with `--no-activate` (`karta import`) on the target host and
   record the import report: counts, timings, peak RSS, database size;
2. set `min_counts` and `max_drop_fraction` from the measured counts, and
   searches and tiles from places verified in the built release;
3. review the file, set `KARTA_PUBLISH_REGION=iran`, and activate.

Record the snapshot's provenance chain: distributor or extraction, file,
digest, replication sequence and timestamp. Mark each claim that could not
be verified. The Chitgar sidecar's claims about its source Iran file
(`iran-260927.osm.pbf`, 229,580,914 bytes, SHA-256 `fbb1b010…4529`, sequence
4920, 2026-09-27T20:23:36Z) are **recorded, not verified**: that file was
not available.

## Owner inputs

Not decided by the implementation; each keeps a Stage 4 gate open:

* **Iran snapshot source** (distributor or extraction process), which also
  fixes the Iran region box;
* **trusted production online source**: provider, manifest signer and
  private-key custodian, and when to activate online updates in a
  deployment (off until then; manual delivery needs none of them);
* **acceptable active-data age** (`KARTA_DATA_STALE_AFTER`; with online
  updates also `max_manifest_validity` and the poll interval);
* **traffic/concurrency, availability and latency objectives** (the
  `karta-objectives` thresholds);
* **target host** and its resources and storage;
* **backup destination and protection**, and the **recovery-time and
  data-loss objectives**;
* **network, proxy and TLS arrangement**, operator access, the production
  secret store, and the **alert destination**;
* whether release databases or backups leave the operator's control, which
  raises the ODbL derivative-database question ([licenses](licenses.md));
  counsel if so.
