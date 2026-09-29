# Karta HTTP API v1

The contract is [`openapi/openapi.yaml`](../openapi/openapi.yaml) (also served at
`/v1/openapi.yaml`); integration tests validate live responses against it.

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/manifest` | active `release_id`, region, OSM data timestamp, style URL, tile template, search limits, attribution, capabilities |
| `GET /v1/search?q=&limit=&lang=&bbox=&release_id=` | named places and POIs (not streets or addresses) |
| `GET /v1/releases/{release_id}/styles/{style_id}.json` | MapLibre style of the release, at a content-addressed URL (the manifest's `style_url`) |
| `GET /v1/releases/{release_id}/style.json` | `307` to the release's current content-addressed style URL (`no-cache`) |
| `GET /v1/releases/{release_id}/tiles/{z}/{x}/{y}.pbf` | vector tile, z 0–16 |
| `GET /v1/fonts/{fontstack}/{range}.pbf` | glyph ranges (`Vazirmatn Regular`, `Vazirmatn Bold`) |
| `GET /health/live`, `GET /health/ready` | liveness; readiness with `reason` |
| `GET /demo/` | MapLibre demo (same-origin, strict CSP) |

## Search

```bash
curl -G http://localhost:8080/v1/search --data-urlencode 'q=Chitgar Lake' --data-urlencode limit=2
```

```json
{"release_id":"rf210a8fe8237f20095686597","query":"Chitgar Lake","normalized_query":"chitgar lake","limit":2,"lang":null,"bbox":null,
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
| 404 | `unknown_release`, `unknown_style`, `tile_zoom_out_of_range`, `unknown_fontstack`, `not_found` |
| 405 / 413 | `method_not_allowed`, `request_body_not_allowed` |
| 503 | `no_active_release`, `service_unavailable`, `timeout` (with `Retry-After`) |
| 500 | `internal_error` |

## Caching

| Resource | Cache-Control | Validator |
| --- | --- | --- |
| tiles (200 and empty 204) | `public, max-age=31536000, immutable` | `ETag "<release_id>-<z>-<x>-<y>"` |
| style (`styles/{style_id}.json`) | `public, max-age=31536000, immutable` | `ETag "<style_id>"` |
| `style.json` redirect | `no-cache` | — |
| glyphs, openapi.yaml | `public, max-age=86400` | `ETag` derived from the response bytes |
| demo page / vendored MapLibre | `no-cache` / `public, max-age=86400` | `ETag` from file content; no `Last-Modified` (npm files carry a fixed mtime) |
| manifest | `no-cache` (always revalidate) | content `ETag` |
| search | `no-cache`; `public, max-age=300` when `release_id` is given | — |
| errors | `no-store` | — |

URLs are the cache keys, and an immutable URL never names different bytes,
so a new release, API build or `KARTA_PUBLIC_BASE_URL` needs no cache purge.
The one exception is the transition from builds before content-addressed
styles ([below](#transition-from-builds-before-content-addressed-styles)).

* **Tiles** are keyed by the release id, which covers every input that
  shapes the release's data (snapshot and provenance digests, data
  timestamp, region id/name/box/default view, schema and style revisions,
  attribution, osm2pgsql/PostgreSQL/PostGIS/GEOS/PROJ/ICU versions; see
  `karta.release_info.identity`), hashed exactly as served (floats
  losslessly, not rounded). A new release has a new id and new URLs.
* **Styles** are keyed by their own bytes: `style_id` is the first 128 bits
  of SHA-256 over the style the server sends. The style also depends on the
  API itself (its renderer) and on `KARTA_PUBLIC_BASE_URL`, which it embeds
  in its tile and glyph URLs. When either changes, an existing release keeps
  its id and tile URLs but gets a new style URL, the manifest (always
  revalidated) issues it, and the old style URL returns `404 unknown_style`
  instead of different bytes. Existing release databases need no migration:
  the running API renders them and names its own output. A client that
  fetched the manifest just before such a change can get `404
  unknown_style` (or `404 unknown_release` after a release switch) for the
  style URL it holds: refetch the manifest and load the style it names, a
  bounded number of times. The demo does this (three attempts,
  `web/app.js`).
* Glyph ranges, `openapi.yaml` and the vendored demo files are not
  immutable: after an API upgrade that changes them, a client may keep the
  previous bytes for up to 24 hours before revalidating against the new
  content ETag.

Tile bytes are generated by `karta.tile` in the release database on the
serving PostgreSQL. The release id records the toolchain at import, but the
API does not yet check that the serving PostgreSQL/PostGIS/GEOS/PROJ match
it, so upgrade the database image only together with a reset and re-import
(see `docs/runbook.md`).

### Transition from builds before content-addressed styles

Builds before `eab072b` answered `GET /v1/releases/{release_id}/style.json`
with the style itself, `Cache-Control: public, max-age=86400` and a content
ETag. A browser, proxy or CDN that stored such a response may keep using it,
without contacting the server, until 24 hours after it fetched it. The
server cannot clear those copies: the `no-cache` `307` only applies to
requests that reach it. When a copy expires, the cache revalidates, gets the
`307` (never a `304`, which would renew the old bytes) and loads the
content-addressed URL. Manifest-issued style URLs are not affected.

While old copies may still be in use:

* They embed absolute tile and glyph URLs under the `KARTA_PUBLIC_BASE_URL`
  the old build ran with. For 24 hours after the upgrade, both must keep
  serving what those URLs named:
  * **The same release.** Keep it active (no reset or re-import). Otherwise
    clients that use `style.json` directly get `404 unknown_release` for tiles
    until their copy expires.
  * **The old base URL.** If the upgrade also changes `KARTA_PUBLIC_BASE_URL`
    (scheme, host, port or path prefix), keep the old one reachable and
    serving `/v1/releases/{release_id}/tiles/…` and `/v1/fonts/…` as before,
    including its CORS origins. Routing it to the upgraded API (stripping any
    path prefix, as before) is enough, because the API matches requests by
    path, not by host.

  If the old base URL can't be kept available, copies that browsers cached
  still request tiles and glyphs there. Those requests fail, and the map
  stays blank or unlabelled, until the copy expires, up to 24 hours after it
  was fetched.
* Purge `/v1/releases/*/style.json` in any reverse proxy or CDN you operate.
  That shortens the window only for clients behind it. It does not clear
  browser caches, and it doesn't help clients whose copy names an old base
  URL that is gone.

Deployed consumers: no external deployment is known. As of 2026-09-29 the
Stage 1 code is not on `main`, and GitHub has no tags, releases or published
images; CI builds images only for its own test run. That can't rule out a
build from this branch running elsewhere, and the repository owner has been
asked to confirm there is none. Until that is confirmed, anyone upgrading such
a build should follow the steps above.

## CORS

Off by default. `KARTA_CORS_ALLOWED_ORIGINS=https://app.example,https://ops.example`
(or `*`) allows `GET`/`HEAD` from those origins, answers preflights with a
10-minute max age, exposes `ETag` and `X-Request-ID`, and never allows
credentials. Responses carry `Vary: Origin`.

## Compatibility

`/v1` is not frozen until its first release: while Stage 1 is a draft its
contract can still change incompatibly. It did once: `eab072b` turned
`GET /v1/releases/{release_id}/style.json` from a `200` style (cacheable for a
day) into a `307` to the content-addressed style URL (see
[the transition](#transition-from-builds-before-content-addressed-styles)).
From the first release on, changes within `/v1` are additive only: new
endpoints, optional parameters, response fields and error codes may appear;
nothing existing is removed or redefined. Clients must ignore unknown fields. Tile layers and fields follow
the schema major version (`schema_revision` in the manifest); removing or
renaming a layer or field requires a new major version and an ADR.
