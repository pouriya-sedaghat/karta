# ADR 0005: Stage 4, operations and capacity

Status: proposed with the Stage 4 draft PR, 2026-10-02. Builds on
[ADR 0001](../architecture.md) and ADRs [0002](0002-stage1-implementation.md),
[0003](0003-stage2-publication.md) and [0004](0004-stage3-online-updates.md).
No stored-data migration (the registry stays at schema version 3) and no
public API change. The operator API gains a reason code and more metrics
(descriptions only), and the API process gains a separate metrics listener.

## Context

Stage 4 is the operational and capacity gate for the selected region
(Iran, from raw OSM data), source and host. Hybrid publication is settled:
manual delivery with a pinned or operator-authorized digest always works,
and configured online delivery needs a signed manifest. Activating online
updates in a deployment, the Iran snapshot source, the target host and every
service objective are owner decisions that are still open. This ADR covers
what can be built and proven without them. Tiers A (fixtures) and B
(Chitgar) are evidence; tier D (full Iran on the target host) closes the
gates.

Gaps found at the start: no whole-publication deadline (the inbox scan ran
under the service's lifetime context); osm2pgsql started without a
process-group policy, and its output was read to EOF before `Wait`, so a
descendant holding the pipe blocked the build even after osm2pgsql died;
no init in the publisher and importer containers; no API request metrics,
no publication duration metric, no database or host metrics, no alert
rules; no backup tooling; and the only documented database-password
rotation (`make reset`) deleted all data.

## Decisions

### One deadline per publication

`KARTA_PUBLISH_TIMEOUT` (default 6h, provisional) bounds a whole
publication: inbox, online and `karta import`, from staging through
validation. The job context is `context.WithTimeoutCause(…,
ErrPublicationTimeout)`, so every database statement is cancelled, and the
staging copy and the snapshot's structural scan stop at their next read
(both were uninterruptible before: bounded by the input size, but not by
time). A copy stopped by a shutdown is `interrupted` and retried, like any
interrupted publication. osm2pgsql
starts in its own process group (`Setpgid`), and cancellation kills the
group. `exec.Cmd.WaitDelay` bounds reading its output after it exits, and
the group is killed again after `Wait`, so a stray descendant can neither
block the build nor survive it. Compose runs `docker-init` as PID 1 in the
publisher and importer to reap orphans.

The **switch is outside the deadline**. It runs in a context detached from
the job and from shutdown (`context.WithoutCancel`), bounded by the pointer
lock timeout plus 30 s. A release validated in time is never refused
because the deadline passed during its millisecond switch, and a switch is
never cancelled between commit and result. The pointer still moves only in
that committed transaction.

A timeout is a **final** `failed` with its own reason code,
`publication_timeout` (audited, metric `karta_publication_timeouts_total`,
CLI exit 8). It is not `interrupted` and does not consume
`KARTA_PUBLISH_MAX_ATTEMPTS`: a snapshot that cannot be built within the
budget would only time out again, so the operator fixes the cause or raises
the deadline and resubmits explicitly. A shutdown during a build (Compose
grace 30 s) stays `interrupted` and is retried, as before.

### Metrics without a client library, on separate listeners

The API exports request counts by route and status class, a latency
histogram by route, in-flight requests, readiness and its reason, the
loaded release and process figures. They are served on a **separate
listener** (`KARTA_METRICS_LISTEN_ADDR`, `:9464` in Compose) that requires
a bearer credential with scope `status`, read from a hashes-only file
(`metrics_tokens`) that is re-read when it changes. The public listener has
no metrics route. Route labels come from the mux pattern through a fixed
map, and unmatched requests are `not_found` or `refused`, so cardinality is
bounded and no path, query or client address becomes a label. The
publisher adds staging and database-volume free space, the deadline, the
running time, outcome counters, timeouts and a duration histogram per
source.

Text exposition is written by a small internal package (`internal/promtext`)
rather than the Prometheus client library. The format needed is a handful
of gauges, counters and fixed-bucket histograms, and avoiding the
dependency keeps the static binaries and the license and vulnerability
surface unchanged.

PostgreSQL is observed by the upstream `postgres_exporter` (pinned by
digest) as role `karta_monitor` (`pg_monitor`, read-only sessions, 3
connections, `CONNECT` to `postgres` only). It runs on a `monitoring` network
without NAT. Host and container metrics (disk, memory against limits) come
from node_exporter and cAdvisor, which the host provides: shipping
privileged host agents in this stack would widen it more than it helps. An
optional Compose profile (`monitoring`) runs the exporter and a Prometheus
loading the rules.

### Alert rules: guards, owner objectives, data age

`deploy/monitoring/alerts.yml` (26 rules) covers availability (down,
not ready, no active release, restart loops, **missing scrape targets** via
`absent()`), freshness (data age, unset threshold, overdue or absent
fetcher, refused or failing source, paused activation), publication
(timeouts, new failures, overrun past the deadline, storage near budget),
objectives (API error ratio and latency) and resources (exporter,
PostgreSQL, WAL growth, disk, container memory). Every rule links to its
response in docs/operations.md.

Thresholds live in `thresholds.yml` as recording rules, in two kinds. The
**engineering guards** are provisional and are set from tier D. The **owner
objectives** are deliberately empty, so the error and latency rules never
fire, and `KartaObjectivesUnset` makes that visible. The **acceptable data
age** stays the publisher's `KARTA_DATA_STALE_AFTER` (exported as a
metric): status, manifest freshness and the alert then use one threshold.
It is unset by default, which `KartaDataAgeThresholdUnset` reports. Data
age comes from the active release's data timestamp, never from check
success. The rules are unit-tested with promtool on synthetic series, and
the integration suite runs them against the live test stack. No alert
destination is configured (owner decision).

### Backups: a physical base backup of the cluster

`scripts/backup.sh` takes a `pg_basebackup` of the whole cluster (tar,
gzip, streamed WAL, SHA-256 manifest), verifies it with `pg_verifybackup`,
and records registry summaries before and after. It also copies the
fetcher's outbox (fetcher paused), the configuration and a manifest with
secret fingerprints, and writes `SHA256SUMS`. Secrets are never included.

A physical backup was chosen over per-database `pg_dump`:

* one consistent point for the registry and every release database (the
  registry's references to release databases cannot be torn);
* the roles come along;
* restore is a file copy plus WAL replay instead of rebuilding indexes for
  every release;
* `pg_verifybackup` checks it end to end.

The cost: it restores only onto the same PostgreSQL major version and
PostGIS build. The backup records the database image, pinned by digest,
and `restore.sh` uses it.

It holds the built release databases, not just snapshots. Restore time is
then copy time rather than import time (Iran's import time is unmeasured).
Release ids, and therefore immutable URLs and pinned clients, survive
exactly. Rebuilding from archived snapshots remains the fallback, to be
re-decided with the tier D import time and backup size.

### Restore: verified, audited, anti-replay preserved

`scripts/restore.sh` has a fixed order: verify, compare secrets, a
preflight of both destinations, database, role passwords from the current
secrets, outbox, `karta restore-check --finalize`, then serving. The
preflight refuses before anything changes if the database volume or the
fetcher's outbox holds data and `--replace` is not given. With `--replace`
and a backup without an outbox, the outbox is emptied rather than left with
another registry's fetcher state.

`restore-check` verifies release databases present and read-only, the
audit table append-only, and the active release loadable. It also checks
the restore against the two summaries the backup takes around
`pg_basebackup`, each read from one repeatable-read snapshot. A physical
base backup restores the registry as it was when the base backup ended,
which is any moment between the two summaries. So the restore is checked
as an interval, not against either endpoint: the append-only audit log and
the verified serials must lie between the two, releases present in both
must be present, and the anti-replay floor must not drop. The active
pointer is pinned down exactly, because every switch is audited in its own
transaction: the restore's pointer must be the one its own audit log's
last switch after the first summary activated, or the first summary's.
Restoring the backup into a scratch cluster to summarize its exact state
would be equally exact, but it doubles the backup's time and space.

Restoring an older registry restores an older pointer and an older
`registry.source_state` serial. So only after every check passes, one
transaction **pauses automatic online activation** (the existing
`online_pause` path, with cause `restore`) and **audits the restored
pointer** (action `restore`) like a rollback. Re-deriving the anti-replay
floor automatically was rejected: the fetcher's state is untrusted (it
faces the network), and the producer's current serial is not knowable
offline. An operator confirms it and resumes. A fetcher serial above the
registry's is reported and kept as the fetcher's floor, never reset. No
schema change was needed: the audit action is free text, and the pause uses
the existing policy row.

### Rotation that keeps data

Every Karta process now reads its database password file for **each new
connection** (`pgxpool` `BeforeConnect`, and the same for single
connections). `scripts/rotate-db-password.sh` sets a new password with
`ALTER ROLE` (on stdin, statement logging off) and rewrites the secret file
**in place**, because a bind mount keeps the original inode. It then proves
the new password with a TCP login. No restart; open connections continue.
The API's metrics credential file is re-read on change. Operator tokens are
still read by the publisher at start: rotating them recreates the
publisher, and a running publication is recorded `interrupted` and retried.
Making the operator API reload would be a later refinement; restart-based
rotation was already tested.

### Capacity tooling and the Iran region

`karta-load` (in the importer image) generates a repeatable mix of tile,
search, manifest and style requests, closed-loop or at a fixed open-loop
rate, pinned to a release, and reports per-kind percentiles, errors and
late starts. `scripts/capacity-run.sh` wraps it with an environment record
and container sampling. `karta region-draft` writes a region file whose box
is exactly the snapshot's header or verified sidecar box, with the digest
pinned and **empty** acceptance checks. Iran's checks must come from a
measured Iran import, not from Chitgar.

## Consequences and limits

* Nothing here is evidence about the target host. Gates 1–5 stay open for
  tier D, and the online half of gates 2, 4 and 5 stays open until a
  production source, signer and custodian are chosen and tested there.
* Every shipped number is provisional: the deadline, the alert guards, the
  memory, storage and cache defaults.
* Backups need the same PostgreSQL major version and PostGIS build to
  restore, and Docker access on the host to take and restore.
* The ODbL derivative-database question for distributed databases or
  backups is raised with the owner (docs/licenses.md), not decided.
