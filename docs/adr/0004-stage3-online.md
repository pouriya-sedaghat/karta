# ADR 0004: Signed online snapshots and hybrid publication

Status: draft implementation, 2026-09-30. Stage 3 acceptance is pending
end-to-end fixture and failure-recovery evidence. This ADR records registry
schema version 3 and the operator API additions before either is merged.
The Stage 2 public API can still read the registry's release pointer after
this migration; an older Stage 2 publisher/importer refuses schema version 3
and must be upgraded or restored with a database backup before rollback.

## Source trust

Online polling is opt-in per deployment, for the deployment's configured
region. There is no built-in production URL or key. The owner pins an Ed25519
public key in a local read-only file and configures one HTTPS manifest URL.
The source signs the exact canonical JSON bytes of a manifest payload; an
envelope contains base64 `payload` and `signature`. Unknown, duplicate or
noncanonical payload fields are refused. The payload binds `region_id`,
`issued_at`, `expires_at`, `data_timestamp`, exact snapshot URL, byte length
and SHA-256, plus the corresponding URL, length and digest for a provenance
sidecar when supplied. The signature is verified *before* following any
payload URL. The snapshot digest authorizes only this online submission;
manual inbox and CLI submissions still need region pins or audited operator
authorization. HTTPS and a checksum from the same origin do not grant that
authorization. The full Stage 2 PBF/provenance checks must agree with the
signed region, digest, size and timestamp before a build.

Rotate a key by distributing a new public key file and restarting the
publisher only after confirming the source signs with the matching private
key. A wrong or expired key fails closed, keeps the active release, and is
shown as a bounded error code. There is no remote key discovery or fallback
to an unsigned checksum. Treat private signing keys as publication
credentials; keep them outside Karta and Git. The public key may be kept in
an operator-managed read-only file.

## Network and resource boundary

The base Compose stack has no publisher egress. An explicit
`compose.online.yaml` overlay adds an outbound network to that process; the
public API remains unchanged. The client disables proxies and redirects,
requires one configured HTTPS origin and ordinary TLS verification, resolves
before each connection, and refuses any DNS answer containing a loopback,
private, link-local or other non-global address. No credentials in URLs or
query strings are accepted. This restricts the application's destinations;
operators should also apply an egress firewall for the chosen source.

The manifest is at most 16 KiB. The snapshot has a configured maximum byte
count no greater than the importer limit, and the signed length and digest
must match the completed response. The provenance sidecar is at most 1 MiB.
Compression and redirects are refused, including unknown-length transfers
that pass the limit. A total network deadline covers manifest and data,
with short connection and header deadlines. A conservative staging space
check includes the snapshot, sidecar and reserve. Partial files live only
in a private 0700 directory and are deleted on error or startup recovery;
there is no resumable partial file. One poll worker runs per publisher.

## Publication, state and recovery

The online worker hands completed staged bytes to the existing Verify,
forward policy, capacity check, isolated Build and compare-and-swap Activate
path. A DB submission is keyed by region and digest. Registry version 3
allows `online` submission/audit sources and adds a singleton `online_state`
with paused policy, last attempt/check, last verified digest/timestamp,
sanitized last error, next attempt and consecutive failures. Crash recovery
marks an in-progress submission interrupted and clears staging; subsequent
polls retry under the existing attempt limit. Failed checks back off from
minutes to six hours with bounded jitter; successful checks use the
configured interval. A different digest at the same signed timestamp, an
older source timestamp, or conflicting claims for the same digest are
refused across restarts.

The last successful check records a verified signed manifest before its
download starts. A later transfer or build failure remains visible as the
last attempt error; the signed source claim remains durable and the same
digest may retry. A manifest that expires during a slow build cannot
authorize an automatic pointer switch. The ready release remains available
for a retry under a fresh signed manifest, within the attempt limit.

The build lock serializes manual and online candidates. The forward policy
and pointer CAS decide which valid candidate activates; a build that loses
the CAS stays ready. An explicit operator activation or rollback sets
`online_state.paused` in the pointer transaction, even when it is a no-op.
An online activation checks the pause bit under that same lock. Authenticated
publish-scope pause/resume actions require a reason and produce audit rows.
Resume permits a verified ready candidate stopped by the pause to retry.
Manual inbox and CLI publication work without the online network.

Operator status and authenticated text metrics distinguish the last
successful signed source check from active release age, failures and the
next attempt. No upstream URL is included in those fields. Public manifest
capabilities remain conservative until online end-to-end acceptance passes;
there is no claim that a retained old release proves a successful check.

## Open decisions and acceptance boundary

The production HTTPS origin, signing key owner/rotation process, permitted
update lag, egress firewall and target resource budget need owner decisions.
Unit tests cover the signature, origin, size, redirect, transfer and timeout
rules. The integration suite now drives a real Compose publisher with a signed
local HTTPS fixture, failed downloads, duplicates, rollback, crash recovery,
manual publication during source failure, a mismatched signed provenance
sidecar, expiration during a build, and serving load. The disposable
`onlinefixture` test image alone can route `example.com` to the host fixture;
the production image retains its private-address refusal. CI results and an
independent review are required before marking the PR ready. The suite does
not establish a production provider's trust, availability or resource budget;
do not deploy the online overlay to the owner's VM on the basis of this ADR.
