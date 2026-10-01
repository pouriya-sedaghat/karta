# Karta runbook (Stage 3)

Karta serves one **active release** of one region: vector tiles, a MapLibre
style with local glyphs, named-place/POI search and a demo page, all from the
local host. An operator publishes new releases **while serving** by placing
snapshots in a local inbox (or with the command-line importer); a release is
built in an isolated candidate database, validated, and activated in one
registry transaction. Replaced releases stay servable to pinned clients for a
grace period and are kept for rollback; old ones are cleaned up. Optionally
(off by default), a **fetcher** polls a configured HTTPS source for signed
snapshot manifests and delivers newer verified snapshots to the publisher,
which publishes them through the same path ("Online updates" below).

Requirements: Docker Engine with Compose v2.24+ (for `!reset`/`!override` in
the overlay files), GNU make, `curl`. Go 1.27+ only for tests and
development. Images are built locally; after `make build` nothing is fetched
from the network by any service.

## Services

| Service | Image, user | Role | Network, port |
| --- | --- | --- | --- |
| `db` | PostGIS 18-3.6 | registry + one database per release | `backend` (internal), no port |
| `api` | distroless, UID 65532, read-only DB role | public read API, `/demo/` | `backend` + `frontend`, 127.0.0.1:8080 |
| `publisher` | importer image, UID 10001 | inbox watcher, builds, activation, cleanup, **operator API** | `backend` + `operator` (no NAT), 127.0.0.1:8081 |
| `fetcher` | api image, UID 65532 | **opt-in** (`make up-online`): polls the online source, verifies signed manifests, downloads, delivers to the `online` volume; no database access | `egress` only, no port |
| `operator-cli` | api image, UID 65532 | one-off operator API client (`make op`), holds the raw operator token | `operator` |
| `importer` | importer image, UID 10001 | one-off command-line publication (`make import-*`) | `backend` |

## Bootstrap

```bash
make secrets        # random DB passwords and operator tokens in ./secrets (0700), never overwritten
make build          # karta-api:local (distroless, 25 MB) and karta-importer:local
make up             # PostgreSQL (waits until healthy), then the API and the publisher
curl -s http://localhost:8080/health/ready
# 503 {"status":"not_ready","reason":"no_active_release",...}   <- expected before the first publication
make op-status      # the publisher's view: region, policy, releases, submissions, storage
```

On a network that intercepts TLS, pass its CA bundle to the build:
`make build KARTA_BUILD_CA_FILE=/path/ca.pem` (used only as a build secret).

`db` initialisation (first start only) runs `deploy/postgres/initdb/10-karta.sh`:
roles `karta_reader` (NOLOGIN), `karta_api` (read-only sessions, 5 s statement
timeout), `karta_importer` (CREATEDB, not superuser), the PostGIS/pg_trgm
template `karta_template` and the registry database `karta_registry`. The
publisher (or the first command-line import) creates and migrates the
registry schema (version 2, docs/adr/0003-stage2-publication.md), including an
existing Stage 1 registry, in place.

The publisher serves the region in `KARTA_PUBLISH_REGION` (a file name in
`config/regions/`, default `tehran-chitgar`); set it in `.env` (for example
`KARTA_PUBLISH_REGION=fixture`) and `make up` again to change it.

## Publish a snapshot (the inbox)

The inbox is `./data/inbox` (`KARTA_INBOX_HOST_DIR` overrides it), mounted
read-only into the publisher. A submission named `NAME` is:

| File | |
| --- | --- |
| `NAME.osm.pbf` | the snapshot (OSM PBF only; `NAME` matches `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`) |
| `NAME.osm.pbf.provenance.json` | optional provenance sidecar (required by regions with `require_provenance`) |
| `NAME.osm.pbf.ready` | **completion marker, written last**: the snapshot's SHA-256 (64 hex digits), optionally followed by the file name, i.e. a `sha256sum` line |

**Completion protocol.** Copy each file under a temporary name that does not
end in these suffixes (a leading dot, or `.part`), rename it into place, and
create the marker last, also by rename. `scripts/submit.sh` does exactly this:

```bash
make publish SNAPSHOT=/path/to/snapshot.osm.pbf [NAME=name]     # or: scripts/submit.sh FILE [INBOX] [NAME]
EXPECTED_SHA256=<hex> scripts/submit.sh FILE                    # refuses a file with another digest
```

What the publisher does, every `KARTA_INBOX_POLL_INTERVAL` (10 s):

1. Lists the inbox. A submission without a marker is shown as
   `waiting_for_ready_marker` and **never opened**; files not following the
   naming rules are ignored; hidden files are the producer's business.
2. Waits until all of the submission's files have kept their size, mtime and
   inode for `KARTA_INBOX_SETTLE` (5 s).
3. Copies the snapshot and sidecar into private staging (never through a
   symlink; directories, devices and FIFOs are rejected without being
   opened) and hashes the copy; the source must not change during the copy
   and the digest must equal the marker's.
4. Verifies the **staged copy**: the whole PBF structure, the region box
   (PBF header or sidecar), the provenance sidecar, a trustworthy data
   timestamp, and that the digest is **pinned or authorized** (next section).
5. Refuses snapshots older than the active release (see "Rules").
6. Checks storage, then builds and validates a candidate release database
   while the API keeps serving the active release.
7. Activates it in one registry transaction (unless
   `KARTA_PUBLISH_AUTO_ACTIVATE=false`: then it stays `ready` for
   `make op CMD='activate --release ID --reason ...'`), and runs retention
   cleanup. The API switches within `KARTA_RELEASE_POLL_INTERVAL` (5 s).

Follow it with `make op-status` (submissions show `state`, `reason_code` and
`reason`) and `make logs`. Outcomes are recorded per exact set of files: the
same unchanged files are never processed again, even after a restart.
**To submit the same files again** (after authorizing a digest, freeing disk
space, or a failed build), touch the marker: `touch data/inbox/NAME.osm.pbf.ready`.
Processed files stay in the inbox (the publisher cannot write there); remove
them when their outcome is recorded.

| Submission state | Meaning |
| --- | --- |
| `published` | built, validated and activated |
| `ready` | built and validated, not activated: `manual_activation`, `active_changed` (an operator switched releases during the build), or `excessive_data_loss` (a resubmitted ready release would lose too much data relative to the release active now) |
| `duplicate` | `duplicate_active` (already active: nothing to do) or `duplicate_retained` (exists as a retained release: roll back or activate it instead) |
| `rejected` | the input failed a check; nothing was built |
| `failed` | the build or validation failed (the candidate was dropped), or the active release's row counts could not be read (`counts_unavailable`: nothing was built) |
| `interrupted` | the process or database stopped during it; retried automatically up to `KARTA_PUBLISH_MAX_ATTEMPTS` (3) times |

| Reason code | Cause |
| --- | --- |
| `symlink`, `not_regular_file`, `invalid_name` | the submission's files are not plain regular files with a valid name |
| `invalid_marker`, `marker_digest_mismatch` | the marker is malformed, or the file is not the one the marker describes (incomplete or changed copy) |
| `changed_during_copy`, `empty_file`, `too_large` | the file changed while staged, is empty, or exceeds `KARTA_MAX_INPUT_MB` |
| `malformed_snapshot` | truncated, padded or corrupt PBF, unsupported compression or features (history files) |
| `region_mismatch` | the snapshot's box (header or sidecar) is not the region's box |
| `provenance_required`, `provenance_invalid` | missing sidecar, or its digest, size, license, box or timestamps do not match |
| `timestamp_missing`, `timestamp_untrusted` | no data timestamp in the sidecar or header, the two disagree, or it is in the future or before 2004 |
| `unauthorized_digest` | the SHA-256 is neither pinned in the region file nor authorized by an operator |
| `older_than_active`, `not_newer`, `region_changed` | the rules below |
| `insufficient_storage` | not enough space for staging, the storage budget or the database volume, or the disk filled during the build |
| `validation_failed`, `build_failed` | the candidate failed a region check (counts, relative drop, tiles, searches) or osm2pgsql/SQL failed |
| `counts_unavailable` | `validation.max_drop_fraction` is set and the active release of the region has no readable row counts (not in the registry, and not decodable from the import report in its database, as for a damaged Stage 1 release); the relative gate is never skipped, so nothing is built. Check the active release's database, or roll back to a release with counts, then touch the marker |
| `excessive_data_loss` | activating the release would drop more rows than `validation.max_drop_fraction` allows relative to the release active at the switch |

### Rules

* **Forward only.** A snapshot replaces the active release only if it is for
  the same region and has a **newer data timestamp** (from the provenance
  source header or the PBF header; file times are never used), or is the
  **same snapshot** (same SHA-256) rebuilt, for example after a database
  image upgrade. Older snapshots (`older_than_active`) and different
  snapshots with the same timestamp (`not_newer`) are refused before any
  build; going back is a rollback.
* **Duplicates never switch.** A snapshot whose release is active is a no-op;
  one whose release is retired is refused (roll back to it explicitly).
* **No silent data loss.** With `validation.max_drop_fraction`, a new
  release must keep at least `1 - fraction` of every counted table's rows of
  the active release of the same region. It is checked when the candidate
  is validated and again, inside the pointer transaction, against the
  release active at the moment of every forward switch (publication or
  `activate`): a release kept `ready` while someone rolled back to a
  release with more data is refused (`excessive_data_loss`). If the active
  release's counts cannot be read, the publication or activation is refused
  (`counts_unavailable`), never let through. Rollback has its own policy and
  no count gate. If a large drop is intended, raise `max_drop_fraction` in
  the region file through a reviewed change.
* **One at a time, in name order.** Builds are serialized; with several
  ready submissions the newest valid one ends up active regardless of order.
* **Region changes are explicit**: `make import-tehran IMPORT_FLAGS=--allow-region-change`,
  or `allow_region_change` on an operator activation or rollback.

## Authorize a new snapshot

Every snapshot's SHA-256 must be **pinned** in the region file or
**authorized** by an operator; there is no setting that accepts arbitrary or
merely newer files. `config/regions/tehran-chitgar.json` pins exactly the
2026-09-27 Chitgar extract; a newer extract is refused (`unauthorized_digest`)
until you authorize it:

1. Obtain the new extract and verify it **out of band**: record where it came
   from, check its SHA-256 against the provider's published checksum or your
   own extraction record (`scripts/extract_tehran.py` writes the sidecar),
   and inspect it (`osmium fileinfo -e`).
2. Authorize exactly that digest and size (the reason is audited):

   ```bash
   make op CMD='authorize --sha256 <64 hex> --size <bytes> --reason "Chitgar 2026-10-04, checked against extraction log X"'
   # optional: --expires 2026-10-31T00:00:00Z
   ```

3. Submit it (`make publish-tehran` for the development file, or
   `make publish SNAPSHOT=...`). A submission already rejected as
   `unauthorized_digest` is re-evaluated automatically on the next scan.

Revoke an authorization that was not used:
`make op CMD='revoke --sha256 <hex> --reason "..."'` (built releases stay).
Alternatively, pin the digest permanently by adding it to `allowed_sha256` in
the region file through a reviewed change; the publisher reads the region file
for every submission.

## Online updates (opt-in)

Online updates are **off** unless you configure them. No production source
is configured: choosing the HTTPS provider, who holds the signing key and the
permitted update lag are owner decisions (docs/adr/0004-stage3-online-updates.md).
When enabled, manual publication through the inbox and the command line
keeps working unchanged, also when the source or the network is down.

### How it works

1. The source publishes, next to each snapshot (and optional provenance
   sidecar), a **signed manifest**: a DSSE envelope with an Ed25519
   signature over the region id and box, a serial number that only grows, a
   validity window, and the snapshot's exact URL, SHA-256, size and data
   timestamp (and the sidecar's URL, SHA-256 and size).
2. The `fetcher` checks the manifest every `poll_interval`: the signature
   must verify with a key pinned in the **source file**, the region and box
   must be this deployment's, the serial must not go back, and the manifest
   must be inside its validity window. Only then does it download, over
   verified TLS, without redirects, within the size and time limits, into a
   private partial file; an interrupted download resumes only if the bytes
   still match. A complete file whose SHA-256 is not the signed one is
   discarded.
3. It hands the verified files to the publisher through the `online` volume
   with the inbox completion protocol (snapshot, sidecar, the exact signed
   manifest, then the ready marker).
4. The publisher verifies the signed manifest **again** on its own staged
   copy (the fetcher is not trusted), records the serial, checks that the
   bytes are the signed ones and that the signed data timestamp is the one
   the snapshot carries, and then publishes exactly like an inbox
   submission: Stage 2 verification, forward rule, storage checks, isolated
   build, validation, one audited pointer transaction. Serving never stops.

The signature authorizes the snapshot's digest, so no per-snapshot operator
authorization is needed. To keep that step anyway, set
`"require_operator_authorization": true` in the source file.

### Enable

1. Write a source file `config/sources/NAME.json` (format:
   `config/sources/README.md`) with the provider's manifest URL and the
   public key(s) you trust, and have it reviewed like any trust change.
2. In `.env`: `KARTA_ONLINE_SOURCE_FILE=/config/sources/NAME.json` and,
   once the permitted lag is decided, `KARTA_DATA_STALE_AFTER=48h` (for
   example).
3. `make up-online` (starts the fetcher too; `make up` does not).
4. `make op-status`: `online.enabled` is true, `online.source` lists the
   trusted keys, and `online.fetcher.state` shows the checks.

To turn it off again, remove `KARTA_ONLINE_SOURCE_FILE`, stop the fetcher
(`docker compose --profile online stop fetcher`) and `make up`.

### Producing signed manifests

On the host where snapshots are produced (never in the Karta deployment):

```bash
go run ./cmd/karta-sign keygen --out owner-2026a.pem          # prints the public key for trusted_keys
go run ./cmd/karta-sign sign --signer owner-2026a=owner-2026a.pem \
    --region config/regions/tehran-chitgar.json \
    --snapshot tehran-chitgar.osm.pbf --snapshot-url tehran-chitgar-20261001.osm.pbf \
    --serial 42 --valid-for 168h --out manifest.json
go run ./cmd/karta-sign verify --source config/sources/NAME.json --region config/regions/tehran-chitgar.json manifest.json
```

`sign` runs Karta's own input verification on the snapshot first (complete
PBF scan, region box, provenance sidecar, data timestamp), so it never signs
a file Karta would refuse; it signs the provenance sidecar next to the
snapshot automatically. Publish the snapshot and sidecar first and the
manifest last; increase `--serial` for every manifest. Keys are PKCS#8 PEM,
interchangeable with OpenSSL (`openssl genpkey -algorithm ed25519`); the
public key for the source file is
`openssl pkey -in key.pem -pubout -outform DER | tail -c 32 | base64`.

### Rotate or revoke a signing key

1. Add the new public key to `trusted_keys` (reviewed change; the fetcher and
   publisher read the source file for every check and delivery, no restart).
2. Sign with both keys during the overlap (`--signer old=... --signer new=...`).
3. Sign with the new key only, then remove the old key, or set its
   `not_after` in advance.

If a key may be compromised: remove it from the source file at once,
`make op CMD='online-pause --reason "key compromise"'`, roll back if a bad
release was activated (a rollback also pauses), and have the producer
publish a manifest with a higher serial signed by a trusted key.

Removing a key, or a manifest expiring, also stops a build that is already
running from going live: the switch verifies the signed manifest again. Such a
submission ends `rejected` with the verification code (`signature_untrusted`,
`manifest_expired`, ...; the reason says "at activation"), the active release
is unchanged, and the validated release stays `ready`. Review it and
`activate` it, `online-retry` it (if the key was removed by mistake and is
back), or have the provider publish a fresh manifest for the same snapshot:
the fetcher delivers it again without downloading, and it is activated.

### Pause, resume, retry

```bash
make op CMD='online-pause --reason "provider incident"'    # validated online snapshots stay ready, not activated
make op CMD='online-resume --reason "provider fixed"'      # applies to the next delivery
make op CMD='activate --release rXXXX --reason "..."'      # activate a release kept ready while paused
make op CMD='online-retry --reason "disk space freed"'     # one more attempt of the newest failed online delivery
```

**A rollback pauses automatic online activation** (audited as
`online_pause` with cause `rollback`), so a later online snapshot cannot
silently undo it; resume when the source has fixed the data. An online build
that was running when an operator switched or rolled back finishes as
`ready` (`active_changed`), never active.

### When manual and online snapshots meet

Builds run one at a time. In every publisher scan the inbox goes first, then
online deliveries (by serial). The Stage 2 rules decide: the newest data
timestamp ends up active, the same snapshot is a duplicate, and of two
different snapshots with the same timestamp the first one switched to (the
manual one within a scan) wins; the other is refused as `not_newer`.

### Freshness, status and metrics

* `make op-status`: `online.fetcher.state` (last check, last success, last
  error and its code, consecutive failures, **next attempt**, download
  progress; `online.fetcher.state_age_seconds` grows if the fetcher stopped),
  `online.verified` (the newest manifest the publisher itself verified),
  `online.policy` (paused or not) and `freshness` (active data age against
  `KARTA_DATA_STALE_AFTER`).
* `make op CMD=metrics` (or scrape `GET /v1/operator/metrics` with the
  monitoring token): `karta_active_data_age_seconds`, `karta_data_stale`,
  `karta_online_last_success_timestamp_seconds`,
  `karta_online_next_attempt_timestamp_seconds`,
  `karta_online_consecutive_failures`, `karta_online_last_error{code}`,
  `karta_online_fetcher_state_age_seconds`, `karta_online_verified_serial`,
  `karta_submissions{source,state}` and more.
* Alert on **data age** (`karta_data_stale`), not on check success: a
  source that answers but has nothing new, or an old release that keeps
  serving, does not make the data fresh. Also alert on a growing
  `karta_online_fetcher_state_age_seconds`.
* The public manifest says only `update_mode: online`, `stale` and
  `stale_after_seconds`; nothing about the source.

### Failure codes (fetcher `last_error.code`, online submission `reason_code`)

| Code | Meaning | Action |
| --- | --- | --- |
| `signature_untrusted`, `signature_invalid` | no signature by a trusted, valid key; or a signature claiming a trusted key does not verify | check the key ids and the source file; investigate a possible attack |
| `manifest_invalid`, `manifest_too_large` | not a well-formed manifest envelope or payload, or above 64 KiB | provider problem |
| `manifest_region_mismatch` | the manifest is for another region or box | wrong source file or provider path |
| `manifest_expired`, `manifest_not_yet_valid`, `manifest_validity_too_long` | outside its validity window, or longer than `max_manifest_validity` | provider must re-sign; check clocks |
| any verification code, reason "at activation" | the manifest was valid when the build started but no longer at the switch (expired, key removed or retired, source file changed); the release was built and kept `ready`, not activated | review and activate it, `online-retry`, or wait for a fresh manifest for the same snapshot |
| `manifest_replayed`, `manifest_conflict` | an older serial than one already verified; a different manifest under the same serial, or a signed data timestamp that disagrees with the snapshot | provider problem or attack |
| `url_refused`, `destination_refused`, `redirect_refused` | a URL that is not https on an allowed host (or carries credentials or a query); an address that is not public and not in `allowed_networks`; a redirect | fix the source file or ask the provider for direct URLs |
| `http_status`, `network_error`, `tls_error`, `timeout`, `stalled`, `truncated` | the transfer failed | retried with backoff; persistent: check the network and the provider |
| `too_large`, `size_mismatch`, `digest_mismatch`, `unexpected_encoding` | the bytes are not the signed ones | retried; the bad bytes are discarded |
| `insufficient_storage` | the outbox (fetcher) or staging and release storage (publisher) is full | free space; `online-retry` for a refused delivery |
| `download_abandoned` | the same snapshot failed `max_download_attempts` times; set aside for `abandon_for` | fix the cause; a different snapshot is tried at once |
| `source_config` | the source or region file is invalid or names another region | fix the file |
| `manifest_missing`, `digest_mismatch` (publisher) | a delivery without a signed manifest, or with other bytes | the outbox was tampered with or the fetcher is broken |
| `online_activation_paused` | built and validated while automatic activation is paused | resume, or activate it |

All Stage 2 reason codes (forward rule, validation, storage) apply to online
submissions as well.

## Operator API

The operator API is served by the publisher on `127.0.0.1:8081`
(`KARTA_OPERATOR_BIND`, `KARTA_OPERATOR_PORT`), never by the public API. The
contract is [`openapi/operator.yaml`](../openapi/operator.yaml). Every
request needs a bearer token; every action needs a `reason` and is audited
with the credential name.

```bash
make op-status                                           # status (monitor or operator token)
make op CMD='audit --limit 50'                           # newest audit records
make op CMD='rollback --reason "B has broken labels"'    # to the most recently replaced release
make op CMD='rollback --release rXXXX --reason "..."'    # to a specific retained release
make op CMD='activate --release rXXXX --reason "..."'    # a ready release (forward only, row counts re-checked)
make op CMD='cleanup --dry-run --reason "check"'         # what cleanup would remove
make op CMD='cleanup --reason "free space"'
make op CMD=metrics                                      # Prometheus text: freshness, online source, submissions
make op CMD='online-pause --reason "..."'                # see "Online updates"
# the same over HTTP from the host:
curl -s -H "Authorization: Bearer $(cat secrets/operator_token)" http://127.0.0.1:8081/v1/operator/status
curl -s -X POST -H "Authorization: Bearer $(cat secrets/operator_token)" -H 'Content-Type: application/json' \
     -d '{"reason":"bad data","expected_active_release_id":"rYYYY"}' http://127.0.0.1:8081/v1/operator/rollback
```

`expected_active_release_id` (`--expected`) makes a switch conditional on the
release you looked at: if someone switched meanwhile you get
`409 active_release_changed` instead of undoing their change. Without it, the
release active when your request arrives is expected. `409 busy` means
another switch holds the pointer lock (retry after `Retry-After`); a running
import never blocks a rollback.

### Credentials

`make secrets` creates two random 256-bit tokens and the file the publisher
reads:

| File | Holder | Scopes |
| --- | --- | --- |
| `secrets/operator_token` | operators (`operator-cli`, curl) | `status`, `publish`, `rollback`, `cleanup` |
| `secrets/operator_monitor_token` | monitoring | `status` |
| `secrets/operator_tokens` | publisher | `NAME SCOPES SHA256(token)` per line: hashes only |

To add a credential (for example a second operator with only `rollback`),
generate a token (`od -An -N32 -tx1 /dev/urandom | tr -d ' \n'`), append
`name rollback,status <sha256 of the token>` to `secrets/operator_tokens`
(`scripts/gen-secrets.sh` rewrites that file, so keep extra lines in your
secret store and re-append them) and restart the publisher. Rotate the two
generated tokens with `make rotate-operator-tokens`. In production keep the
raw tokens on the operators' side only and give the publisher the hash file
through the orchestrator's secret store. Tokens are never logged; failed
authentication is logged and audited (at most 30 audit rows per minute).

## Rollback and pinned clients

* **Rollback** changes only the active pointer, in one audited transaction,
  to a retained (`ready` or `retired`), validated release that passes the
  same compatibility checks as serving (schema major, style, serving
  database toolchain). Removed, failed or incompatible releases are refused
  (`409`). Rollback is immediate; the API follows within 5 s.
* **Pinned clients.** When a release is replaced (publication or rollback),
  clients that pinned its `release_id` keep getting it for
  `KARTA_RELEASE_PIN_GRACE` (24 h); requests already running always finish on
  their release. After the grace period, or once cleanup removed it, pinned
  URLs answer `410 release_expired` and clients refetch the manifest (the
  demo does). The grace is recorded per release (`pinned_until`) in the
  registry, so API restarts and several API instances agree.
* After a rollback, a submission that was building finishes as `ready`
  (`active_changed`) instead of activating; activate it explicitly if wanted.

## Retention and cleanup

Cleanup keeps the active release, the `KARTA_RETAIN_RELEASES` (2) most
recent other validated releases (rollback targets), and every release inside
its pin grace plus `KARTA_CLEANUP_MARGIN` (5 min, longer than the API's
drain). It never removes a release while any database session uses it. It
runs after every publication, every `KARTA_CLEANUP_INTERVAL` (15 min) and on
request (`make op CMD='cleanup --reason ...'`, with `--dry-run` to preview).
Removed releases keep their registry row (`removed`) and their audit trail.
An operator's cleanup is audited with its actual result: `succeeded` (it
removed releases), `noop` (a dry run, whose record lists `would_remove`, or
nothing to remove) or `failed` (it ended with an error: `503`, the record's
`error` and the releases it had already `removed`). Rerun it after fixing
the cause; a release left `removing` is finished by the next cleanup or at
start.

## Recovery after interruption

Nothing needs to be done by hand after a crash, kill, host reboot or database
restart: restart the service (Compose does, `restart: unless-stopped`). At
every start the publisher (and every command-line import, first):

* drops leftover candidate databases and staging copies;
* marks releases stuck `importing`/`validating` as `failed` and drops their
  databases; finishes interrupted removals;
* drops release databases no retained release references;
* marks submissions left `processing` as `interrupted`; the inbox scan
  retries them (up to 3 attempts, then `failed`, `too_many_attempts`).

The active pointer only moves in a committed transaction, so after any
interruption it names either the previous release or the new one, never a
partial one; a publication interrupted after the switch committed is
reported as `duplicate_active` when retried. A build that fails because the
database went away is retried, not failed. The API keeps serving from its
loaded releases while the registry is unreachable.

## Storage

A release database is about 17 MB for the fixture and 34 MB for the Chitgar
sample (most of the fixture's is the PostGIS template); a publication needs
room for the active release, the retained releases and one candidate at the
same time. Checks before a build:

* staging (`staging` volume): the snapshot plus `KARTA_STAGING_RESERVE_MB` (64);
* `KARTA_RELEASE_STORAGE_BUDGET_MB` (10240): all release databases plus the
  candidate estimate (`max(1.25 × the region's largest release,
  KARTA_CANDIDATE_SIZE_FACTOR (40) × snapshot + 32 MiB)`);
* optional `KARTA_DB_VOLUME_PATH`: a path in the publisher container on the
  database volume (mount it read-only); its free space must exceed the
  estimate plus `KARTA_MIN_FREE_MB` (512).

If the disk still fills during a build, the build fails
(`insufficient_storage`), the candidate is dropped and serving continues;
free space and touch the marker. `KARTA_RELEASE_TABLESPACE` puts new release
databases in a PostgreSQL tablespace (created by the superuser, with `CREATE`
granted to `karta_importer`) on a separate volume, so imports cannot fill the
volume holding the registry. `make op-status` shows current use.

## Import the fixture

```bash
make -s import-fixture > fixture-import.json   # command-line publication; JSON result on stdout, logs on stderr
make wait-ready                                 # the API picks the release up within 5 s
make smoke
```

The fixture (`testdata/fixture/karta-fixture.osm`, CC0) imports in under a
second; its digest is pinned in `config/regions/fixture.json`, like the two
PBF snapshots A and B the publication tests use
(`testdata/fixture/snapshots/`). To publish those through the inbox instead,
start the publisher with `KARTA_PUBLISH_REGION=fixture` and
`make publish SNAPSHOT=testdata/fixture/snapshots/karta-fixture-a.osm.pbf`.

## Import the real Chitgar sample

The PBF and its sidecar are **not** in Git. Copy the two supplied files into
`data/local/` (see `docs/development-data.md`), then either publish them
through the inbox (the publisher's default region is `tehran-chitgar`):

```bash
make verify-tehran    # both files present, SHA-256 7d0e69a2…191e, world-readable (the publisher is UID 10001)
make up
make publish-tehran   # copies PBF + sidecar into data/inbox with the completion protocol
make op-status        # the submission becomes published; or follow `make logs`
make wait-ready
make test-browser-tehran   # renders lake, park/road and mall views; screenshots in artifacts/tehran/
```

or with the command-line importer (`make import-tehran`; add
`IMPORT_FLAGS=--allow-region-change` if another region's release is active,
for example the fixture). `config/regions/tehran-chitgar.json` pins the
snapshot digest and requires the provenance sidecar: a different file (even a
newer Geofabrik snapshot) is refused until authorized (above). Do not
substitute another snapshot silently.

## Command-line import (`karta import`) and exit codes

`make import-*` runs `karta import --snapshot FILE --region FILE` in the
importer container. It stages, verifies, builds and activates exactly like an
inbox submission and is audited as actor `cli`. Flags: `--no-activate`
(build and validate only; activate later), `--allow-region-change`,
`--provenance FILE`, `--reason TEXT`, `--actor NAME`, `--report FILE`,
`--keep-failed`.

| Code | Meaning | Active release |
| --- | --- | --- |
| 0 | published and activated; this exact release is already active; or built and ready (`--no-activate`) | new / unchanged |
| 2 | usage or configuration error | unchanged |
| 3 | input verification failed: digest not pinned or authorized, provenance, box, size, symlink, timestamp, malformed PBF | unchanged |
| 4 | refused by policy: older than the active release, not newer, another region, a retained duplicate, or the active release changed during the build (the release is left `ready`) | unchanged |
| 5 | release validation failed (counts, relative drop, tile contract, style, search or tile checks) | unchanged |
| 6 | insufficient storage (budget, staging, database volume, or the disk filled) | unchanged |
| 7 | another build holds the lock (`KARTA_IMPORT_LOCK_TIMEOUT`) | unchanged |
| 1 | other failure (database, osm2pgsql) | unchanged |
| 130 | interrupted | unchanged |

Each build runs in a working database `karta_c<random>`; the release id is
derived there (it includes the database toolchain versions) and the database
is renamed to `karta_<release_id>` only after every check passed. On failure
the candidate is dropped (`--keep-failed` keeps it for inspection until the
next build, which removes leftover candidates) and the registry records the
release as `failed` with the reason. Changing a region's name, box or default
view (by any amount), the provenance sidecar or the toolchain gives a new
release id; changing only its acceptance thresholds does not.

## Upgrading the database image

Tiles and normalized search terms are computed by the serving PostgreSQL.
The API serves a release only while the running PostgreSQL, PostGIS, GEOS,
PROJ, pg_trgm and ICU versions equal the ones recorded when it was built
(checked when a release is loaded and on every new connection), so an
immutable tile URL never serves bytes computed by other software. After an
upgrade that changes any of them, readiness reports `release_incompatible`
(`detail` names the components) until the snapshot is published again:

1. Plan a maintenance window: until step 3 the API answers `503`.
2. Upgrade the image (pinned by digest in `compose.yaml`) and `make up`.
3. Submit the active snapshot again (touch its ready marker in the inbox, or
   `make import-*`): the same snapshot rebuilt is allowed by the forward
   rule, gets a new release id (the toolchain is part of it) and is activated.
4. Retained releases built with the old toolchain cannot be rolled back to
   (`release_incompatible`); cleanup removes them over time.

## Normal operation

* `make up` / `make down` start and stop without losing data; the API reloads
  the active release (and pinned retired ones) on start; the publisher runs
  its recovery and resumes watching the inbox. Readiness (`/health/ready`) is 200 only with a
  loaded, compatible release and a reachable database; liveness
  (`/health/live`) reports the process only. The Compose health check uses
  readiness.
* Services restart `unless-stopped`; JSON logs are rotated (3 × 10 MB per
  container). Access logs contain method, path, status, bytes and duration,
  never query strings (search terms).
* If PostgreSQL goes away, readiness turns 503 `database_unavailable` and data
  requests return 503; the API recovers by itself when the database returns.
* Resource limits (override in `.env`): db 2 GiB / 2 CPUs, api 512 MiB / 1 CPU,
  publisher 4 GiB / 2 CPUs, importer 4 GiB, fetcher (online updates only)
  256 MiB / 0.5 CPU. Measured use is in the PR
  descriptions (Stage 1 for the Chitgar import, Stage 2 for publication).
* Upgrading the API image or changing `KARTA_PUBLIC_BASE_URL` needs no cache
  purge and no re-import: existing releases keep their ids and tile URLs, the
  style gets a new content-addressed URL (the manifest issues it), and style
  URLs issued before return `404 unknown_style` rather than different bytes.
  One exception applies only when upgrading from a build before `eab072b`
  (no external deployment is known, but none can be ruled out). Caches may
  keep that build's `style.json` responses for up to 24 hours, and those
  responses embed absolute tile and glyph URLs under the old
  `KARTA_PUBLIC_BASE_URL`. For those 24 hours:
  * keep the same release active, with no reset or re-import;
  * if the base URL changes, keep the old one reachable and serving tiles and
    glyphs as before, for example by routing it to the upgraded API;
  * purge `/v1/releases/*/style.json` in proxies or CDNs you operate.

  If the old base URL can't be kept, browser-cached copies fail to load tiles
  and glyphs until they expire. Purging a CDN does not clear browser caches.
  See `docs/api.md`, "Transition from builds before content-addressed styles".
* Upgrading the database image (PostgreSQL/PostGIS) is different: see
  "Upgrading the database image" above.

## Behind a reverse proxy with a path prefix

Karta can be published under a path, e.g. `https://example.com/maps/`. Set
`KARTA_PUBLIC_BASE_URL` to the public URL including the prefix, and have the
proxy strip the prefix before forwarding:

```nginx
location /maps/ {
    proxy_pass http://127.0.0.1:8080/;   # trailing slash: /maps/v1/… is sent as /v1/…
}
```

```bash
KARTA_PUBLIC_BASE_URL=https://example.com/maps make up
```

The manifest, style, tile and glyph URLs are absolute under the base URL, so
they carry the prefix. The demo calls the API relative to its own URL, and `/`
and `/demo` redirect with a relative `Location: demo/`. So
`https://example.com/maps/` opens the demo at `https://example.com/maps/demo/`.
Expose the prefix with its trailing slash; nginx redirects a bare `/maps` to
`/maps/` for such a location. `make test-browser-prefix` runs all browser
tests through such a proxy (see Development).

## Reset and cleanup

Removing individual releases is `make op CMD='cleanup ...'` (above). A
reset is only needed to start over:

```bash
make reset   # docker compose down -v: deletes ALL releases, the registry and its audit log (the pgdata and staging volumes)
make clean   # reset + remove images, web/dist, web/node_modules and artifacts/
```

Secrets in `./secrets` survive both; delete the directory to rotate the
database passwords together with a `make reset` (the roles are created only
at first initialisation). Operator tokens rotate without a reset
(`make rotate-operator-tokens`).

## Simulated disconnected run

```bash
make test-offline
```

Starts a separate project (`karta-offline`) whose API and database are only on
the internal Docker network: no published port and no route to any external
host. `scripts/check-isolated.sh` verifies this from the running API
container's configuration:

* it is attached only to `karta-offline_backend`, and that network is `internal`;
* it publishes no port and has no port binding;
* its network namespace has no IPv4 or IPv6 default route.

As a negative control, the target first attaches the API to an extra plain
bridge network, like `frontend` in `compose.yaml`, and requires the check to
fail. It then detaches the API and requires the check to pass. The fixture is
imported. Headless Chromium, whose only network is the API container (every
other host goes to a dead proxy), loads the demo, renders two views and runs a
search. It asserts that every request went to the API origin and that the
Persian presentation-form glyph ranges were served.

## Development without Docker for the API

```bash
make web                       # npm ci + copy MapLibre into web/dist (pinned, integrity-checked)
KARTA_PUBLIC_BASE_URL=http://localhost:8080 KARTA_DB_HOST=… KARTA_DB_PASSWORD_FILE=… \
KARTA_WEB_DIR=web/dist go run ./cmd/karta serve
make lint test                 # gofmt, vet, staticcheck, govulncheck, gosec, fixture check; unit tests
make test-integration          # isolated compose project on ports 18080/18081/18082/55433 (Stage 1, 2 and 3 suites; local controlled HTTPS source)
make test-integration RUN=TestPublication   # only the publication suite (RUN=TestOnline: only Stage 3)
make fixtures                  # regenerate testdata/fixture/snapshots/*.osm.pbf from their XML sources
make test-browser              # browser tests against the running stack (BASE_URL)
make test-browser-prefix       # the same through a proxy serving Karta under /maps, API restarted with that base URL
```

## Troubleshooting

| Symptom | Check |
| --- | --- |
| readiness `no_active_release` | nothing published yet, or every publication failed: `make op-status` (submissions) |
| readiness `release_incompatible` | the release's schema major, style layers, fonts or **serving database toolchain** do not match; `detail` names the problem; publish the snapshot again (see "Upgrading the database image") |
| a submission stays `waiting_for_ready_marker` | write the `.ready` marker (last), e.g. with `scripts/submit.sh` |
| submission `unauthorized_digest` / import exit 3 "refusing to import a different snapshot" | the file is not a pinned snapshot: restore the documented file, or verify and authorize the new one |
| submission `marker_digest_mismatch` | the copy is incomplete or the file changed: copy it again with the protocol and touch the marker |
| import exit 3 "permission denied", submission `io_error` | `chmod 0644` the snapshot and sidecar and `0755` the inbox (the publisher and importer run as UID 10001) |
| pinned client gets `410 release_expired` | its release was replaced more than `KARTA_RELEASE_PIN_GRACE` ago or removed: refetch the manifest |
| operator API `401` / `403` | wrong token file, or the credential lacks the scope; see `make op-status` with the operator token |
| operator API `409 busy` | another switch holds the pointer lock; retry after a few seconds |
| demo shows nothing, console CSP errors | open the demo at `KARTA_PUBLIC_BASE_URL` + `/demo/` (the page only talks to that origin) |
| `online.enabled` false | `KARTA_ONLINE_SOURCE_FILE` not set for the publisher; online updates are opt-in |
| `online.fetcher.error` "has not written its state" / growing `state_age_seconds` | the fetcher is not running: `make up-online`, `docker compose --profile online logs fetcher` |
| online submissions stay `ready` with `online_activation_paused` | automatic activation is paused (by an operator or a rollback): `online-resume`, or activate the release |
| fetcher `destination_refused` for a private mirror | add its network to the source file's `allowed_networks` |
