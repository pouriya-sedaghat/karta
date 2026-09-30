# ADR 0003: Stage 2, safe manual publication

Status: proposed with the Stage 2 PR, 2026-09-29. Builds on
[ADR 0001](../architecture.md) and [ADR 0002](0002-stage1-implementation.md).
It records a stored-data migration (registry schema version 2) and additive
public API changes, as ADR 0001 requires before either.

## Context

Stage 1 served one release: an import activated itself only when nothing was
active, replacing data needed `make reset` (downtime), and
`release.Manager.swap` closed the previous release's pool immediately (the
design gate recorded in ADR 0002). Stage 2 must publish new snapshots from a
local inbox while serving, keep pinned clients consistent, roll back, clean
up, audit operator actions and recover from interruption. Online downloads
are Stage 3.

## Decisions

### A separate publisher process with its own operator listener

`karta publisher` (the importer image, UID 10001) watches the inbox, builds
and activates releases, runs retention cleanup and serves the operator API on
its own port (8081, published on 127.0.0.1 only). The public API process is
unchanged in privilege: it has no operator routes, keeps the read-only
`karta_api` role, and cannot write the registry. A compromise of the public
API therefore cannot publish, roll back or delete. The publisher network
(`operator`) exists only to publish the operator port and does not
masquerade, so the process that parses untrusted snapshots has no NAT route
out. Rejected: operator endpoints in the public API behind a path prefix (one
process would hold both the read-only and the importer credentials, and the
public listener would parse operator bodies).

`karta import` remains, as a command-line front end to the same publication
path (same staging, validation, policy, activation and audit); its exit codes
gained 4 (policy refusal), 6 (storage) and 7 (busy).

### Inbox completion protocol and private staging

A submission is `NAME.osm.pbf`, an optional `NAME.osm.pbf.provenance.json`
and a marker `NAME.osm.pbf.ready`, written last, holding the snapshot's
SHA-256 (the `sha256sum` line format is accepted). Producers write under
temporary names and rename (`scripts/submit.sh`). Until the marker exists the
publisher only lists and lstat's the files; it never opens them (the
integration test proves this with an unreadable snapshot). The files must
then keep their size, modification time and inode for a settle interval. The
snapshot is copied with `O_NOFOLLOW|O_NONBLOCK` into a private 0700 staging
directory while being hashed; the copy is used only if the source was not
replaced or modified during the copy and its digest equals the marker's.
Everything after that reads the staged copy, and the importer re-hashes it
after osm2pgsql, so the bytes validated are the bytes imported.

The inbox is mounted read-only; outcomes are recorded in the registry keyed
by the submission's fingerprint (names, sizes, mtimes, inodes), so a rescan
after a restart never processes the same files twice, and touching the
marker is an explicit re-submission. Rejected: inotify (events are lost
across restarts; a periodic rescan is needed anyway) and "wait for a stable
size" without a marker (a stalled copy looks stable).

### Complete validation, and digest authorization that cannot be switched off

A PBF is scanned completely (every blob framed, decoded, and structurally
walked; the last blob must end at EOF; history files and unknown required
features are refused); a file cut exactly between two blobs is structurally
valid and is caught by its digest. Provenance sidecars need their digest,
size and license, a consistent header box and timestamps; the source claim
(`source`, `source_sha256`) is kept as a claim and never reported as
verified. The data timestamp comes from the provenance source header or the
PBF header, never from file times; the two must agree when both exist, and it
must lie between 2004-08-09 and now plus `KARTA_MAX_FUTURE_SKEW`.

A snapshot is accepted only if its SHA-256 is pinned in the region file
(`expected_sha256` or the new `allowed_sha256`) or an operator authorized it
through the audited `POST /v1/operator/authorizations` (optionally bound to a
size and an expiry). **Behaviour change:** in Stage 1 a region without
`expected_sha256` accepted any file; now it accepts nothing until a digest is
authorized. There is no setting that accepts arbitrary files, and a newer
snapshot is not accepted because it is newer. The committed fixture region
pins its three committed files. An `unauthorized_digest` rejection is
re-evaluated automatically once exactly that digest is authorized.

### Publication policy

Forward only: a snapshot replaces the active release only if it is for the
same region and strictly newer by data timestamp, or the same snapshot
(digest) rebuilt, for example after a toolchain change. Older snapshots and
different snapshots with the same timestamp are refused before any build.
Duplicates never change the active pointer: a snapshot whose release is
active is a no-op; one whose release is retired is refused (roll back to it
explicitly); a validated release that was never activated (an interrupted
publication) is activated when submitted again. Changing the served region
needs `--allow-region-change` (CLI) or `allow_region_change` (operator API).
Submissions are processed one at a time in name order, so the newest valid
snapshot ends up active whatever the order.

A relative gate complements the absolute `min_counts`: with
`validation.max_drop_fraction`, every counted table must keep at least
`1 - fraction` of the active release's rows (0.5 for the fixture, 0.3 for
Chitgar). Like other thresholds it is not part of the release id. The gate
fails closed: the active release's counts come from the registry or, for a
Stage 1 release, from the import report in its database; if neither can be
read or decoded the publication fails (`counts_unavailable`) before any
build. A release validated against one active release may be activated
later, after the pointer moved (an import that finished after a rollback, a
manual-activation build, a resubmitted ready release). So every forward
switch, publication or operator `activate`, applies the gate again inside
the pointer transaction against the release active there, after the
compare-and-swap check: a switch that would lose too much data relative to
that release is refused (`excessive_data_loss`), and counts that cannot be
read refuse it too. Rollback stays an explicit operation with its own
policy (region and compatibility, no count gate).

### One pointer transaction, compare-and-swap, no import lock

Builds are serialized by a session advisory lock (as in Stage 1). The active
pointer changes only in `registry.Activate`: one transaction takes a
transaction-scoped advisory lock (bounded by `lock_timeout`), checks the
expected active release, checks the target is `ready` or `retired`, applies
the caller's policy, retires the previous release with
`pinned_until = now() + pin grace`, moves the pointer and writes the audit
record. Operator switches do not take the build lock, so a rollback is never
blocked by an import; a publication compares against the active release it
saw when its build started, so an import finishing after a rollback stays
`ready` instead of undoing it. Of two concurrent switches with the same
expectation, one wins and the other gets `409 active_release_changed`.

### Pinning, grace and drain in the API

The release manager serves the active release and every retired release
whose `pinned_until` has not passed; it reads both from the registry every
poll interval, so the grace survives API restarts and is the same for every
API instance. A release that stops being served is removed from lookup at
once, but its pool is closed only after a drain period
(`KARTA_REQUEST_TIMEOUT` + 5 s), so a request that resolved it finishes on
it. Lookups distinguish served, expired or removed (`410 release_expired`),
unloadable (`503`) and unknown (`404`) ids. Rejected: reference counting per
request (the drain period gives the same guarantee without touching every
handler).

### Retention and cleanup

Cleanup keeps the active release, the `KARTA_RETAIN_RELEASES` (default 2)
most recent other validated releases, and every release still inside its pin
grace plus `KARTA_CLEANUP_MARGIN` (longer than the API drain). It skips any
release with a database session, marks the release `removing` under a row
lock (so no rollback can pick it), drops the database without `FORCE` (the
drop fails if a session appears), then marks it `removed`. It runs after each
publication, every `KARTA_CLEANUP_INTERVAL`, and on operator request (with a
dry run). Every removal is audited; an operator's cleanup request is also
audited as a whole with its actual result: `failed` when it ends with an
error (with the error and what it removed before), `noop` for a dry run
(with what it would remove) or when nothing was removed, `succeeded`
otherwise.

### Recovery

At every publisher start and before every CLI import, under the build lock:
drop leftover candidate databases, mark releases stuck `importing` or
`validating` as `failed` and drop their databases, finish `removing`
releases, drop release databases no retained release references, mark
submissions left `processing` as `interrupted` (retried up to
`KARTA_PUBLISH_MAX_ATTEMPTS`), and empty staging. The active pointer is never
touched: it only ever moves in a committed transaction. A build failure
caused by the database going away (connection loss, restart) is an
interruption and retried, not a final failure. Failpoints
(`KARTA_FAILPOINTS`, test-only, logged loudly) stop the process at each
critical transition so tests can prove this.

### Capacity

Before a build: staging must have the snapshot plus `KARTA_STAGING_RESERVE_MB`
free; release databases (active, retained, leftover candidates) plus a
candidate estimate (`max(1.25 × the largest release of the region,
KARTA_CANDIDATE_SIZE_FACTOR × snapshot + 32 MiB)`) must fit
`KARTA_RELEASE_STORAGE_BUDGET_MB`; if `KARTA_DB_VOLUME_PATH` is mounted, its
free space must exceed the estimate plus `KARTA_MIN_FREE_MB`. A disk that
fills anyway fails the build (PostgreSQL `53100` or ENOSPC), drops the
candidate and leaves serving untouched. `KARTA_RELEASE_TABLESPACE` can put
release databases on their own volume.

### Serving-toolchain drift

ADR 0002 left one gap: tiles and normalized search terms are computed on the
serving PostgreSQL, whose image could be upgraded without re-importing. Now
the API refuses to serve a release unless the running PostgreSQL, PostGIS,
GEOS, PROJ, pg_trgm and ICU versions equal the ones recorded in the release
(`release_info.toolchain`), checked when the release is loaded and on every
new connection to its database (pgx `AfterConnect`); the publisher applies
the same check before a rollback or activation. ICU is now read with
`pg_collation_actual_version` (the loaded library), which equals the catalog
value Stage 1 recorded on an unchanged image, so existing release ids are
unchanged (checked: fixture snapshots A and B get the same ids from the
Stage 1 and Stage 2 importers). After a database image upgrade the release is
refused (`release_incompatible`) until the snapshot is published again, which
builds a new release id on the new toolchain. Immutable tile URLs therefore
stay valid across database image upgrades: they are served only by the
toolchain that produced them, and never by another.

### State names

ADR 0001 sketched `discovered`, `staging`, `validating`, `ready`, `active`,
`failed`, `retired`. They are implemented as submission states (`processing`
covers discovery, staging and verification; outcomes `published`, `ready`,
`duplicate`, `rejected`, `failed`, `interrupted`) and release states
(`importing`, `validating`, `ready`, `active`, `retired`, `failed`, plus
`removing` and `removed` for cleanup), so a rejected file never creates a
release row.

## Registry schema version 2 (stored-data migration)

`registry.Migrate` now keeps `registry.schema_migrations` and applies
numbered migrations in one transaction under an advisory lock, so the
publisher and CLI imports can start concurrently. Version 1 is the Stage 1
schema, written idempotently, so a Stage 1 registry (which has no
migrations table) is recognised and upgraded in place. Version 2:

* `releases`: states `retired`, `removing`, `removed` allowed; columns
  `source_size`, `schema_major`, `database_bytes`, `counts`, `submission_id`,
  `activated_at` (backfilled from `active_release`), `deactivated_at`,
  `pinned_until`, `removed_at`.
* `active_release.activated_by`.
* New `submissions`, `authorizations` (one open authorization per region and
  digest) and `audit` (append-only: update, delete and truncate raise). The
  Stage 1 `events` rows are copied into `audit`; `events` is kept unchanged
  and no longer written.
* `karta_reader` may read `schema_migrations`; it still cannot read
  submissions, authorizations or audit.

Release databases are unchanged (no migration, no re-import). The API reads
a version 1 registry as before (active release only) until a Stage 2 process
migrates it. Rollback of the code: a Stage 1 API reads a version 2 registry
correctly (it only reads the active pointer); a Stage 1 importer refuses
region files that use `allowed_sha256` (strict parsing), which is the safe
outcome.

## Public API changes (additive)

* `410 release_expired` on release-pinned paths (tiles, styles, `style.json`,
  search with `release_id`) for a published release that is no longer served;
  `503` with `Retry-After` for a pinned release that cannot be loaded.
* Manifest: `capabilities.manual_updates` is now `true`, new
  `capabilities.release_pinning`, `freshness.update_mode` is `manual` (new
  enum value), new `freshness.activated_at`.
* The demo treats `release_expired` like `unknown_release` and reloads
  when a pinned search expires.

Unchanged: map and search behaviour, cache headers, release id derivation,
style URLs, error bodies.

## Test fixtures

Two committed, deterministic PBF snapshots (A from the Stage 1 fixture, B a
later edit) are generated from XML sources by `cmd/karta-fixture` with raw
(uncompressed) blobs, so the bytes do not depend on a zlib implementation;
`make lint` and a unit test check they match their sources and the region
pins. CI uses only these and variants the tests generate from them.

## Consequences and limits

* One region per deployment (one active pointer), one publisher per inbox
  and staging directory (enforced with a lock file), one host.
* The operator API is plain HTTP on loopback; exposing it elsewhere needs TLS
  in front. Tokens are static bearer tokens (hashes on the server); rotation
  restarts the publisher.
* The pin grace is time-based; a client pinned longer than the grace gets
  `410` and must refetch the manifest.
* The API picks up a switch within `KARTA_RELEASE_POLL_INTERVAL` (5 s): for
  that long after a switch the manifest may still name the previous release.
