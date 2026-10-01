# Data flow, trust boundaries and security assumptions (Stage 3)

```mermaid
flowchart LR
    subgraph Operator host
        INBOX["inbox (data/inbox)<br/>read-only mount"]
        PBF["data/local, testdata<br/>(read-only, CLI import)"]
        CFG["region config<br/>(config/regions, read-only)"]
        SEC["./secrets (0700)"]
        OPR["operator: make op / curl<br/>bearer token"]
        SRCF["online source file<br/>(config/sources, read-only):<br/>pinned Ed25519 keys, destinations"]
    end
    subgraph "Docker network: egress (opt-in, profile online)"
        FET["fetcher (distroless, UID 65532, read-only FS)<br/>poll → verify manifest → download → deliver<br/>no DB credential, no listener"]
    end
    SRC["HTTPS snapshot source<br/>(owner's choice; untrusted location)"]
    OUT[("online outbox volume<br/>fetcher rw, publisher ro")]
    subgraph "Docker network: backend (internal, no external route)"
        PUB["publisher (UID 10001, read-only FS)<br/>scan → stage → verify → build → switch<br/>operator API :8081"]
        IMP["importer (UID 10001, on demand)"]
        DB[("PostgreSQL/PostGIS<br/>karta_registry + karta_r… release DBs")]
        API["API (distroless, UID 65532, read-only FS)"]
    end
    STG[("staging volume<br/>0700, publisher only")]
    Client["Browser / core / apps"]
    INBOX --> PUB
    FET -- "HTTPS, TLS verified, no redirects,<br/>public or allowed addresses only" --> SRC
    FET --> OUT --> PUB
    SRCF --> FET & PUB
    PUB <--> STG
    PBF --> IMP
    CFG --> PUB & IMP
    SEC -. secrets .-> PUB & IMP & API & DB
    OPR -- "HTTP, 127.0.0.1:8081, network 'operator' (no NAT)" --> PUB
    PUB & IMP -- "karta_importer: CREATE DATABASE, candidates, registry" --> DB
    API -- "karta_api: read-only, 3 s statements" --> DB
    Client -- "HTTP GET only, 127.0.0.1:8080 by default" --> API
```

## Trust boundaries

| Boundary | Trusted | Controls |
| --- | --- | --- |
| Online source → fetcher | nobody: the source's location, TLS identity, file names and checksums prove nothing about the data | only the source file's `https` manifest URL and allowed hosts; every connection's resolved address checked (no loopback, private, link-local/metadata, shared, reserved, multicast unless in `allowed_networks`); TLS 1.2+ verified (system roots or the source's CA file only); no redirects; no proxy from the environment; bounded connect/handshake/header/total/stall times; manifest 64 KiB, sidecar 1 MiB, snapshot exactly its signed size (≤ `KARTA_MAX_INPUT_MB`), no decompression; manifest signature (pinned Ed25519 key), region, box, serial and validity verified **before** any download; downloads in a private 0700 directory, hashed, resumed only with `If-Range` and a full-file digest match; one check and one download at a time; backoff with jitter; outbox space reserve |
| Fetcher → publisher (outbox) | nobody: the fetcher faces the network and is treated as untrusted | the publisher reads the outbox read-only with the inbox protocol (marker, settle, `O_NOFOLLOW` copy into private staging) and then verifies the staged signed manifest itself: trusted key, region, box, validity, serial not lower than (or conflicting with) the newest one it verified (registry), staged snapshot and sidecar exactly the signed bytes, signed data timestamp equal to the derived one; the marker digest authorizes nothing; then all Stage 2 checks |
| Inbox → publisher | nobody: anyone who can write the inbox directory can submit files; nothing is imported unless its digest is pinned or authorized | read-only mount; completion marker required before any file is opened; names restricted; lstat only until then; symlinks, directories, devices, FIFOs rejected without opening; `O_NOFOLLOW\|O_NONBLOCK` open checked against the listed inode; settle interval; copy into a private 0700 staging volume with the digest compared to the marker and the source re-checked after the copy; bounded entry count, marker and sidecar sizes |
| Snapshot bytes → verification and osm2pgsql | nobody until verified | only the staged copy is read; the whole PBF is scanned (every blob framed within 64 KiB/32 MiB limits, raw or zlib decoded to its declared size, protobuf structure walked, trailing bytes and history files rejected); region box; provenance digest/size/license/box/timestamps; trustworthy data timestamp (sidecar or header, consistent, not in the future); **SHA-256 pinned in the region file or authorized by an operator, or (online deliveries only) signed by a key pinned in the source file**; re-hashed after osm2pgsql; osm2pgsql as non-root in a read-only container with a tmpfs and no NAT route out (online updates do not change this: downloads run in the separate fetcher) |
| Source file → fetcher and publisher | operator (reviewed configuration) | strict JSON (unknown fields and repeated keys rejected), https-only URLs without credentials/query/fragment, 1–8 Ed25519 keys with ids, bounded intervals; region must equal the publisher's; read again for every check and delivery (a removed key stops being trusted without a restart) |
| Snapshot file → command-line importer | the operator who runs it (host shell) | same staging copy and verification as the inbox, without the marker |
| Region config → importer | operator | strict JSON (unknown fields rejected), validated ids/boxes/digests; identifiers are never interpolated from free text into SQL |
| Operator → operator API | holders of a token, by scope | separate process and port from the public API (no operator routes there); 127.0.0.1 only by default; bearer tokens of 256 random bits compared by SHA-256 in constant time; per-credential scopes `status`, `publish`, `rollback`, `cleanup`; `POST` bodies `application/json`, ≤ 16 KiB, strict (unknown fields rejected), reasons bounded; per-request timeout; `no-store`; every action and every refusal audited with the credential name (refusals rate-limited); tokens and bodies never logged |
| Publisher/importer → database | importer role | not a superuser: `CREATEDB` only; it owns the registry and the release databases it creates; PostGIS comes from a template created at initialisation; the audit table rejects updates, deletes and truncation by trigger (the owner could drop the trigger: this protects against mistakes and application bugs, not against a compromised publisher) |
| API → database | nobody: API input is untrusted | `karta_api` is a member of `karta_reader` with `CONNECT` + `SELECT`/`EXECUTE` only (registry: releases, active pointer and schema version; not submissions, authorizations or audit); sessions default read-only at the role, the release database and the connection; `statement_timeout` 3 s (connection) and 5 s (role); only parameterized SQL; release databases are frozen `default_transaction_read_only = on` after import; a release is served only while the serving toolchain equals the recorded one (checked on every new connection) |
| Client → API | untrusted | `GET`/`HEAD`/`OPTIONS` only, bodies rejected, 16 KiB header limit, 5 s header / 10 s read / request deadline, strict parameter validation (unknown or repeated parameters rejected, UTF-8 and control characters checked, bounded lengths and numbers), LIKE metacharacters removed by normalization and escaped again, errors without internal details, access logs without query strings |
| Browser → demo | untrusted page context | CSP `default-src 'none'` with `script-src`/`connect-src` `'self'`, `frame-ancestors 'none'`; MapLibre and fonts served locally; no CDN, OSM tile or Nominatim access |

## Network exposure

* PostgreSQL publishes no port and sits on an `internal` network with no route
  outside Docker. The API is published on `127.0.0.1` by default
  (`KARTA_HTTP_BIND`) and the operator API on `127.0.0.1:8081`
  (`KARTA_OPERATOR_BIND`); put TLS termination in front of either for any
  other exposure, and expose the operator API only to operators.
* The publisher's extra network (`operator`) exists only to publish the
  operator port; it is a bridge with IP masquerading disabled, so the
  publisher (which parses untrusted snapshots) has no NAT route to outside
  hosts. This relies on Docker's bridge driver option; verify it on other
  runtimes.
* With online updates enabled (opt-in), the **fetcher** is the only
  container with a route out, on its own `egress` network; it is not on the
  backend network (no database access) and publishes no port. The publisher
  stays without a route out. `make test-integration` puts the fetcher on an
  internal network with only the local test source and checks its isolation
  with `scripts/check-isolated.sh`.
* The running API makes no outbound connections: it needs the database and
  nothing else, and keeps answering during an internet outage. `make
  test-offline` runs it with no external route. It checks the container's
  network attachments, the network's `internal` flag, published ports and
  default routes (`scripts/check-isolated.sh`, with a negative control on a
  frontend-like network), and checks the result from the browser's point of
  view too.
* CORS is off unless origins are listed in `KARTA_CORS_ALLOWED_ORIGINS`;
  credentials are never allowed.

## Secrets

Secret *names* are in `.env.example`; values live only in `./secrets/*` (random,
generated locally, git-ignored) and reach containers as Docker secrets. An
online source's bearer token (optional, `auth_token_file`) is a file outside
Git mounted into the fetcher only; it is sent only in the `Authorization`
header to the source's allowed hosts (redirects are refused, URLs may not
carry credentials or a query) and never logged or reported. Signing keys
never reach a deployment: it holds only public keys. Passwords are
read from files, never put in DSNs, command lines or logs; osm2pgsql receives
the importer password through a temporary 0600 pgpass file. Operator
credentials: `operator_token` and `operator_monitor_token` (raw 256-bit
tokens, mounted only into the one-off `operator-cli` container) and
`operator_tokens` (name, scopes and SHA-256 of each token: all the publisher
receives). See docs/runbook.md, "Credentials", for adding credentials and
rotation. The secret files
are 0644 inside a 0700 directory so the non-root container users can read the
bind-mounted files; on shared hosts or in production use the orchestrator's
secret store with per-service ownership. The database uses SCRAM-SHA-256.

## Containers

API: distroless static image, UID 65532, read-only root filesystem, all
capabilities dropped, `no-new-privileges`, 512 MiB / 1 CPU / 256 PIDs, health
check via the binary itself. Publisher and importer: Ubuntu 24.04 +
osm2pgsql 1.11.0, UID 10001, read-only root filesystem with a 1 GiB `/tmp`
tmpfs, all capabilities dropped, `no-new-privileges`, inputs mounted
read-only; the publisher writes only its `staging` volume (0700) and is
limited to 4 GiB / 2 CPUs / 256 PIDs. Fetcher (opt-in): the distroless API
image, UID 65532, read-only root filesystem, all capabilities dropped,
`no-new-privileges`, 256 MiB / 0.5 CPU / 64 PIDs, writes only its outbox
volume. PostgreSQL: official PostGIS image (drops to the `postgres` user),
`no-new-privileges`, memory/CPU/PID limits. All images are pinned by digest.

## Threat model: publication (Stage 2)

| Threat | Mitigation | Residual risk |
| --- | --- | --- |
| A partial or still-copying file is imported | completion marker written last; settle interval; digest of the staged copy must equal the marker; source re-checked after the copy | a producer that writes the marker before finishing gets `marker_digest_mismatch`, not a partial import |
| File swapped for a symlink (to `/etc/...` or another snapshot) or a FIFO between listing and reading | lstat-based rejection, `O_NOFOLLOW\|O_NONBLOCK` open, inode/size/mtime of the opened file compared with the listing, re-checked after the copy | none known on a local filesystem; network filesystems with weak inode semantics are not supported |
| Malicious or corrupt PBF (truncated, padded, zip-bomb blob, deep structure) | full structural scan with size limits before any import; zlib output bounded to the declared size (≤ 32 MiB per blob); osm2pgsql without privileges, no NAT route, memory/CPU/PID limits | a crafted file that passes the structural scan still reaches osm2pgsql (libosmium), which is exposed to parser bugs; the digest authorization means only operator-approved bytes get there |
| An arbitrary or merely newer snapshot is accepted | exact SHA-256 pinned in the region file or authorized by an operator (audited, optional size binding and expiry); no configuration accepts any file | an operator who authorizes a digest without verifying it out of band |
| Replay of an older snapshot, or of a retired release | forward-only rule by trusted data timestamp; duplicates never switch; rollback is a separate, scoped, audited action | — |
| Forged provenance or timestamps | sidecar digest/size/box/timestamp consistency; header and sidecar timestamps must agree; future and pre-2004 timestamps refused | the sidecar's claims about the original source (`source`, `source_sha256`) cannot be verified without that source and are recorded only as claims |
| Wrong region published | region box check (header or sidecar); region changes need an explicit flag | — |
| Failed or malicious candidate alters active data | builds only in a fresh candidate database; the active pointer moves only in the audited compare-and-swap transaction after every check; release databases are read-only after freezing | — |
| A well-formed but much smaller snapshot (a bad extract) silently replaces good data | absolute `min_counts`; relative `max_drop_fraction` checked at validation and again inside the pointer transaction against the release active at every forward switch (a release validated against another active release is re-judged); row counts that cannot be read fail closed (`counts_unavailable`) | an operator who raises the fraction, or rolls back deliberately (rollback has no count gate); a loss spread evenly under the fraction |
| Operator API abuse | not on the public listener; loopback by default; scoped tokens; strict, bounded requests; audit; denials rate-limited in the audit log | plain HTTP: expose only behind TLS; static tokens rotate by restart; no per-client rate limit (256-bit tokens make guessing infeasible) |
| Token or secret disclosure through logs | tokens never logged (only the credential name); bodies and query strings not logged; the publisher holds only token hashes | the raw tokens exist in `./secrets` on a development host |
| Disk exhaustion breaks serving | capacity checks before staging and before a build; builds fail cleanly on ENOSPC and drop their candidate; release databases are read-only; optional separate tablespace | a disk filled by something else can still stop PostgreSQL itself |
| Removal of a release in use | never the active release, never inside its pin grace plus margin, never with a database session, `removing` state under a row lock excludes rollback | — |
| Interrupted publication leaves inconsistent state | recovery at every start under the build lock; the pointer only moves in a committed transaction | — |
| Immutable URL serves different bytes after a database upgrade | the serving toolchain must equal the recorded one (per connection); otherwise the release is refused | — |

## Threat model: online updates (Stage 3)

| Threat | Mitigation | Residual risk |
| --- | --- | --- |
| The source (or anyone controlling its host, DNS or a TLS-intercepting path) serves a malicious or wrong snapshot | the snapshot's digest, size, data timestamp, region and box must be signed by a key pinned in the reviewed source file; the location, TLS and any published checksum authorize nothing; then every Stage 2 check | a compromised **signing key** can publish any snapshot that passes the Stage 2 checks until the key is removed; `require_operator_authorization` removes unattended publication for owners who prefer that |
| A checksum downloaded from the same location bypasses digest authorization | the inbox and CLI never read manifests; online deliveries are authorized only by a signature from a pinned key (or additionally by Stage 2 authorization); the fetcher's marker digest is only a completeness check | — |
| Replay of an older signed manifest (freeze or rollback attack) | serial must not decrease (fetcher state, saved before any further work so a crash cannot forget it, and the fetcher refuses to start from a state file it cannot read; authoritatively, the registry); a different envelope under the same serial is refused; validity window bounded by `max_manifest_validity`; the forward rule refuses older data | a source can withhold updates until the newest manifest it served expires; data age is monitored (`karta_data_stale`) |
| A manifest expires, or its key is removed, while its snapshot is being built | the automatic switch verifies the exact envelope again inside the pointer transaction, under the source and region files in force and at the current time (and, with `require_operator_authorization`, the pin or authorization); any failure refuses the switch, the active release stays, and the candidate stays `ready` for review, retry or a fresh manifest | a key removed after the check ran but before the commit (the same transaction, milliseconds); an operator who activates the candidate explicitly |
| Conflicting or stale source claims | signed data timestamp must equal the timestamp derived from the snapshot and not be after `issued_at`; expired or future manifests refused; region/box binding; signed sidecar digest | — |
| Server-side request forgery: the source or DNS points the fetcher at local or private services (cloud metadata, the Docker host, other containers), including by redirect or re-resolution | https only; allowed hosts only; no redirects; the resolved address is checked when each connection is made; non-public ranges refused unless explicitly allowed; proxy variables ignored; the fetcher is not on the backend network | an `allowed_networks` entry is trusted as configured |
| Resource exhaustion by the source (huge, endless, compressed or slow responses, many retries) | byte limits on every response, the signed size as a hard cap, no decompression, bounded header size; connect/handshake/header/total/stall timeouts; one download at a time; exponential backoff with jitter; per-snapshot attempt cap; disk reserve check; at most two deliveries and one partial download kept | a slow source can delay updates (visible as failures and data age) |
| Partial or corrupted download imported | download to a private partial file; resume only with `If-Range` and an exact `Content-Range`; full-file SHA-256 must equal the signed digest or the file is deleted; delivered by atomic link and marker; the publisher re-hashes the staged copy | — |
| A compromised fetcher (it faces the network) | no database credential, no listener, read-only root, no capabilities, only its outbox writable; the publisher verifies every delivery's signature, serial and bytes itself (tested with forged deliveries) | it can withhold updates and misreport its own check times; alert on data age and on the age of its state |
| Online automation overrides an operator decision | compare-and-swap against the active release seen at build start; a rollback pauses automatic online activation in the same transaction; pause and resume are explicit, audited, and serialized with automatic switches by a row lock | an operator who resumes too early |
| Source credentials leak | bearer token only from a secret file, only in the `Authorization` header to allowed hosts, never in URLs (queries and userinfo refused), logs, status or the public API (tested) | the token file on the host |
| Source outages block manual publication or serving | the fetcher is a separate process; the inbox, the CLI importer and serving do not depend on it (tested with the network cut and the source stopped) | — |

## Known limitations

* No TLS inside the Docker network (`KARTA_DB_SSLMODE=disable`); set
  `verify-full` when PostgreSQL runs on another host.
* No rate limiting in the API; put a reverse proxy with limits in front of
  public deployments. Expensive queries are bounded by the statement timeout.
* `govulncheck` needs vuln.go.dev; it runs in CI (see the PR for what could be
  run in the implementation environment).
* The fetcher does not support a forward proxy (it needs direct HTTPS egress
  to the source). Check times it reports are not authenticated.
* No production online source, signing-key owner or staleness threshold is
  configured; see docs/adr/0004-stage3-online-updates.md, "Consequences and
  limits".
* The operator API has no TLS and no per-client rate limiting of its own;
  bind it to loopback (the default) or put a TLS proxy with access control in
  front.
