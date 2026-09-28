# Karta

Karta is a self-hosted, reusable OpenStreetMap map and place-search service. Its intended dependency direction is `apps -> core -> karta`: clients use Karta's versioned HTTP API through core; Karta does not import application or core code.

Current state: **architecture baseline only**. No runnable service or OSM dataset has been committed yet. Start with [architecture](docs/architecture.md), [delivery stages](docs/delivery-plan.md), and the [first Claude Code implementation brief](prompts/phase-1.md).

The initial product scope is map display and search for named places and points of interest. Address geocoding, reverse geocoding, and routing are separate future decisions. All runtime assets and data must be available locally for disconnected operation.

OpenStreetMap data is licensed under ODbL. Applications displaying the map must show OpenStreetMap contributor attribution and a license link. Karta must serve its own tiles; public OSM tile and Nominatim endpoints are not production backends.
