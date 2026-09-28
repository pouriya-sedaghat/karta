# Karta HTTP API v1

The contract is [`openapi/openapi.yaml`](../openapi/openapi.yaml) (also served at
`/v1/openapi.yaml`); integration tests validate live responses against it.

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/manifest` | active `release_id`, region, OSM data timestamp, style URL, tile template, search limits, attribution, capabilities |
| `GET /v1/search?q=&limit=&lang=&bbox=&release_id=` | named places and POIs (not streets or addresses) |
| `GET /v1/releases/{release_id}/style.json` | MapLibre style pinned to the release |
| `GET /v1/releases/{release_id}/tiles/{z}/{x}/{y}.pbf` | vector tile, z 0–16 |
| `GET /v1/fonts/{fontstack}/{range}.pbf` | glyph ranges (`Vazirmatn Regular`, `Vazirmatn Bold`) |
| `GET /health/live`, `GET /health/ready` | liveness; readiness with `reason` |
| `GET /demo/` | MapLibre demo (same-origin, strict CSP) |

## Search

```bash
curl -G http://localhost:8080/v1/search --data-urlencode 'q=Chitgar Lake' --data-urlencode limit=2
```

```json
{"release_id":"r37a15d199da7e2c601a3e68c","query":"Chitgar Lake","normalized_query":"chitgar lake","limit":2,"lang":null,"bbox":null,
 "results":[{"id":"way/1259635603","osm_type":"way","osm_id":1259635603,"display_name":"دریاچه چیتگر",
   "names":{"name":"دریاچه چیتگر","name:en":"Chitgar Lake","name:fa":"دریاچه چیتگر"},
   "category":"tourism","subcategory":"attraction","lon":51.2149387,"lat":35.745582,"bbox":[…],
   "importance_rank":5,"match":{"key":"name:en","value":"Chitgar Lake","language":"en","type":"exact"}}, …]}
```

* `q`: 1–100 characters (≤ 400 bytes), no control characters, at least one
  letter or digit after normalization. `limit`: 1–50 (default 10). `lang`:
  `[a-z]{2,3}`, restricts matches to `name:<lang>`-style tags and prefers
  `name:<lang>` as display name. `bbox`: `west,south,east,north`.
  `release_id`: search a specific release (Stage 1: the active one only).
* Normalization and the deterministic ranking are described in the OpenAPI
  `search` operation. `match.language` is `null` for tags without a language
  suffix (such as `name`), whose language OSM does not declare.

## Errors

Every error is JSON with `Cache-Control: no-store`:

```json
{"error":{"code":"invalid_parameter","message":"limit must be an integer 1..50","parameter":"limit","request_id":"6f1c…"}}
```

| Status | Codes |
| --- | --- |
| 400 | `invalid_parameter`, `unknown_parameter`, `invalid_query`, `invalid_tile_coordinates` |
| 404 | `unknown_release`, `tile_zoom_out_of_range`, `unknown_fontstack`, `not_found` |
| 405 / 413 | `method_not_allowed`, `request_body_not_allowed` |
| 503 | `no_active_release`, `service_unavailable`, `timeout` (with `Retry-After`) |
| 500 | `internal_error` |

## Caching

| Resource | Cache-Control | Validator |
| --- | --- | --- |
| tiles (200 and empty 204) | `public, max-age=31536000, immutable` | `ETag "<release_id>-<z>-<x>-<y>"` |
| style.json, glyphs, openapi.yaml | `public, max-age=86400` | content `ETag` |
| manifest | `no-cache` (always revalidate) | content `ETag` |
| search | `no-cache`; `public, max-age=300` when `release_id` is given | — |
| errors | `no-store` | — |

Release-pinned URLs are the cache keys: a new release has a new id and thus
new style and tile URLs, so no cache needs purging when the active release
changes. The style embeds `KARTA_PUBLIC_BASE_URL`; changing that value
requires purging cached styles.

## CORS

Off by default. `KARTA_CORS_ALLOWED_ORIGINS=https://app.example,https://ops.example`
(or `*`) allows `GET`/`HEAD` from those origins, answers preflights with a
10-minute max age, exposes `ETag` and `X-Request-ID`, and never allows
credentials. Responses carry `Vary: Origin`.

## Compatibility

Within `/v1` changes are additive only: new endpoints, optional parameters,
response fields and error codes may appear; nothing existing is removed or
redefined. Clients must ignore unknown fields. Tile layers and fields follow
the schema major version (`schema_revision` in the manifest); removing or
renaming a layer or field requires a new major version and an ADR.
