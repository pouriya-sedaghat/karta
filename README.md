# Karta

Karta is a self-hosted, reusable OpenStreetMap map and place-search service. Its intended dependency direction is `apps -> core -> karta`: clients use Karta's versioned HTTP API through core; Karta does not import application or core code.

Current state: **Stage 1, offline map display and named-place/POI search**, under review. One imported OSM snapshot (a *release*) is served as vector tiles with a local MapLibre style, Persian-capable glyphs and search. Manual and online update with release switching (Stages 2 and 3), address geocoding, reverse geocoding and routing are **not** implemented. See [architecture](docs/architecture.md), [delivery stages](docs/delivery-plan.md), [Stage 1 decisions](docs/adr/0002-stage1-implementation.md), [runbook](docs/runbook.md), [API](docs/api.md) and [OpenAPI contract](openapi/openapi.yaml), [map data and style](docs/map-data.md), [security and data flow](docs/security.md), and [Tehran data](docs/development-data.md).

```bash
make secrets build up        # PostgreSQL/PostGIS + API; readiness is 503 until an import
make import-fixture          # committed synthetic fixture (CC0)
make wait-ready
open http://localhost:8080/demo/
make test test-integration test-browser test-offline
```

The real Chitgar Lake sample is not in Git; copy the supplied PBF and sidecar to `data/local/`, then `make reset up import-tehran test-browser-tehran`.

All runtime assets and data are local: after images are built, the stack needs no network, and the demo page cannot contact any other host. OpenStreetMap data is licensed under ODbL; every map shows `© OpenStreetMap contributors` with a license link. Karta serves its own tiles; public OSM tile and Nominatim endpoints are not production backends. Bundled font: Vazirmatn (SIL OFL 1.1); map renderer: MapLibre GL JS (BSD-3-Clause, installed from npm with pinned integrity).
