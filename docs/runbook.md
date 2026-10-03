# Karta runbook (Stage 5)

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
Stage 5 adds two more opt-in intake paths in front of the same publication
path: a **local intake** for outages (an authenticated command and a
protected-folder watcher, "Local intake") and a **controlled source bridge**
that turns Geofabrik's Iran extract into signed online updates
("Controlled source bridge"). Both are engineering features tested with
fixtures and local controlled services only.

Stage 4 adds the operational layer: a deadline for every publication,
metrics on a separate API listener, Prometheus alert rules with documented
responses, backup and restore, credential rotation that keeps data, and
capacity tooling. Commands are in "Operations" below; the reasoning, the
trust inventory and the response to every alert are in
[docs/operations.md](operations.md). Nothing here claims production
readiness: the target host, the Iran snapshot source and the service
objectives are owner decisions still open (docs/operations.md, "Owner
inputs").

Requirements: Docker Engine with Compose v2.24+ (for `!reset`/`!override` in
the overlay files), GNU make, `curl`. Go 1.27+ only for tests and
development. Images are built locally; after `make build` nothing is fetched
from the network by any service.

## Services

| Service | Image, user | Role | Network, port |
| --- | --- | --- | --- |
| `db` | PostGIS 18-3.6 | registry + one database per release | `backend` (internal), no port |
| `api` | distroless, UID 65532, read-only DB role | public read API, `/demo/`; metrics on a separate listener | `backend` + `frontend`, 127.0.0.1:8080 (API), 127.0.0.1:9464 (metrics, monitoring credential only) |
| `publisher` | importer image, UID 10001, `docker-init` as PID 1 | inbox watcher, builds, activation, cleanup, **operator API** | `backend` + `operator` (no NAT), 127.0.0.1:8081 |
| `fetcher` | api image, UID 65532 | **opt-in** (`make up-online`): polls the online source, verifies signed manifests, downloads, delivers to the `online` volume; no database access | `egress` only, no port |
| `operator-cli` | api image, UID 65532 | one-off operator API client (`make op`), holds the raw operator token | `operator` |
| `intake-watch` | api image, UID 65532 | **opt-in** (`make up-intake`): watches the protected landing area, hands complete deliveries to the publisher with its own `intake_watch` credential | `operator` (no NAT), no port |
| `intake-cli` | api image, UID 65532 | one-off authenticated intake command (`make intake-submit`), a person's `intake_submit` credential | `operator` |
| `bridge-acquire`, `bridge-sign`, `bridge-serve` | api image, UID 65532 | **opt-in** (`compose.bridge.yaml`, `make up-bridge`): the controlled source bridge | `bridge-egress` / none / `bridge` (no NAT), 127.0.0.1:8443 and :9465 |
| `importer` | importer image, UID 10001, `docker-init` | one-off command-line publication (`make import-*`), `restore-check`, `registry-summary`, `region-draft`, `karta-load` | `backend` |
| `postgres-exporter` | pinned upstream image | **opt-in** (`make up-monitoring`): PostgreSQL statistics as role `karta_monitor` | `backend` + `monitoring` (no NAT), 127.0.0.1:9187 |
| `prometheus` | pinned upstream image | **opt-in** (`make up-monitoring`): scrapes the three targets, evaluates the Karta alert rules | `backend` + `monitoring` (no NAT), 127.0.0.1:9090 |

## Bootstrap

```bash
make secrets        # random DB passwords, operator and monitoring tokens in ./secrets (0700), never overwritten
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

A `failed` submission with `publication_timeout` ran longer than
`KARTA_PUBLISH_TIMEOUT` and was stopped. It is final, not `interrupted`: it is
not retried automatically and does not use up attempts.

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
| `publication_timeout` | the whole publication (staging through validation) exceeded `KARTA_PUBLISH_TIMEOUT`; osm2pgsql and every process it started were stopped, every statement cancelled, the candidate dropped. The reason names the phase. Find the cause, raise the deadline if the build needs longer, then touch the marker (docs/operations.md, "Bounded publication") |
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
  `karta_submissions{source,state}`,
  `karta_submission_last_finished_timestamp_seconds{source,state}` and more.
* Alert on **data age** (`karta_data_stale`), not on check success: a
  source that answers but has nothing new, or an old release that keeps
  serving, does not make the data fresh. Also alert on a growing
  `karta_online_fetcher_state_age_seconds`.
* The public manifest says only `update_mode: online`, `stale` and
  `stale_after_seconds`; nothing about the source.

### The fetcher refuses to start: state cannot be read

The fetcher keeps its state in `.fetcher/state.json` inside its outbox. That
file records the highest manifest serial it verified, which may belong to a
manifest that was never delivered and so is unknown to the publisher. If the
file exists but cannot be read (damaged, an unsupported version, replaced by
a symlink or a directory), the fetcher logs "refusing to start" and exits
without contacting the source. It does not start from an empty state, which
would let an older but still valid manifest through.

* Restore the file from a backup if you have one, then start the fetcher.
* Otherwise, before moving it aside, note the serial the source serves now
  and `online.verified.serial` in `make op-status` (the newest serial the
  publisher verified). After the file is moved aside, the fetcher starts from
  the newest complete delivery in its outbox, and the publisher still refuses
  anything below the serial it verified. Only a serial that was verified but
  never delivered is forgotten.
* Only the fetcher writes this file: find out how it was damaged (disk,
  volume, or someone with write access to the outbox volume).

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

## Local intake (opt-in, Stage 5)

The local intake publishes a complete snapshot during an internet outage
without a signing key, a fetcher or a per-file approval *after* a deliberate
delivery: it establishes that the copy is complete from an **independent
digest**, copies the bytes into its own handoff directory, and creates an
authorization for **exactly that digest and size** through a narrow
credential. The publisher then stages, verifies, builds and activates it
like any manual submission and re-checks the authorization inside the
switch transaction ([ADR 0006](adr/0006-stage5-hybrid-intake.md)). Two
front ends share one implementation:

* the **command**, `karta intake submit` (`make intake-submit`): a named
  person with their own `intake_submit` credential submits one file and
  follows it to its outcome. Always available once the intake is enabled,
  also when the watcher is off or its preflight fails;
* the **watcher**, `karta intake watch` (`make up-intake`): watches a
  protected landing area for deliveries that carry a producer completion
  marker. Opt-in per host, only after its preflight passes. **While it
  runs, write access to the landing area is publication authority** for any
  snapshot that passes the common checks: keep that write access to the
  landing account and the people holding its keys.

The direct manual path (inbox with a pin or an operator authorization, or
`karta import`) stays available as an independent fallback.

### Enable the intake and the command

1. `.env`: `KARTA_INTAKE_DIR=/data/intake`, then `make up` (the publisher is
   recreated with the `intake` volume read-only). `make op-status` shows
   `intake.enabled: true` and the limits (`max_ttl_seconds`, `max_open`).
2. Give each person their own credential (scope `intake_submit` only):

   ```bash
   scripts/operator-credential.sh add alice intake_submit    # creates secrets/alice.token; give it to alice only
   scripts/operator-credential.sh list
   ```

   The running publisher picks it up at its next request; no restart.
3. Submit with an **independent expectation** from the source copy: its
   SHA-256 (and size) as recorded where the file came from, or the
   producer's completion marker:

   ```bash
   make intake-submit FILE=/media/usb/iran-2026-10-01.osm.pbf TOKEN=secrets/alice.token \
     SHA256=<64 hex of the source copy> SIZE=<bytes> REASON="outage delivery, digest from the download log"
   make intake-submit FILE=... TOKEN=... EXPECT=/media/usb/iran-2026-10-01.osm.pbf.complete
   make intake-submit FILE=... TOKEN=... ATTEST="copied directly from the verified source disk"   # last resort: the person's attestation, recorded
   ```

   The file (and its `.provenance.json` sidecar beside it, if any) must be
   world-readable: the command runs as UID 65532. It copies and hashes the
   file from wherever it is (a USB disk, a hypervisor share), refuses a
   mismatch **before** creating any authorization, hands it off and waits
   for the outcome (`NO_WAIT=1` returns after the handoff). Exit status: 0
   published (or already active), 2 usage or no expectation, 3 the input was
   refused (mismatch, region, size), 4 a publication outcome other than
   published (the JSON names the state and reason code), 130 interrupted
   (the publication continues), 1 other failures.

### Enable the watcher

1. **Landing area.** On the Karta host (or VM), a local POSIX filesystem
   (ext4, xfs, btrfs, zfs, f2fs, tmpfs). Not a hypervisor or SMB/NFS share,
   FUSE (virtiofs, vmhgfs), 9p/WSL drvfs, NTFS or FAT: the preflight refuses
   those, because another system decides who writes them.

   ```bash
   sudo useradd --system --create-home --shell /usr/sbin/nologin karta-landing   # SFTP only: restrict it in sshd_config
   sudo install -d -o karta-landing -g karta-landing -m 0755 /srv/karta/landing
   # per-person SSH keys in ~karta-landing/.ssh/authorized_keys; umask 022 for the account (files must be readable by UID 65532)
   make intake-check LANDING=/srv/karta/landing LANDING_UID=$(id -u karta-landing)
   ```

   `make intake-check` runs the preflight with the host root mounted
   read-only, so the host parents are checked too (owners, no write for
   others, no symlinks). It must print `"ok": true`. A group of writers is
   allowed only as `KARTA_INTAKE_WRITER_GID` (mode 0775).
2. `.env`: `KARTA_INTAKE_DIR=/data/intake`,
   `KARTA_INTAKE_LANDING_HOST_DIR=/srv/karta/landing`,
   `KARTA_INTAKE_LANDING_UID=<uid>` (and `KARTA_INTAKE_WRITER_GID`).
3. `make up-intake`: runs the check again, registers the watcher's own
   credential (`local-intake`, scope `intake_watch`, token in
   `secrets/intake_watch_token`, mounted only into the watcher) and starts
   it.
4. `make op-status`: `intake.watcher.state.preflight.ok` is true and
   `last_scan_at` advances every `KARTA_INTAKE_POLL_INTERVAL`.

The preflight runs before every scan; when it fails the watcher hands off
nothing and reports why (`KartaIntakePreflightFailing`). Use the command
meanwhile.

### Deliver to the landing area

A delivery is `NAME.osm.pbf`, an optional `NAME.osm.pbf.provenance.json`,
and the **completion marker** `NAME.osm.pbf.complete`, written **last**:

```json
{"format": "karta-delivery/1", "file": "NAME.osm.pbf", "sha256": "<64 hex>", "size_bytes": 229580914}
```

The digest and size come from the **source copy, before the transfer**.
Upload under a temporary name (a leading dot, `.part` or `.filepart`, which
the watcher ignores) and rename. A `.sha256` or `.md5` file is never a
completion signal: a checksum computed from a cut copy matches the cut copy.

From Linux, macOS or WSL (`scripts/deliver.sh`: hashes the source, uploads
with sftp or copies locally, renames, writes the marker last):

```bash
make deliver SNAPSHOT=iran-2026-10-01.osm.pbf DEST=sftp://karta-landing@karta-host/srv/karta/landing
```

From Windows 10/11 or Server 2019+ (PowerShell and the built-in OpenSSH
client, as the landing account with your own key):

```powershell
$f = "C:\data\iran-2026-10-01.osm.pbf"; $name = "iran-2026-10-01"
$sha = (Get-FileHash -Algorithm SHA256 -LiteralPath $f).Hash.ToLower()   # from the source copy, before the transfer
$len = (Get-Item -LiteralPath $f).Length
$marker = Join-Path $env:TEMP "$name.osm.pbf.complete"
[IO.File]::WriteAllText($marker, "{`"format`": `"karta-delivery/1`", `"file`": `"$name.osm.pbf`", `"sha256`": `"$sha`", `"size_bytes`": $len}")
$batch = Join-Path $env:TEMP "deliver-$name.sftp"
@"
cd /srv/karta/landing
put "$f" ".$name.osm.pbf.part"
rename ".$name.osm.pbf.part" "$name.osm.pbf"
put "$marker" ".$name.osm.pbf.complete.part"
rename ".$name.osm.pbf.complete.part" "$name.osm.pbf.complete"
"@ | Set-Content -Encoding ascii -LiteralPath $batch
sftp -b $batch karta-landing@karta-host      # -b: any failed command stops the batch, so no marker after a failed upload
```

The marker tolerates a byte-order mark, CRLF and upper-case hex. A
hypervisor shared folder (VirtualBox, VMware, Hyper-V) or an SMB share is
only an untrusted transfer space: from there, use the command with an
expectation.

Every delivered file must be owned by the landing account and writable by
it alone (mode 0644: umask 022 for the landing account); files writable by
their group or others are refused (`unsafe_mode`), since anyone who can
write them could change them in place. The watcher copies a complete
delivery into the handoff volume and leaves the landing files alone; it remembers what it consumed and never processes
the same files twice. Remove published deliveries from the landing area
(`make op-status` shows each entry's state). A raw Geofabrik file has no
provenance sidecar: keep `require_provenance` off in the Iran region file, or
deliver a reviewed sidecar with it. The watcher's audit names the channel,
its credential, the landing owner, name, size, time and digest: who
delivered is in the landing account's SSH logs, outside Karta. Where every
delivery needs a named person, use the command only.

### Status, pause, resume and turning it off

```bash
make op-status                                       # intake: enabled, auto_activate, open authorizations, watcher state
make op CMD='intake-pause --reason "checking a delivery"'
make op CMD='intake-resume --reason "checked"'
```

A **rollback** (and a restore) pauses automatic activation of deliveries
admitted only by the watcher, so automation never undoes it; they are
still built and kept `ready` (`intake_activation_paused`). A command
delivery by a named person is not paused: that is how a deliberate delivery
resumes publication. `intake-resume` (scope `publish`) resumes the watcher.

* **Stop the watcher**: `make intake-off` (stops it and removes its
  credential; its open authorizations expire by themselves, or close them
  with `make op CMD='revoke --sha256 ... --reason "..."'`).
* **Stop one digest**: `make op CMD='revoke --sha256 ... --reason "..."'`
  closes every open authorization of it, the intake's included, and the
  intake cannot authorize it again (`digest_revoked`) until an operator
  authorizes it (`make op CMD='authorize ...'`).
* **Remove a person's credential**: `scripts/operator-credential.sh remove alice`
  (refused from the next request on, no restart).
* **Turn the intake off**: remove `KARTA_INTAKE_DIR` from `.env` and
  `make up`: the publisher refuses intake authorizations
  (`intake_disabled`).

None of these affects online updates or direct manual publication.

### Failure codes

| Where | Code | Meaning, action |
| --- | --- | --- |
| landing entry | `waiting_for_completion_marker`, `settling`, `queue_full` | waiting: no marker yet; files changed within `KARTA_INTAKE_SETTLE`; `KARTA_INTAKE_MAX_OPEN` handoffs in flight |
| landing entry | `size_mismatch`, `digest_mismatch`, `changed_during_copy`, `completion_stale` | the copy is not the source's (cut, changed, or replaced after the marker): deliver again under a new name |
| landing entry | `invalid_completion`, `invalid_name` | the marker is not `karta-delivery/1` for this file, or the name is not allowed |
| landing entry | `symlink`, `not_regular_file`, `hard_link`, `wrong_owner`, `unsafe_mode`, `too_large`, `empty_file` | unsafe or foreign files (also files writable by their group or others: upload with umask 022) are never opened: remove them |
| landing entry | `digest_revoked` | an operator revoked this digest: only an operator's `authorize` releases it again |
| landing entry | `region_mismatch`, `malformed_snapshot`, `timestamp_missing` | the advisory header check failed before any authorization |
| preflight | `landing_missing`, `not_a_directory`, `symlink`, `wrong_owner`, `writable_by_others`, `writable_by_group`, `unsupported_filesystem`, `parent_*` | fix the landing area (above) |
| API | `intake_disabled`, `intake_limit_reached`, `intake_refused` (`region_mismatch`, `too_large`, `validity_beyond_cap`, `digest_revoked`), `credentials_unavailable` | the intake is off; too many open authorizations for the credential; the request is outside the publisher's bounds, or the digest was revoked by an operator; the credentials file does not parse |
| submission | `authorization_revoked`, `authorization_expired`, `unauthorized_digest` | the authorization was revoked, expired (also while queued) or never covered the digest, checked again at the switch: the built release stays `ready`, the pointer is unchanged; a fresh authorization (or a new delivery) re-evaluates it and activates the ready release without a rebuild |
| submission | `intake_activation_paused` | watcher activation is paused: `intake-resume`, or activate the ready release explicitly |

## Controlled source bridge (opt-in, Stage 5)

The bridge turns Geofabrik's Iran extract into signed online updates that
Karta's existing fetcher accepts ([ADR 0006](adr/0006-stage5-hybrid-intake.md)).
Three processes of the api image (`compose.bridge.yaml`):

* `bridge-acquire` checks the distributor's hints (HEAD validators and the
  `.md5`) every `poll_interval` and downloads only when they change, or at
  least every `reverify_interval`; resumes only with a strong ETag
  (`If-Range`); discards a file replaced during the download
  (`source_changed`) or not matching the `.md5`; honours `429`/`503`
  `Retry-After`. The only process with a route out; no key.
* `bridge-sign` has no network and the only copy of the key. It verifies
  every download with Karta's importer checks, signs only newer data, and
  allocates serials from its durable state (the exact envelope is saved
  before it is published). Different bytes with equal or older data are
  **held** and reported, not signed. It renews the current manifest at
  `renew_before` without the distributor.
* `bridge-serve` serves the manifest and the content-addressed assets over
  HTTPS, read-only, with Range and strong ETags.

A bridge signature means the bytes passed the bridge's policy; it is not
Geofabrik's signature (Geofabrik publishes none) and not proof that the OSM
data are correct.

### Co-located setup

Prerequisites (owner inputs): the Iran region file (`config/regions/iran.json`
from the fixed, recorded PBF; docs/operations.md, "The Iran region"), the
signing key from its custodian, and Geofabrik's current download terms.

1. `cp config/bridge/geofabrik-iran.example.json config/bridge/geofabrik-iran.json`;
   put a real contact into `user_agent`; review. Starting `bridge-acquire`
   downloads the full Iran extract.
2. `cp config/bridge/signer.example.json config/bridge/signer.json`; set the
   key id. The custodian places the key at `secrets/bridge/signing-key.pem`
   (or sets `KARTA_BRIDGE_SIGNING_KEY_FILE`), readable by UID 65532 inside
   the private `secrets` directory.
3. `make bridge-tls`: the bridge's own CA and `bridge-serve`'s certificate
   (`secrets/bridge/`), and the CA copy `config/sources/bridge-ca.pem`.
4. Write the fetcher's source file `config/sources/bridge.json`
   (`config/sources/README.md`, "A co-located bridge") with the bridge's
   public key (`go run ./cmd/karta-sign pubkey --key secrets/bridge/signing-key.pem`).
5. `.env`: `KARTA_PUBLISH_REGION=iran`,
   `KARTA_BRIDGE_SOURCE_FILE=/config/bridge/geofabrik-iran.json`,
   `KARTA_ONLINE_SOURCE_FILE=/config/sources/bridge.json`,
   `KARTA_DATA_STALE_AFTER=72h` (proposed; owner decision) and, for
   monitoring, `KARTA_BRIDGE_METRICS_LISTEN_ADDR=:9465`.
6. `make up-bridge`, then `make bridge-status` (acquire and sign reports)
   and `make op-status` (`online.verified.serial`, freshness).

Co-located, the fetcher leaves `egress` and reaches only `bridge-serve` on
the no-NAT `bridge` network (`KARTA_BRIDGE_SUBNET`, in its
`allowed_networks`); `bridge-sign` has no network. Use `$(BRIDGE_COMPOSE)`
(`docker compose -f compose.yaml -f compose.bridge.yaml`) for every command
that touches the fetcher while the bridge is in use, or `make up-online`
recreates the fetcher with `egress`.

**Separate bridge host.** The same file alone
(`docker compose -f compose.bridge.yaml --profile bridge up -d`) with
`KARTA_BRIDGE_BIND` set to the interface the Karta host reaches,
`BRIDGE_TLS_NAMES="bridge.example.org"` for `make bridge-tls`, and the
Karta host's fetcher keeping its own `egress` with `manifest_url`
`https://bridge.example.org:8443/manifest.json`. The Make targets take
`BRIDGE_COMPOSE="docker compose -f compose.bridge.yaml"` there.

### The bridge signer refuses to start

The signer fails closed (exit status 4, `KartaBridgeSignerStalled`) when its
state is missing although a manifest is published, or its high-water serial
is below the published manifest's (`state_behind_published`, for example
after restoring an older backup of `bridge-state`), or the clock is earlier
than the last `issued_at` (`clock_behind`). For a state problem:

1. read the serial Karta accepted: `make op-status`, `online.verified.serial`
   (and the published manifest's serial in `make bridge-status`);
2. raise the high-water serial to at least both, with a reason:

   ```bash
   make bridge-raise-high-water SERIAL=<at least that serial> REASON="state restored from the 2026-10-01 backup; Karta verified 41"
   ```

The raise is recorded in the signer state and never lowers the serial; the
next manifest uses the following serial. A snapshot the old state never
recorded is signed again under a new serial (same bytes). For the clock,
fix the time; nothing is signed meanwhile and the current manifest stays
valid until it expires.

### Failure codes (`make bridge-status`: acquire `last_error.code`, sign `last_error.code` and `held.code`)

| Code | Meaning, action |
| --- | --- |
| `rate_limited` | `429`/`503`: backing off, `Retry-After` honoured |
| `source_changed` | `iran-latest` was replaced during the download, or the `.md5` changed: retried at the next check |
| `distributor_checksum_mismatch` | the bytes do not match the distributor's `.md5`: nothing spooled (`KartaBridgeSourceRefused`) |
| `source_config` | the source file is invalid |
| `http_status`, `tls_error`, `destination_refused`, `redirect_refused`, `too_large`, `timeout`, `stalled`, `network_error`, `insufficient_storage` | as for the fetcher ("Failure codes" above) |
| `not_newer` (held) | different bytes whose data are not newer than the last signed snapshot: inspect; nothing to undo |
| `malformed_snapshot`, `region_mismatch`, `timestamp_missing` (held) | the download failed Karta's checks; a `region_mismatch` after a Geofabrik boundary change needs a reviewed region-file update |
| `state_behind_published`, `clock_behind` | fail closed: "The bridge signer refuses to start" |
| `asset_missing` | the asset of the current manifest is gone: not renewed; restore the publish volume or let the next download re-sign |
| `serial_exhausted` | the serial reached 999,999,999,999: a new bridge identity and key are needed |

**Turn it off**: `make bridge-off` stops the bridge and the fetcher reading
it; the active release stays and ages. Local intake and direct manual
publication are unaffected. Online updates from another source: set
`KARTA_ONLINE_SOURCE_FILE` and `make up-online`.

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

`make secrets` creates two random 256-bit tokens, the file the publisher
reads and the file the API's metrics listener reads:

| File | Holder | Scopes |
| --- | --- | --- |
| `secrets/operator_token` | operators (`operator-cli`, curl) | `status`, `publish`, `rollback`, `cleanup` |
| `secrets/operator_monitor_token` | monitoring | `status` |
| `secrets/operator_tokens` | publisher | `NAME SCOPES SHA256(token)` per line: hashes only |
| `secrets/metrics_tokens` | API metrics listener | the monitoring credential's hash only; rewritten in place, re-read when it changes |
| `secrets/operator_tokens.extra` | host (merged into `operator_tokens`) | narrow extra credentials (hashes) |
| `secrets/intake_watch_token`, `secrets/NAME.token` | the intake watcher; one person | `intake_watch`; `intake_submit` |

Narrow extra credentials (an intake person, the intake watcher, a second
monitoring credential) are added and removed with
`scripts/operator-credential.sh add NAME intake_submit|intake_watch|status`
and `remove NAME`: they live in `secrets/operator_tokens.extra`, and
`scripts/gen-secrets.sh` merges them into `secrets/operator_tokens`, which
it rewrites in place. The publisher re-reads that file when it changes: a
new credential works and a removed one is refused from the next request on,
**without a restart**. A file that does not parse refuses every request
(503 `credentials_unavailable`) until it is fixed. For other scopes, add a
line `NAME SCOPES SHA256` to `operator_tokens.extra` yourself (an
`intake_*` scope must be a credential's only scope) and run
`scripts/gen-secrets.sh`. Rotate the two generated tokens with
`make rotate-operator-tokens`. In production keep the
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
| 8 | the import exceeded `KARTA_PUBLISH_TIMEOUT` and was stopped (`publication_timeout`) | unchanged |
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
  256 MiB / 0.5 CPU; monitoring (opt-in) exporter 128 MiB / 0.25 CPU,
  Prometheus 512 MiB / 0.5 CPU. Measured use is in the PR descriptions
  (Stage 1 for the Chitgar import, Stage 2 for publication, Stage 4 for
  Chitgar load and resource figures, labeled tier B). All of these are
  provisional until measured on the target host with the Iran snapshot
  (docs/operations.md, "Capacity").
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

## Operations

The commands for running a deployment. Why each works the way it does, the
trust inventory and the response to every alert are in
[docs/operations.md](operations.md).

### Publication deadline

```bash
KARTA_PUBLISH_TIMEOUT=10h make up   # in .env; default 6h (provisional: derive it from the measured publication time)
```

A publication still running at the deadline is stopped with every process
it started, its candidate is dropped, and it ends `failed`
(`publication_timeout`); `karta import` exits 8. It is not retried
automatically: touch the marker (inbox), `online-retry` (online) or run
the import again once the cause is fixed.

### Monitoring

```bash
make up-monitoring       # karta_monitor role, PostgreSQL exporter, Prometheus with the Karta rules (profile monitoring)
make test-alerts         # promtool: configuration check and rule unit tests
curl -s -H "Authorization: Bearer $(cat secrets/operator_monitor_token)" http://127.0.0.1:9464/metrics             # API
curl -s -H "Authorization: Bearer $(cat secrets/operator_monitor_token)" http://127.0.0.1:8081/v1/operator/metrics # publisher
open http://127.0.0.1:9090/alerts
```

Set the acceptable data age (`KARTA_DATA_STALE_AFTER`) and, in
`deploy/monitoring/thresholds.yml`, the API objectives once the owner has
decided them; until then `KartaDataAgeThresholdUnset` and
`KartaObjectivesUnset` say so. No alert destination is configured.

### Backup and restore

```bash
make backup                                                     # backups/karta-<UTC time>: cluster base backup, outbox, registry summaries, config, MANIFEST
make backup BACKUP_DEST=/mnt/backup                             # elsewhere (keep ./secrets offline, separately)
KARTA_BACKUP_MAX_RATE=20M make backup                           # limit pg_basebackup's read rate on a busy host
make restore BACKUP=backups/karta-20261002T150153Z              # into an empty project or host
make restore BACKUP=backups/karta-20261002T150153Z REPLACE=1    # deletes this project's database and outbox first
make restore-check                                              # verify the registry and every retained release now (changes nothing)
COMPOSE="docker compose -p karta-restore-test" KARTA_HTTP_PORT=18180 KARTA_OPERATOR_PORT=18181 \
  KARTA_API_METRICS_PORT=18464 make restore BACKUP=...         # rehearse into an isolated project
```

A restore checks both destinations first and changes nothing unless the
database volume and the fetcher's outbox are empty or `REPLACE=1` is given
(a volume with an interrupted restore's files is not empty). It then stops
the whole stack, restores, and starts only the API and the publisher: the
fetcher and the monitoring profile stay stopped. It pauses automatic online
activation and audits the restored pointer. Before `make up-online` and `online-resume`, confirm the
producer's current manifest serial. Then resubmit anything published or
authorized after the backup (docs/operations.md, "Backup and restore").

### Rotating credentials

```bash
make rotate-db-password ROLE=api        # or importer, monitor, superuser: no restart, data kept
make rotate-operator-tokens             # both operator tokens; no restart (the publisher reloads its credentials)
scripts/operator-credential.sh remove NAME   # an intake or other extra credential, refused at once
make monitoring-role                    # (re)create karta_monitor after a restore without it
```

Signing keys for online updates: "Rotate or revoke a signing key" above.

### Upgrade

```bash
make backup
git pull && make build
make up                                  # and make up-monitoring / make up-online where used
make op-status smoke
# failed? check out the backup's karta_commit, make build, then:
make restore BACKUP=backups/karta-... REPLACE=1
```

### Incidents

| Situation | Commands |
| --- | --- |
| bad data went live | `make op CMD='rollback --reason "..."'` (pauses online activation); later `online-resume` |
| online source misbehaves or a key may be compromised | `make op CMD='online-pause --reason "..."'`, then "Rotate or revoke a signing key" |
| a publication hangs or overruns | `make op-status` (job phase), `make logs`; it stops at `KARTA_PUBLISH_TIMEOUT`; `docker compose restart publisher` records it interrupted |
| disk full | `make op CMD='cleanup --dry-run --reason "disk"'`, then without `--dry-run`; serving continues, builds fail `insufficient_storage` |
| database lost or corrupt | `make restore BACKUP=... REPLACE=1` |
| host restart | nothing: services restart (`unless-stopped`) and the publisher recovers; check `make op-status` |

### Capacity and the Iran region

```bash
scripts/capacity-run.sh tier-d-iran-closed-c64 -duration 10m -concurrency 64   # report in artifacts/capacity/
scripts/capacity-run.sh tier-d-iran-rate200 -rate 200 -duration 10m -concurrency 64
docker compose run --rm --no-deps -v "$PWD/data/local:/in:ro" --entrypoint /usr/local/bin/karta importer \
  region-draft --snapshot /in/iran.osm.pbf --id iran --name "Iran"              # region file for the chosen snapshot
```

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

Secrets in `./secrets` survive both. Database passwords and tokens rotate
without a reset and without losing data: `make rotate-db-password ROLE=...`
and `make rotate-operator-tokens` ("Operations" below).

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
make test-integration          # isolated compose project on ports 18080-18083/18087/18090/55433 (Stage 1-4 suites; local controlled HTTPS source)
make test-integration RUN=TestOperations    # only Stage 4: deadlines, rotation, backup/restore, monitoring profile
make test-alerts               # promtool: Prometheus configuration and alert rule unit tests
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
| submission `publication_timeout` / import exit 8 | the build exceeded `KARTA_PUBLISH_TIMEOUT`; the reason names the phase. Fix the cause or raise the deadline, then touch the marker |
| online submissions stay `ready` after a restore | a restore pauses automatic online activation: confirm the producer's serial, then `online-resume` |
| `make restore` "holds a database; add --replace" | the project already has data: restore into another project, or `REPLACE=1` to delete it first |
| `online.enabled` false | `KARTA_ONLINE_SOURCE_FILE` not set for the publisher; online updates are opt-in |
| `online.fetcher.error` "has not written its state" / growing `state_age_seconds` | the fetcher is not running: `make up-online`, `docker compose --profile online logs fetcher` |
| online submissions stay `ready` with `online_activation_paused` | automatic activation is paused (by an operator or a rollback): `online-resume`, or activate the release |
| fetcher `destination_refused` for a private mirror | add its network to the source file's `allowed_networks` |
