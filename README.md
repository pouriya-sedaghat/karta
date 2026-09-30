# Karta

Karta is a self-hosted, reusable OpenStreetMap map and place-search service. Its intended dependency direction is `apps -> core -> karta`: clients use Karta's versioned HTTP API through core; Karta does not import application or core code.

Current state: **Stage 2, safe manual publication**, under review, on top of Stage 1 (offline map display and named-place/POI search). The active OSM snapshot (a *release*) is served as vector tiles with a local MapLibre style, Persian-capable glyphs and search. An operator publishes new snapshots **while serving** through a local inbox (or the command line): each is verified completely, its digest must be pinned or operator-authorized, it is built in an isolated candidate database and activated in one audited transaction; replaced releases stay available to pinned clients for a grace period and for rollback; an authenticated operator API (separate from the public API) gives status, rollback and cleanup. Online updates (Stage 3), address geocoding, reverse geocoding and routing are **not** implemented. See [architecture](docs/architecture.md), [delivery stages](docs/delivery-plan.md), [Stage 1 decisions](docs/adr/0002-stage1-implementation.md), [Stage 2 decisions](docs/adr/0003-stage2-publication.md), [runbook](docs/runbook.md), [API](docs/api.md), [OpenAPI contract](openapi/openapi.yaml) and [operator API](openapi/operator.yaml), [map data and style](docs/map-data.md), [security and data flow](docs/security.md), and [Tehran data](docs/development-data.md).

```bash
make secrets build up        # PostgreSQL/PostGIS, API, publisher; readiness is 503 until a publication
make import-fixture          # committed synthetic fixture (CC0), command-line publication
make wait-ready
open http://localhost:8080/demo/
make op-status               # operator view: releases, submissions, storage
make test test-integration test-browser test-offline
```

The real Chitgar Lake sample is not in Git; copy the supplied PBF and sidecar to `data/local/`, then `make reset up publish-tehran wait-ready test-browser-tehran` (inbox publication; the publisher's default region is Chitgar).

All runtime assets and data are local: after images are built, the stack needs no network, and the demo page cannot contact any other host. OpenStreetMap data is licensed under ODbL; every map shows `© OpenStreetMap contributors` with a license link. Karta serves its own tiles; public OSM tile and Nominatim endpoints are not production backends. Bundled font: Vazirmatn (SIL OFL 1.1); map renderer: MapLibre GL JS (BSD-3-Clause, installed from npm with pinned integrity).
