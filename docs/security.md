# Data flow, trust boundaries and security assumptions (Stage 2)

```mermaid
flowchart LR
    subgraph Operator host
        INBOX["inbox (data/inbox)<br/>read-only mount"]
        PBF["data/local, testdata<br/>(read-only, CLI import)"]
        CFG["region config<br/>(config/regions, read-only)"]
        SEC["./secrets (0700)"]
        OPR["operator: make op / curl<br/>bearer token"]
    end
    subgraph "Docker network: backend (internal, no external route)"
        PUB["publisher (UID 10001, read-only FS)<br/>scan → stage → verify → build → switch<br/>operator API :8081"]
        IMP["importer (UID 10001, on demand)"]
        DB[("PostgreSQL/PostGIS<br/>karta_registry + karta_r… release DBs")]
        API["API (distroless, UID 65532, read-only FS)"]
    end
    STG[("staging volume<br/>0700, publisher only")]
    Client["Browser / core / apps"]
    INBOX --> PUB
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
| Inbox → publisher | nobody: anyone who can write the inbox directory can submit files; nothing is imported unless its digest is pinned or authorized | read-only mount; completion marker required before any file is opened; names restricted; lstat only until then; symlinks, directories, devices, FIFOs rejected without opening; `O_NOFOLLOW\|O_NONBLOCK` open checked against the listed inode; settle interval; copy into a private 0700 staging volume with the digest compared to the marker and the source re-checked after the copy; bounded entry count, marker and sidecar sizes |
| Snapshot bytes → verification and osm2pgsql | nobody until verified | only the staged copy is read; the whole PBF is scanned (every blob framed within 64 KiB/32 MiB limits, raw or zlib decoded to its declared size, protobuf structure walked, trailing bytes and history files rejected); region box; provenance digest/size/license/box/timestamps; trustworthy data timestamp (sidecar or header, consistent, not in the future); **SHA-256 pinned in the region file or authorized by an operator**; re-hashed after osm2pgsql; osm2pgsql as non-root in a read-only container with a tmpfs and no NAT route out |
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
generated locally, git-ignored) and reach containers as Docker secrets. Passwords are
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
limited to 4 GiB / 2 CPUs / 256 PIDs. PostgreSQL: official PostGIS image (drops to the `postgres` user),
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
| Operator API abuse | not on the public listener; loopback by default; scoped tokens; strict, bounded requests; audit; denials rate-limited in the audit log | plain HTTP: expose only behind TLS; static tokens rotate by restart; no per-client rate limit (256-bit tokens make guessing infeasible) |
| Token or secret disclosure through logs | tokens never logged (only the credential name); bodies and query strings not logged; the publisher holds only token hashes | the raw tokens exist in `./secrets` on a development host |
| Disk exhaustion breaks serving | capacity checks before staging and before a build; builds fail cleanly on ENOSPC and drop their candidate; release databases are read-only; optional separate tablespace | a disk filled by something else can still stop PostgreSQL itself |
| Removal of a release in use | never the active release, never inside its pin grace plus margin, never with a database session, `removing` state under a row lock excludes rollback | — |
| Interrupted publication leaves inconsistent state | recovery at every start under the build lock; the pointer only moves in a committed transaction | — |
| Immutable URL serves different bytes after a database upgrade | the serving toolchain must equal the recorded one (per connection); otherwise the release is refused | — |

## Known limitations

* No TLS inside the Docker network (`KARTA_DB_SSLMODE=disable`); set
  `verify-full` when PostgreSQL runs on another host.
* No rate limiting in the API; put a reverse proxy with limits in front of
  public deployments. Expensive queries are bounded by the statement timeout.
* `govulncheck` needs vuln.go.dev; it runs in CI (see the PR for what could be
  run in the implementation environment).
* The operator API has no TLS and no per-client rate limiting of its own;
  bind it to loopback (the default) or put a TLS proxy with access control in
  front.
