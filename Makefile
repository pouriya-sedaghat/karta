# Karta developer and operator commands. See docs/runbook.md.
SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE        ?= docker compose
TEST_PROJECT   ?= karta-test
TEST_COMPOSE   := $(COMPOSE) -p $(TEST_PROJECT) -f compose.yaml -f compose.test.yaml
OFFLINE_COMPOSE := $(COMPOSE) -p karta-offline -f compose.yaml -f compose.offline.yaml
# The stack with a co-located controlled source bridge (compose.bridge.yaml).
# On a separate bridge host: make BRIDGE_COMPOSE="docker compose -f compose.bridge.yaml" ...
BRIDGE_COMPOSE ?= $(COMPOSE) -f compose.yaml -f compose.bridge.yaml
# A plain bridge network, like compose.yaml's `frontend`, attached to the
# offline API only as the negative control of `make test-offline`.
OFFLINE_PROBE_NET := karta-offline_frontend-probe
VERSION        ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
# Optional PEM bundle for TLS-intercepting build networks (passed as a build secret).
KARTA_BUILD_CA_FILE ?=
comma          := ,
BUILD_SECRET   := $(if $(KARTA_BUILD_CA_FILE),--secret id=build_ca$(comma)src=$(KARTA_BUILD_CA_FILE),)
BASE_URL       ?= http://localhost:8080
# Public base URL for `make test-browser-prefix`: a test proxy on this address
# serves Karta under /maps and strips the prefix before forwarding to BASE_URL.
PREFIX_BASE_URL ?= http://127.0.0.1:18091/maps
ARTIFACTS      ?= artifacts

# Prometheus (promtool) for the alert-rule checks; the same pinned image as compose.yaml.
PROMETHEUS_IMAGE := prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e

# Tool versions for static and security checks (run with `go run`, no global installs).
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1
# staticcheck v0.8.1, the latest release, reads compiler export data up to
# version 4; Go 1.27.2 writes version 5 ("export data version 5 is greater
# than maximum supported version 4"). Until a release reads it, staticcheck
# analyses with the toolchain go.mod declares; everything else uses the
# installed Go.
STATICCHECK_GOTOOLCHAIN := go1.27.1
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0
GOSEC       := github.com/securego/gosec/v2/cmd/gosec@v2.29.0

# A setting from the environment or else from .env, as Compose reads it
# (shell code for a recipe; $(1) may be a shell variable reference).
setting = $$( { printenv $(1) || sed -n "s/^$(1)=//p" .env 2>/dev/null | tail -n 1; } | sed -e 's/^"\(.*\)"$$/\1/' )

TEHRAN_PBF := data/local/tehran-chitgar.osm.pbf
TEHRAN_SHA := 7d0e69a2d5e1ad184ee48637626882bb8bcb7acd8e95123e05d0697ce216191e
# Host directory mounted read-only as the publisher's inbox (compose.yaml).
INBOX      ?= data/inbox
# Extra flags for `make import-*`, e.g. IMPORT_FLAGS=--allow-region-change.
IMPORT_FLAGS ?=

.PHONY: help
help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: secrets
secrets: ## Create database passwords and operator tokens in ./secrets (never overwrites)
	@./scripts/gen-secrets.sh

# Host directories that compose.yaml bind-mounts, created as the invoking user
# before any compose command: Docker would create a missing bind-mount source
# as root, and the inbox would then not be writable by `make publish`.
.PHONY: data-dirs
data-dirs:
	@mkdir -p data/local $(INBOX) && chmod 755 $(INBOX)

.PHONY: build
build: ## Build the api and importer images
	docker build $(BUILD_SECRET) --build-arg VERSION=$(VERSION) -f deploy/Dockerfile --target api -t karta-api:local .
	docker build $(BUILD_SECRET) --build-arg VERSION=$(VERSION) -f deploy/Dockerfile --target importer -t karta-importer:local .

.PHONY: up
up: secrets data-dirs ## Start PostgreSQL, the API and the publisher (the API reports not-ready until a release is published)
	$(COMPOSE) up -d --wait db
	$(COMPOSE) up -d api publisher

.PHONY: up-online
up-online: secrets data-dirs ## Start the stack with online updates (set KARTA_ONLINE_SOURCE_FILE in .env first; docs/runbook.md)
	@{ test -n "$$KARTA_ONLINE_SOURCE_FILE" || grep -Eq '^KARTA_ONLINE_SOURCE_FILE=.+' .env 2>/dev/null; } || { \
	  echo "Online updates are opt-in: set KARTA_ONLINE_SOURCE_FILE=/config/sources/NAME.json (a reviewed source file) in .env first." >&2; exit 2; }
	$(COMPOSE) up -d --wait db
	$(COMPOSE) --profile online up -d api publisher fetcher

.PHONY: wait-ready
wait-ready: ## Wait until /health/ready returns 200
	@for i in $$(seq 1 60); do \
	  if curl -fsS $(BASE_URL)/health/ready >/dev/null 2>&1; then echo "ready"; exit 0; fi; sleep 2; \
	done; curl -sS $(BASE_URL)/health/ready; echo; exit 1

.PHONY: import-fixture
import-fixture: data-dirs ## Publish the committed synthetic fixture with the command-line importer (prints the JSON result)
	@$(COMPOSE) run --rm -T importer --snapshot /data/testdata/fixture/karta-fixture.osm --region /config/regions/fixture.json $(IMPORT_FLAGS)

.PHONY: verify-tehran
verify-tehran: ## Check the Chitgar extract and sidecar are present and match the documented SHA-256
	@test -f $(TEHRAN_PBF) -a -f $(TEHRAN_PBF).provenance.json || { \
	  echo "Missing $(TEHRAN_PBF) or its .provenance.json: copy the two supplied files into data/local/ (docs/development-data.md)." >&2; exit 3; }
	@echo "$(TEHRAN_SHA)  $(TEHRAN_PBF)" | sha256sum --quiet -c - || { echo "SHA-256 mismatch: this is not the documented snapshot; do not substitute another." >&2; exit 3; }
	@for f in $(TEHRAN_PBF) $(TEHRAN_PBF).provenance.json; do \
	  [ $$(( 0$$(stat -c %a $$f) & 4 )) -ne 0 ] || { echo "Make $$f world-readable (chmod 0644): the importer runs as UID 10001." >&2; exit 3; }; \
	done

.PHONY: import-tehran
import-tehran: data-dirs verify-tehran ## Publish the real Chitgar extract with the command-line importer (IMPORT_FLAGS=--allow-region-change replaces another region)
	@mkdir -p $(ARTIFACTS)
	@$(COMPOSE) run --rm -T importer --snapshot /data/local/tehran-chitgar.osm.pbf --region /config/regions/tehran-chitgar.json $(IMPORT_FLAGS) | tee $(ARTIFACTS)/tehran-import.json

.PHONY: publish
publish: data-dirs ## Submit SNAPSHOT=file.osm.pbf (and its .provenance.json, if any) to the inbox with the completion protocol [NAME=...]
	@test -n "$(SNAPSHOT)" || { echo "usage: make publish SNAPSHOT=path/to/file.osm.pbf [NAME=name]" >&2; exit 2; }
	@./scripts/submit.sh $(SNAPSHOT) $(INBOX) $(NAME)

.PHONY: publish-tehran
publish-tehran: data-dirs verify-tehran ## Submit the Chitgar extract and sidecar to the inbox (publisher region tehran-chitgar, the default)
	@EXPECTED_SHA256=$(TEHRAN_SHA) ./scripts/submit.sh $(TEHRAN_PBF) $(INBOX) tehran-chitgar-$$(date -u +%Y%m%dT%H%M%SZ)

.PHONY: op
op: ## Run an operator API command, e.g. make op CMD='rollback --reason "bad data"' (see docs/runbook.md)
	@$(COMPOSE) run --rm -T operator-cli $(CMD)

.PHONY: op-status
op-status: ## Publication status: active and retained releases, submissions, authorizations, storage
	@$(COMPOSE) run --rm -T operator-cli status

.PHONY: rotate-operator-tokens
rotate-operator-tokens: data-dirs ## Replace both operator tokens (the publisher and the API reload their credential files; no restart)
	rm -f secrets/operator_token secrets/operator_monitor_token
	./scripts/gen-secrets.sh

.PHONY: operator-credential
operator-credential: ## Add, list or remove a narrow operator credential: ARGS='add NAME intake_submit' | ARGS='remove NAME' | ARGS=list
	@./scripts/operator-credential.sh $(ARGS)

# --- local intake (Stage 5, opt-in; docs/runbook.md, "Local intake") -------------

.PHONY: intake-check
intake-check: ## Check a landing area, host parents included, before enabling the watcher: LANDING=/abs/dir LANDING_UID=uid [WRITER_GID=gid]
	@landing="$(LANDING)"; [ -n "$$landing" ] || landing=$(call setting,KARTA_INTAKE_LANDING_HOST_DIR); \
	  uid="$(LANDING_UID)"; [ -n "$$uid" ] || uid=$(call setting,KARTA_INTAKE_LANDING_UID); \
	  gid="$(WRITER_GID)"; [ -n "$$gid" ] || gid=$(call setting,KARTA_INTAKE_WRITER_GID); \
	  [ -n "$$landing" ] && [ -n "$$uid" ] || { echo "usage: make intake-check LANDING=/path/to/landing LANDING_UID=uid [WRITER_GID=gid]" \
	    "(or KARTA_INTAKE_LANDING_HOST_DIR and KARTA_INTAKE_LANDING_UID in .env)" >&2; exit 2; }; \
	  case "$$landing" in /*) ;; *) landing="$$(pwd)/$${landing#./}" ;; esac; \
	  docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
	    -v /:/host:ro -e KARTA_INTAKE_LANDING_UID="$$uid" -e KARTA_INTAKE_WRITER_GID="$$gid" \
	    karta-api:$${KARTA_IMAGE_TAG:-local} intake check --landing "$$landing" --host-root /host

.PHONY: up-intake
up-intake: secrets data-dirs ## Start the stack with the intake watcher (KARTA_INTAKE_DIR=/data/intake, KARTA_INTAKE_LANDING_HOST_DIR and _UID in .env)
	@[ "$(call setting,KARTA_INTAKE_DIR)" = /data/intake ] || { \
	  echo "The local intake is opt-in: set KARTA_INTAKE_DIR=/data/intake in .env first (docs/runbook.md, \"Local intake\")." >&2; exit 2; }
	@for v in KARTA_INTAKE_LANDING_HOST_DIR KARTA_INTAKE_LANDING_UID; do \
	  [ -n "$(call setting,$$v)" ] || { echo "Set $$v in .env (the protected landing area and its owner's UID)." >&2; exit 2; }; done
	@[ -d "$(call setting,KARTA_INTAKE_LANDING_HOST_DIR)" ] || { \
	  echo "The landing directory does not exist: create it owned by the landing account (docs/runbook.md)." >&2; exit 2; }
	@$(MAKE) -s intake-check
	@./scripts/operator-credential.sh add local-intake intake_watch secrets/intake_watch_token
	$(COMPOSE) up -d --wait db
	$(COMPOSE) up -d api publisher
	$(COMPOSE) --profile intake up -d intake-watch

.PHONY: intake-off
intake-off: ## Turn the watcher off: stop it and remove its credential (the command and direct publication stay available)
	-$(COMPOSE) --profile intake stop intake-watch
	-$(COMPOSE) --profile intake rm -f intake-watch
	-./scripts/operator-credential.sh remove local-intake secrets/intake_watch_token
	@echo "The watcher is stopped and its credential refused. Its open authorizations expire by themselves;"
	@echo "to close one now: make op CMD='revoke --sha256 HEX --reason \"intake off\"' (see make op-status, intake)."

.PHONY: intake-submit
intake-submit: ## Submit FILE=x.osm.pbf with your TOKEN=secrets/NAME.token and SHA256=hex [SIZE=n] | EXPECT=marker | ATTEST="reason" [REASON=...]
	@test -n "$(FILE)" -a -n "$(TOKEN)" || { echo "usage: make intake-submit FILE=path.osm.pbf TOKEN=secrets/NAME.token" \
	  "(SHA256=hex [SIZE=bytes] | EXPECT=path.osm.pbf.complete | ATTEST='why it is complete') [REASON=text] [NO_WAIT=1]" >&2; exit 2; }
	@test -f "$(FILE)" -a -f "$(TOKEN)" || { echo "$(FILE) or $(TOKEN) does not exist" >&2; exit 2; }
	@$(COMPOSE) run --rm -T -v "$(abspath $(dir $(FILE))):/submit:ro" -v "$(abspath $(TOKEN)):/run/secrets/intake_token:ro" \
	  $(if $(EXPECT),-v "$(abspath $(EXPECT)):/expect/$(notdir $(EXPECT)):ro") \
	  intake-cli submit "/submit/$(notdir $(FILE))" $(if $(SHA256),--expect-sha256 $(SHA256)) $(if $(SIZE),--expect-size $(SIZE)) \
	  $(if $(EXPECT),--expect-file "/expect/$(notdir $(EXPECT))") \
	  $(if $(ATTEST),--attest-complete --reason "$(ATTEST)",$(if $(REASON),--reason "$(REASON)")) $(if $(NO_WAIT),--no-wait)

.PHONY: deliver
deliver: ## Deliver SNAPSHOT=x.osm.pbf to a landing area DEST=dir|sftp://user@host/path with its completion marker [NAME=...]
	@test -n "$(SNAPSHOT)" -a -n "$(DEST)" || { echo "usage: make deliver SNAPSHOT=path.osm.pbf DEST=landing-dir|sftp://user@host/path [NAME=name]" >&2; exit 2; }
	@./scripts/deliver.sh $(SNAPSHOT) $(DEST) $(NAME)

# --- controlled source bridge (Stage 5, opt-in; docs/runbook.md) ------------------

.PHONY: bridge-tls
bridge-tls: ## Create the co-located bridge's private TLS CA and bridge-serve certificate (secrets/bridge; CA copy in config/sources)
	@./scripts/gen-bridge-tls.sh

.PHONY: up-bridge
up-bridge: secrets data-dirs ## Start a co-located bridge and the fetcher reaching only it (KARTA_BRIDGE_SOURCE_FILE, KARTA_ONLINE_SOURCE_FILE in .env)
	@for v in KARTA_BRIDGE_SOURCE_FILE KARTA_ONLINE_SOURCE_FILE; do \
	  [ -n "$(call setting,$$v)" ] || { echo "The bridge is opt-in: set $$v in .env first (docs/runbook.md, \"Controlled source bridge\")." >&2; exit 2; }; done
	@key=$(call setting,KARTA_BRIDGE_SIGNING_KEY_FILE); key=$${key:-secrets/bridge/signing-key.pem}; [ -s "$$key" ] || { \
	  echo "No bridge signing key at $$key: its custodian provides it (docs/operations.md, owner inputs)." >&2; exit 2; }
	@[ -s secrets/bridge/tls.pem ] || [ -n "$(call setting,KARTA_BRIDGE_TLS_CERT_FILE)" ] || { echo "No bridge TLS certificate: run make bridge-tls first." >&2; exit 2; }
	$(BRIDGE_COMPOSE) up -d --wait db
	$(BRIDGE_COMPOSE) --profile bridge --profile online up -d api publisher bridge-acquire bridge-sign bridge-serve fetcher

.PHONY: bridge-status
bridge-status: ## The bridge's acquire and sign reports (JSON)
	@$(BRIDGE_COMPOSE) --profile bridge run --rm -T --no-deps bridge-serve bridge status

.PHONY: bridge-raise-high-water
bridge-raise-high-water: ## After a signer state was lost or restored: SERIAL=N (at least Karta's verified serial) REASON="..."
	@test -n "$(SERIAL)" -a -n "$(REASON)" || { echo "usage: make bridge-raise-high-water SERIAL=N REASON='...' (docs/runbook.md)" >&2; exit 2; }
	$(BRIDGE_COMPOSE) --profile bridge stop bridge-sign
	$(BRIDGE_COMPOSE) --profile bridge run --rm -T --no-deps bridge-sign bridge sign --raise-high-water $(SERIAL) --reason "$(REASON)"
	$(BRIDGE_COMPOSE) --profile bridge up -d bridge-sign

.PHONY: bridge-off
bridge-off: ## Stop the co-located bridge and the fetcher reading it (local intake and direct publication are unaffected)
	-$(BRIDGE_COMPOSE) --profile bridge --profile online stop bridge-acquire bridge-sign bridge-serve fetcher
	@echo "The bridge and the fetcher are stopped; the active release stays and ages (KartaDataStale)."
	@echo "Online updates from another source: set KARTA_ONLINE_SOURCE_FILE and run make up-online (compose.yaml only)."

# Backup destination for `make backup` (git-ignored by default).
BACKUP_DEST ?= backups

.PHONY: backup
backup: ## Back up the running deployment into BACKUP_DEST (database, outbox, config, never secrets; scripts/backup.sh)
	@COMPOSE="$(COMPOSE)" ./scripts/backup.sh $(BACKUP_DEST)

.PHONY: restore
restore: ## Restore BACKUP=backups/karta-... into this project (REPLACE=1 deletes its existing data first; scripts/restore.sh)
	@test -n "$(BACKUP)" || { echo "set BACKUP=backups/karta-<time>" >&2; exit 2; }
	@COMPOSE="$(COMPOSE)" ./scripts/restore.sh $(BACKUP) $(if $(REPLACE),--replace)

.PHONY: restore-check
restore-check: ## Verify the registry and every retained release database (changes nothing)
	@$(COMPOSE) run --rm -T --no-deps --entrypoint /usr/local/bin/karta importer restore-check

.PHONY: rotate-db-password
rotate-db-password: ## Rotate a database role password without a restart: ROLE=api|importer|monitor|superuser
	@test -n "$(ROLE)" || { echo "set ROLE=api|importer|monitor|superuser" >&2; exit 2; }
	@COMPOSE="$(COMPOSE)" ./scripts/rotate-db-password.sh $(ROLE)

.PHONY: monitoring-role
monitoring-role: ## Create or update the PostgreSQL role of the metrics exporter (karta_monitor, pg_monitor; idempotent)
	@COMPOSE="$(COMPOSE)" ./scripts/create-monitor-role.sh

.PHONY: up-monitoring
up-monitoring: secrets data-dirs ## Start the optional monitoring profile: PostgreSQL exporter and Prometheus with the Karta alert rules
	$(COMPOSE) up -d --wait db
	@$(MAKE) -s monitoring-role
	$(COMPOSE) --profile monitoring up -d postgres-exporter prometheus

.PHONY: smoke
smoke: ## Query the manifest, a Persian search and a tile
	@curl -fsS $(BASE_URL)/v1/manifest | head -c 600; echo
	@curl -fsS -G $(BASE_URL)/v1/search --data-urlencode 'q=دریاچه' --data-urlencode limit=3; echo

.PHONY: logs
logs: ## Follow service logs
	$(COMPOSE) logs -f --tail=100

.PHONY: down
down: ## Stop the stack, keeping data
	$(COMPOSE) down

.PHONY: reset
reset: ## Stop the stack and delete all imported data (the database volume)
	$(COMPOSE) down -v --remove-orphans

.PHONY: clean
clean: reset ## reset, then remove images, demo build and test artefacts
	-docker image rm karta-api:local karta-importer:local
	rm -rf web/dist web/node_modules $(ARTIFACTS)

# --- development and tests ------------------------------------------------------

.PHONY: web
web: ## Build the demo assets into web/dist for running `karta serve` outside Docker
	cd web && npm ci --ignore-scripts --no-audit --no-fund && node build.mjs

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w cmd internal openapi tests

.PHONY: fixtures
fixtures: ## Regenerate the committed PBF fixture snapshots from their XML sources
	go run ./cmd/karta-fixture

.PHONY: lint
lint: ## gofmt, go vet, staticcheck, govulncheck, gosec, committed fixture check
	@out=$$(gofmt -l cmd internal openapi tests); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go run ./cmd/karta-fixture -check > /dev/null
	go vet ./...
	go vet -tags integration ./tests/integration/
	go vet -tags browser ./tests/browser/
	GOTOOLCHAIN=$(STATICCHECK_GOTOOLCHAIN) go run $(STATICCHECK) ./...
	GOTOOLCHAIN=$(STATICCHECK_GOTOOLCHAIN) go run $(STATICCHECK) -tags integration,browser ./tests/...
	go run $(GOVULNCHECK) ./...
	go run $(GOSEC) -quiet -exclude-generated ./...

.PHONY: test-alerts
test-alerts: ## Check the Prometheus configuration and alert rules, and run the rule unit tests (promtool; needs Docker)
	@tok=$$(mktemp) && chmod 644 $$tok && echo 0000000000000000000000000000000000000000000000000000000000000000 > $$tok && \
	  docker run --rm -v "$(CURDIR)/deploy/monitoring:/etc/karta:ro" -v "$$tok:/run/secrets/operator_monitor_token:ro" \
	    --entrypoint /bin/promtool $(PROMETHEUS_IMAGE) check config /etc/karta/prometheus.yml; status=$$?; rm -f $$tok; exit $$status
	@tok=$$(mktemp) && chmod 644 $$tok && echo 0000000000000000000000000000000000000000000000000000000000000000 > $$tok && \
	  docker run --rm -v "$(CURDIR)/deploy/monitoring:/etc/karta:ro" -v "$(CURDIR)/deploy/monitoring/bridge:/etc/karta-extra:ro" \
	    -v "$$tok:/run/secrets/operator_monitor_token:ro" \
	    --entrypoint /bin/promtool $(PROMETHEUS_IMAGE) check config /etc/karta/prometheus.yml; status=$$?; rm -f $$tok; exit $$status
	docker run --rm -v "$(CURDIR)/deploy/monitoring:/etc/karta:ro" -w /etc/karta/tests --entrypoint /bin/promtool $(PROMETHEUS_IMAGE) test rules alerts_test.yml

.PHONY: test
test: ## Unit tests (no Docker needed)
	go test -race -count=1 ./...

.PHONY: testsource
testsource: ## Build the controlled HTTPS source image of the Stage 3 tests (test-only)
	docker build $(BUILD_SECRET) -q -f tests/integration/sourceserver/Dockerfile -t karta-testsource:local . > /dev/null

.PHONY: test-integration
test-integration: secrets data-dirs testsource ## Registry concurrency tests (PostgreSQL) and full-stack integration tests on an isolated compose project (committed fixtures, local controlled HTTPS source only)
	@mkdir -p $(ARTIFACTS)
	@tmp=$$(mktemp -d) && chmod 755 $$tmp && mkdir -m 755 $$tmp/inbox $$tmp/online $$tmp/sources $$tmp/docroot $$tmp/tls \
	    $$tmp/intake $$tmp/landing $$tmp/bridge $$tmp/regions && \
	  export KARTA_INBOX_HOST_DIR=$$tmp/inbox KARTA_ONLINE_HOST_DIR=$$tmp/online KARTA_TEST_SOURCE_CONFIG_DIR=$$tmp/sources \
	    KARTA_TEST_SOURCE_DOCROOT=$$tmp/docroot KARTA_TEST_SOURCE_TLS_DIR=$$tmp/tls KARTA_TEST_UID=$$(id -u) KARTA_TEST_GID=$$(id -g) \
	    KARTA_INTAKE_HOST_DIR=$$tmp/intake KARTA_INTAKE_LANDING_HOST_DIR=$$tmp/landing KARTA_TEST_BRIDGE_DIR=$$tmp/bridge \
	    KARTA_TEST_REGION_DIR=$$tmp/regions && \
	  { $(TEST_COMPOSE) --profile online --profile intake down -v --remove-orphans >/dev/null 2>&1 || true; } && \
	  $(TEST_COMPOSE) up -d --wait db && $(TEST_COMPOSE) up -d api publisher && \
	  KARTA_TEST_PG_DSN="host=127.0.0.1 port=$${KARTA_TEST_DB_PORT:-55433} user=postgres dbname=postgres sslmode=disable password=$$(cat secrets/db_superuser_password)" \
	    go test -race -count=1 -timeout 5m -v -run '^TestDB' ./internal/registry/ ; dbstatus=$$?; \
	  KARTA_TEST_COMPOSE="$(TEST_COMPOSE)" KARTA_TEST_ARTIFACTS=$(CURDIR)/$(ARTIFACTS) \
	    go test -tags integration -count=1 -timeout 60m -v $(if $(RUN),-run '$(RUN)') ./tests/integration/ ; \
	  status=$$?; [ $$dbstatus -eq 0 ] || status=$$dbstatus; for s in api publisher fetcher source intake-watch; do \
	    $(TEST_COMPOSE) --profile online --profile intake logs --no-color $$s > $(CURDIR)/$(ARTIFACTS)/integration-$$s.log 2>&1 || true; done; \
	  $(TEST_COMPOSE) --profile online --profile intake down -v --remove-orphans; \
	  docker network rm $(TEST_PROJECT)_bridge $(TEST_PROJECT)_bridge-egress >/dev/null 2>&1 || true; rm -rf -- "$$tmp"; exit $$status

.PHONY: test-browser
test-browser: ## Render the demo in headless Chromium against the running stack (BASE_URL)
	@mkdir -p $(ARTIFACTS)
	KARTA_TEST_BASE_URL=$(BASE_URL) KARTA_TEST_ARTIFACTS=$(CURDIR)/$(ARTIFACTS)/browser go test -tags browser -count=1 -v ./tests/browser/

.PHONY: test-browser-tehran
test-browser-tehran: ## Render the real Chitgar release (after `make import-tehran`) and check lake, park/road and mall views
	@mkdir -p $(ARTIFACTS)
	KARTA_TEST_BASE_URL=$(BASE_URL) KARTA_TEST_ARTIFACTS=$(CURDIR)/$(ARTIFACTS)/tehran \
	KARTA_TEST_VIEWS="$$(cat tests/browser/tehran-views.json)" \
	KARTA_TEST_SEARCH='{"q":"دریاچه چیتگر","expect":"دریاچه چیتگر"}' \
	go test -tags browser -count=1 -v ./tests/browser/

.PHONY: test-browser-prefix
test-browser-prefix: ## Browser tests through a proxy serving Karta under /maps (running stack; restarts the API with PREFIX_BASE_URL, then restores it)
	@mkdir -p $(ARTIFACTS)
	KARTA_PUBLIC_BASE_URL=$(PREFIX_BASE_URL) $(COMPOSE) up -d --no-deps api
	@$(MAKE) -s wait-ready
	KARTA_TEST_BASE_URL=$(PREFIX_BASE_URL) KARTA_TEST_PREFIX_UPSTREAM=$(BASE_URL) \
	KARTA_TEST_ARTIFACTS=$(CURDIR)/$(ARTIFACTS)/prefix go test -tags browser -count=1 -v ./tests/browser/ ; \
	  status=$$?; $(COMPOSE) up -d --no-deps api; $(MAKE) -s wait-ready || status=1; exit $$status

.PHONY: test-offline
test-offline: secrets data-dirs ## Disconnected run: API and DB on an internal-only network (checked), fixture imported, browser via container IP
	$(OFFLINE_COMPOSE) down -v --remove-orphans >/dev/null 2>&1 || true
	-@docker network rm $(OFFLINE_PROBE_NET) >/dev/null 2>&1
	$(OFFLINE_COMPOSE) up -d --wait db
	$(OFFLINE_COMPOSE) run --rm -T importer --snapshot /data/testdata/fixture/karta-fixture.osm --region /config/regions/fixture.json > /dev/null
	$(OFFLINE_COMPOSE) up -d api
	@mkdir -p $(ARTIFACTS)
	@api=$$($(OFFLINE_COMPOSE) ps -q api); probe=karta-importer:$${KARTA_IMAGE_TAG:-local}; \
	  cleanup() { docker network rm $(OFFLINE_PROBE_NET) >/dev/null 2>&1 || true; $(OFFLINE_COMPOSE) down -v --remove-orphans; }; \
	  echo "negative control: the isolation check must fail while the api is also on a frontend (non-internal) network"; \
	  docker network create $(OFFLINE_PROBE_NET) >/dev/null && docker network connect $(OFFLINE_PROBE_NET) $$api || { cleanup; exit 1; }; \
	  if scripts/check-isolated.sh $$api karta-offline_backend $$probe; then control=passed; else control=failed; fi; \
	  docker network disconnect $(OFFLINE_PROBE_NET) $$api && docker network rm $(OFFLINE_PROBE_NET) >/dev/null || { cleanup; exit 1; }; \
	  if [ $$control = passed ]; then echo "the isolation check passed although the api was on a frontend network" >&2; cleanup; exit 1; fi; \
	  scripts/check-isolated.sh $$api karta-offline_backend $$probe || { cleanup; exit 1; }; \
	  ip=$$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' $$api); \
	  echo "api container IP $$ip"; \
	  for i in $$(seq 1 30); do curl -fsS http://$$ip:8080/health/ready >/dev/null 2>&1 && break; sleep 1; done; \
	  $(OFFLINE_COMPOSE) exec -T api /usr/local/bin/karta healthcheck || { cleanup; exit 1; }; \
	  KARTA_TEST_BASE_URL=http://karta.internal:8080 KARTA_TEST_RESOLVE=karta.internal=$$ip \
	  KARTA_TEST_ARTIFACTS=$(CURDIR)/$(ARTIFACTS)/offline go test -tags browser -count=1 -v ./tests/browser/ ; \
	  status=$$?; cleanup; exit $$status
