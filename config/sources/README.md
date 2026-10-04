# Online source files (Stage 3, opt-in)

This directory holds **online source files**: one JSON file per online
snapshot source, mounted read-only into the publisher and the fetcher at
`/config/sources`. Online updates are off unless `KARTA_ONLINE_SOURCE_FILE`
names one of these files (see `docs/runbook.md`, "Online updates").

**No production source is configured.** The owner has not yet chosen a
production HTTPS provider, a signing-key owner or a permitted update lag;
these are deployment decisions, made by committing a reviewed source file
here (or providing one through the deployment's configuration). The
acceptance tests generate their own source file, test CA and test key at run
time; nothing in Git is trusted for unattended updates.

A source file is part of the trust configuration: its `trusted_keys` decide
which signatures authorize a snapshot, so change it only through review.
Unknown fields are rejected.

```json
{
  "region_id": "tehran-chitgar",
  "manifest_url": "https://extracts.example.org/karta/tehran-chitgar/manifest.json",
  "allowed_hosts": ["downloads.example.org"],
  "trusted_keys": [
    { "id": "owner-2026a", "ed25519_public_key": "<base64 of the 32-byte public key>" }
  ],
  "poll_interval": "1h",
  "max_manifest_validity": "168h"
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `region_id` | yes | must equal the `id` of the region the deployment serves |
| `manifest_url` | yes | `https://` URL of the signed manifest; no credentials, query or fragment |
| `allowed_hosts` | no | further `host[:port]` the manifest may point snapshots to (the manifest's host is always allowed; no port means 443) |
| `allowed_networks` | no | CIDR prefixes the fetcher may connect to although they are not public (a private mirror); every other loopback, private, link-local, shared, reserved or multicast address is refused |
| `ca_file` | no | absolute path of a PEM bundle: the only roots trusted for this source (default: system roots) |
| `auth_token_file` | no | absolute path of a secret file holding a bearer token sent in the `Authorization` header (never in URLs or logs) |
| `trusted_keys` | yes | 1 to 8 Ed25519 public keys (`id`, `ed25519_public_key`, optional `not_after`) |
| `require_operator_authorization` | no | `true` keeps the Stage 2 rule for online snapshots: a valid signature **and** a pinned or operator-authorized digest |
| `poll_interval` | yes | time between successful checks |
| `max_manifest_validity` | yes | longest `expires_at - issued_at` accepted |
| `download_timeout` | no | bound on one snapshot download (1h) |
| `stall_timeout` | no | abort a download that receives nothing for this long (60s) |
| `retry_initial`, `retry_max` | no | exponential backoff with jitter after a failed check (1m, 1h) |
| `max_download_attempts`, `abandon_for` | no | after this many failed downloads of one snapshot, set it aside for this long (5, 24h) |

Producers sign manifests with `cmd/karta-sign` (or any DSSE/Ed25519
implementation following `docs/adr/0004-stage3-online-updates.md`).

## A co-located bridge (Stage 5)

With the controlled source bridge on the same host (`compose.bridge.yaml`,
`docs/runbook.md`, "Controlled source bridge"), the fetcher reaches only
`bridge-serve` on the no-NAT `bridge` network. Its source file trusts the
bridge's own CA (`make bridge-tls` writes `bridge-ca.pem` here,
git-ignored), allows exactly the `bridge` subnet, and pins the bridge's
public key (`go run ./cmd/karta-sign pubkey --key KEY.pem`, run by the key's
custodian):

```json
{
  "region_id": "iran",
  "manifest_url": "https://bridge-serve:8443/manifest.json",
  "allowed_networks": ["10.233.0.0/24"],
  "ca_file": "/config/sources/bridge-ca.pem",
  "trusted_keys": [
    { "id": "bridge-iran-KEYID", "ed25519_public_key": "<base64 of the bridge's 32-byte public key>" }
  ],
  "poll_interval": "15m",
  "max_manifest_validity": "192h"
}
```

`max_manifest_validity` must be at least the bridge's `manifest_validity`
(7 days in `config/bridge/signer.example.json`, plus margin). The bridge's
assets are on the manifest's own host, so no `allowed_hosts` are needed. A
bridge on a separate host is an ordinary HTTPS source: its DNS name, its
CA (or a public certificate) and no `allowed_networks` unless it is on a
private network.
