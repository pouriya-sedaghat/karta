# Delivery and review plan

Every development stage ends in a small pull request. Claude Code writes implementation and tests; the project review inspects the actual diff, checks integration behavior, asks for corrections if needed, and merges only after explicit owner decision. Review is keyed to PR creation/updates for this repository. No code or tests exist at the architecture-baseline stage.

| Stage | Deliverable | Acceptance evidence / review gate |
| --- | --- | --- |
| 0: baseline | Architecture, API outline, dependencies, assumptions, first implementation brief | Repository contains these documents; unresolved region/traffic decisions are visible. |
| 1: offline vertical slice | Local PostGIS/osm2pgsql flex, named-place search, vector tile/style, HTTP API, tiny test extract, reproducible runbook | With networking disabled after setup, manifest, search, tile and map demo work; runtime makes no outbound requests. Integration tests prove API shape and Persian Unicode name preservation. |
| 2: safe manual publication | Configurable inbox, full snapshot validation, isolated candidate, release registry, atomic activation, pinning, rollback | Requests remain successful during import; broken/truncated PBF never changes active release; a valid new PBF activates consistently across tiles and search; rollback works. |
| 3: online updates | Configured HTTPS source polling/download, metadata/digest/provenance, shared publication path | A newer source activates; unavailable network leaves previous release healthy; duplicates and mismatched regions are rejected. |
| 4: production hardening | Authenticated admin operations, observability, cleanup, resource limits, deployment and recovery instructions, load/fault rehearsal | Demonstrated limits and capacity for selected region/QPS; backup/restore, disk exhaustion and rollback drill; license/attribution audit. |
| 5: optional extensions | Extract-compatible diff replication, address geocoding with self-hosted Nominatim, routing, HA | Separate ADR and benchmark for each; no implied MVP commitment. |

Review severity: blocker = active-release corruption, user-visible outage during a normal update, unsafe arbitrary file ingestion, security boundary breach, or runtime external dependency; major = API mismatch, failed rollback, absent test for a critical transition; minor = docs, ergonomics, and maintainability. A successful CI run alone is insufficient for a release claim.

Open owner decisions: production geography/extract source, traffic/concurrency and availability target, deployment environment and resource budget, whether search must include full addresses, and permitted update lag. Defaults in code must be configurable until these are settled.
