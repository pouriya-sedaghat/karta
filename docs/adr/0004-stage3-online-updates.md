# ADR 0004: Stage 3, online updates and hybrid publication

Status: proposed with the Stage 3 PR, 2026-10-01. Builds on
[ADR 0001](../architecture.md), [ADR 0002](0002-stage1-implementation.md)
and [ADR 0003](0003-stage2-publication.md). It records a stored-data
migration (registry schema version 3), additive public API changes and the
trust decision for unattended updates, as ADR 0001 requires before each.

## Context

Stage 2 publishes snapshots an operator places in a local inbox (or names on
the command line); a snapshot is accepted only if its SHA-256 is pinned in
the region file or authorized through the audited operator API. Stage 3 must
poll a configured HTTPS source, download a newer trustworthy complete
snapshot within strict limits and publish it through the same
validation/build/activation path while serving continues; a failed poll,
download or import must never change the active release or block manual
publication.

Two owner decisions are still open and are deliberately **not** made here:
the production HTTPS provider (and who holds the signing key), and the
permitted update lag. Both stay explicit configuration: there is no default
source URL, no default trusted key and no default staleness threshold.

## Decisions

### Opt-in, per deployment

Online updates are off unless the publisher is given an online **source
file** (`KARTA_ONLINE_SOURCE_FILE`, a JSON file under `config/sources/`; see
its README) and the `fetcher` service is started (`make up-online`, compose
profile `online`). The source file names the region it serves (it must equal
the publisher's region), the manifest URL, further allowed hosts and
networks, an optional CA bundle and bearer-token file, the trusted signing
keys and the polling, validity, timeout and retry bounds. It is reviewed
configuration: changing who is trusted, or where the fetcher may connect,
is a change to that file, never something the source can do. Both the
fetcher and the publisher read it again for every check and every delivery,
so revoking a key takes effect without a restart.

### Trust: a manifest signed by a pinned key

A source publishes a **manifest**: a DSSE envelope
(https://github.com/secure-systems-lab/dsse, payload type
`application/vnd.karta.snapshot-manifest+json; version=1`, signature over
DSSE's pre-authentication encoding) carrying Ed25519 signatures. The signed
payload binds:

| Field | Bound to |
| --- | --- |
| `format` | `karta-snapshot-manifest/1` |
| `region_id`, `bbox` | the deployment's region id and box (box within 1e-7°) |
| `serial` | a positive integer the signer increases with every manifest |
| `issued_at`, `expires_at` | a validity window, at most the source's `max_manifest_validity` |
| `snapshot.url`, `.sha256`, `.size_bytes` | the exact snapshot bytes and where to fetch them |
| `snapshot.data_timestamp` | the OSM data timestamp the importer must derive from those bytes |
| `provenance.url`, `.sha256`, `.size_bytes` | the exact provenance sidecar, if any |

A manifest is accepted only if at least one signature verifies with a key
listed in the source file's `trusted_keys` (and inside its optional
`not_after`), and **no** signature that names a trusted key fails to verify
(a forged signature under a trusted key id is treated as an attack, not
ignored). Signatures by unknown key ids are ignored, which is what lets a
producer sign with a new key before every deployment trusts it.

The signature is the authorization of the digest. It replaces, for online
deliveries only, the Stage 2 requirement that an operator authorize each
digest; it does not weaken it for the inbox or the command line, where a
signed manifest is not even read. A source file can keep both
(`require_operator_authorization: true`): a valid signature **and** a pinned
or operator-authorized digest, for owners who want an unattended download
but an attended publication.

What does **not** authorize anything: the manifest's location, TLS (it
authenticates the server, not the data), a checksum published next to the
snapshot, the fetcher's completion marker, file names or times. The
publisher compares the staged snapshot with the signed digest and size
itself; the marker digest the fetcher writes only proves that the copy is
complete, as in Stage 2.

Rejected: a bare `.sha256` file from the same host (anyone who can change the
snapshot can change it); TLS pinning alone (a compromised or mistaken
provider still serves bad data); OpenPGP or X.509 code-signing chains (larger
dependency and review surface for one pinned key); signify/minisign files
(no binding of region, serial and validity without a second format); Sigstore
keyless signing (needs online transparency infrastructure the deployment
must not depend on, and an identity policy the owner has not chosen).

### Replay, staleness and conflicting claims

* **Replay.** The newest verified serial is persisted twice: by the fetcher
  (its state file) and, authoritatively, by the publisher in the registry
  (`registry.source_state`). A lower serial is refused (`manifest_replayed`);
  a different envelope under a serial already verified is refused
  (`manifest_conflict`). A source that keeps serving an old but valid
  manifest can only delay updates until `expires_at`; after that every check
  fails (`manifest_expired`) and the data ages visibly (freshness below).
* **Validity.** `issued_at` may not be in the future (beyond
  `KARTA_MAX_FUTURE_SKEW`), `expires_at` must be later than now and at most
  `max_manifest_validity` after `issued_at`. Deployments need a synchronized
  clock.
* **Conflicting claims.** The signed `data_timestamp` must not be after
  `issued_at` and must equal the timestamp the importer derives from the
  staged snapshot (provenance source header or PBF header), otherwise the
  delivery is refused (`manifest_conflict`) rather than one claim being
  picked. The signed provenance sidecar goes through the Stage 2 provenance
  checks (digest, size, box, timestamps, license).
* **Older or equal data** is handled by the unchanged Stage 2 forward rule:
  older than the active release is refused (`older_than_active`), a
  different snapshot with the same timestamp is refused (`not_newer`), the
  same snapshot is a duplicate.

### Key handling, rotation and failure

The private key never reaches a Karta deployment: producers sign where
snapshots are made (`cmd/karta-sign`, or OpenSSL plus any DSSE
implementation; both use PKCS#8 Ed25519 keys). Who holds it is an owner
decision. Rotation:

1. Generate the new key; add its public key to the source file's
   `trusted_keys` beside the old one (reviewed change, deployed).
2. Sign new manifests with both keys during the overlap.
3. Sign with the new key only; remove the old key from `trusted_keys`, or
   schedule its end in advance with `not_after`.

If a key is compromised: remove it from the source file (effective at the
next check and delivery, without a restart), pause automatic activation
(`make op CMD='online-pause ...'`) if a bad release may have been accepted,
roll back if needed (which also pauses), and have the producer publish a
manifest with a higher serial signed by a trusted key. A compromised key can
publish any snapshot that also passes the Stage 2 checks (complete PBF,
region box, provenance, timestamp, forward rule, row counts, validation
searches and tiles) until it is removed; `require_operator_authorization`
removes that exposure at the cost of unattended publication.

Failure behaviour: a source file that does not parse, names another region
or has no valid key stops the publisher and fetcher at start; if it breaks
later, each check fails with `source_config` and each delivery is refused.
When no trusted key is valid any more (all past `not_after`), every manifest
is refused (`signature_untrusted`): nothing is published and the data ages.
In every failure the active release keeps serving.

### A separate fetcher process holds the only route out

The publisher parses untrusted snapshots with osm2pgsql and holds the
importer database credential; Stage 2 gave it no NAT route out. Stage 3 keeps
that: downloads run in a separate `karta fetcher` process (the distroless
API image, UID 65532, read-only root, all capabilities dropped, 256 MiB / 0.5
CPU / 64 PIDs) that has **no database credential and no listener**, sits on
its own `egress` network, and writes only to its outbox volume, which the
publisher mounts read-only. The publisher never opens a network connection
for updates; the API never contacts the source on any request path.

The fetcher hands snapshots over with the Stage 2 inbox completion protocol,
plus the signed manifest: `NAME.osm.pbf`, optional
`NAME.osm.pbf.provenance.json`, `NAME.osm.pbf.manifest.json` (the exact
envelope bytes) and the `NAME.osm.pbf.ready` marker, written last, each by an
atomic rename or link. `NAME` is `<region>-s<serial, 12 digits>-<digest
prefix>`, so name order is serial order. The publisher processes the outbox
like the inbox (listing without opening, settle interval, `O_NOFOLLOW` copy
into private staging, digest against the marker) and then verifies the
staged envelope itself, as above. A compromised fetcher can therefore delay
or withhold updates and misreport its own status, but cannot publish
anything that is not signed by a trusted key; the acceptance tests write
forged deliveries into the outbox to prove it.

Rejected: downloading inside the publisher (gives the snapshot parser and
the importer credential a route out); an egress proxy for the publisher (the
parser would still be on a network that reaches it, and one more service to
pin and configure).

### Network, time, size, concurrency and disk bounds

* **Destinations.** `https` only, no credentials, query or fragment in any
  URL; snapshot and sidecar URLs (absolute or relative to the manifest)
  must be on the manifest's host or an `allowed_hosts` entry (exact host and
  port). **No redirects** are followed (`redirect_refused`): the source
  publishes direct URLs in the signed manifest. Every connection's actual
  address is checked when it is made (a dialer control hook, after name
  resolution), so a name that resolves, or later re-resolves, to loopback,
  RFC 1918, shared (100.64/10), link-local and cloud metadata
  (169.254/16), documentation, benchmarking, multicast, reserved, ULA or
  IPv6 forms embedding such IPv4 addresses (mapped, NAT64, 6to4) is refused
  (`destination_refused`) unless an `allowed_networks` prefix contains it.
  Proxy environment variables are ignored.
* **TLS.** TLS 1.2+, certificate and host name always verified, against the
  system roots or only the source's `ca_file`.
* **Time.** 10 s to connect, 10 s TLS handshake, 20 s for response headers,
  30 s for the manifest and sidecar, `download_timeout` (1 h) for the
  snapshot, and `stall_timeout` (60 s) without data aborts a transfer.
* **Bytes.** Envelope 64 KiB, payload 16 KiB, sidecar 1 MiB, snapshot exactly
  its signed size and at most `KARTA_MAX_INPUT_MB`; a declared
  `Content-Length` must match, an unknown-length body is cut at the signed
  size and discarded if it goes beyond. Bodies are requested and accepted
  only as `identity` (no transparent or declared decompression), and
  response headers are limited to 64 KiB.
* **Concurrency.** One check at a time, one download at a time, at most two
  connections; one fetcher per outbox (a lock file).
* **Disk.** Before downloading, the outbox volume must hold the snapshot,
  sidecar and `KARTA_ONLINE_RESERVE_MB`; a full volume fails the attempt
  (`insufficient_storage`). The outbox keeps the two newest deliveries and
  one partial download. The publisher's Stage 2 staging and release storage
  checks apply unchanged.
* **Retries.** After a failed check the next one waits
  `retry_initial × 2^(failures−1)`, capped at `retry_max`, scaled by a random
  factor in [0.5, 1) (jitter); after a success, the poll interval ±10 %. A
  snapshot whose download fails `max_download_attempts` times is set aside
  for `abandon_for` (`download_abandoned`) while the manifest is still
  checked at the poll interval, so a different snapshot is noticed at once.
  The next attempt is persisted and shown to operators.

### Interrupted transfers resume only with a verified byte identity

A download is written to `.partial/<sha256>.part` (private 0700 directory).
After an interruption the bytes held are hashed again and the rest is
requested with `Range` and, when the first response had a strong ETag,
`If-Range`; a `206` must cover exactly the missing bytes of the signed size,
a `200` restarts from zero, anything else discards the partial file. The
complete file must match the signed SHA-256 or it is deleted
(`digest_mismatch`). Only then is it linked into the outbox, so partial input
is never visible to the publisher.

### State, idempotency and recovery

| Crash during | Recovery |
| --- | --- |
| poll (before or after the manifest verified) | nothing persisted changes; the next check repeats it |
| download | the partial file is resumed (verified as above) |
| after the download, before the delivery | the complete partial file is delivered without downloading again |
| delivery, before the marker | the incomplete delivery is removed at start (the publisher never saw it); the verified partial is delivered again |
| after the marker, before the state was saved | the complete delivery is adopted at start; nothing is downloaded or delivered twice |
| publisher verification, staging, build, switch | Stage 2 recovery: the submission becomes `interrupted` and is retried from the unchanged delivery (up to `KARTA_PUBLISH_MAX_ATTEMPTS`); a recorded serial is accepted again for the same envelope; the pointer only moves in a committed transaction |

Idempotency: the fetcher does not download a snapshot whose digest it
already delivered (a new serial for the same snapshot is only checked); the
publisher records outcomes per exact set of files, and a snapshot whose
release exists is a duplicate (Stage 2), so the same digest never builds or
switches twice, across restarts too. Fault-injection points
(`fetch.after_manifest`, `fetch.mid_download`, `fetch.after_download`,
`fetch.before_marker`, `fetch.after_marker`, `online.after_verify`, plus the
Stage 2 points) prove each row.

### Simultaneous manual and online candidates

The rule is deterministic and adds nothing new to the Stage 2 rules:

1. Builds are serialized by the build lock across the inbox, online
   deliveries and command-line imports.
2. In every publisher scan the manual inbox is processed first, then online
   deliveries, each in name order (online: serial order).
3. Every candidate passes the forward rule against the active release when
   its build starts and again inside the pointer transaction, which also
   compares the active release with the one seen at build start
   (compare-and-swap). The newest data timestamp ends up active; the same
   digest is a duplicate; of two different snapshots with the same timestamp
   the first switched wins, so the manual one wins within a scan.
4. An explicit operator switch or rollback is never overwritten by an online
   build that started earlier: that build's compare-and-swap fails and it
   stays `ready` (`active_changed`). A **rollback also pauses automatic
   activation of online snapshots**, in the rollback's own transaction, so a
   later online snapshot cannot undo it either; an operator resumes it
   explicitly. While paused, online snapshots are still downloaded,
   verified, built and kept `ready` (`online_activation_paused`).
5. The pause is read under a row lock inside the pointer transaction, so a
   pause and an automatic switch are serialized. Pause, resume and refusals
   are audited (`online_pause`, `online_resume`, `noop` and `rejected`
   outcomes). An operator can queue the newest failed online delivery for
   another attempt (`online/retry`, audited), for example after freeing
   disk space.

Manual publication never depends on the online path: the inbox and the
command-line importer work with the fetcher stopped, the source down or the
network disconnected.

### Freshness and observability

* **Operator status** (`GET /v1/operator/status`): `online` (source summary
  without secrets, trusted key ids and fingerprints, the activation policy,
  the newest manifest the publisher verified, the fetcher's report: last
  check, last success, last error, consecutive failures, next attempt,
  current manifest, last delivery and download progress, and how old that
  report is) and `freshness` (active data timestamp and age, configured
  threshold, stale or not).
* **Metrics** (`GET /v1/operator/metrics`, Prometheus text, `status` scope):
  active data age, staleness, online enabled and activation policy, the
  verified serial and data timestamp, the fetcher's last check, success,
  error code, failures, next attempt and state age, submissions by source
  and state, releases by state, storage.
* **Staleness** is computed from the active release's data timestamp only,
  never from a check: a source that answers but has nothing new, or an old
  release that still serves, does not make the data fresh. Alert on
  `karta_data_stale` (or the age) and on `karta_online_fetcher_state_age_seconds`.
  The threshold is `KARTA_DATA_STALE_AFTER` (unset by default: an owner
  decision).
* Check times are reported by the fetcher, the network-facing process, and
  shown as such; the publisher's own verification is recorded separately.

## Registry schema version 3 (stored-data migration)

* `submissions.source` and `audit.source` allow `online`;
  `submissions.manifest_serial` and `manifest_sha256` are added.
* New `registry.source_state` (newest verified manifest per region),
  `registry.online_policy` (pause and retry per region) and
  `registry.freshness` (update mode and staleness threshold, the only new
  table `karta_reader` may read).

Release databases are unchanged. A Stage 2 publisher refuses to start on a
version 3 registry (newer than it supports), which is the safe outcome; a
Stage 2 API reads it as before (it only reads releases and the pointer).

## Public API changes (additive)

* `freshness.update_mode` has the new value `online` on deployments with
  online updates enabled; new `freshness.stale_after_seconds` and
  `freshness.stale` (null without a threshold).
* `capabilities.online_updates` is `true` exactly when online updates are
  enabled on the deployment.

Nothing about the source (URL, errors, check times) is in the public API.

## Consequences and limits

* No forward-proxy support: the fetcher needs direct HTTPS egress to the
  source (proxy variables are ignored so they cannot bypass the destination
  policy).
* Complete snapshots only, as before; no replication diffs.
* One source per deployment and region; one fetcher per outbox.
* Fetcher-reported times are not authenticated; alert on data age.
* Correct clocks are required for manifest validity.
* Before operating against a real provider: choose the provider and the key
  owner, have the provider publish signed manifests (or run the signing step
  at the extraction host), commit the reviewed source file, decide the
  permitted lag (`KARTA_DATA_STALE_AFTER`) and alerting, size
  `max_manifest_validity` and the poll interval to it, and run the stack with
  that source in the target environment. Stage 3 is verified only against the
  local controlled source in CI and in the implementation environment, not
  on the owner's VM.
