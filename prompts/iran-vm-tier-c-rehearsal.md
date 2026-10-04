# Draft brief: full-Iran rehearsal on the owner's Windows-hosted VM (tier C)

Status: **draft, revised after Claude Code's brief review (2026-10-04)**, for
Codex review and owner decisions. This is a measurement and recovery
rehearsal of the merged Stage 5 code, not a new implementation stage or a
production acceptance test. The production host does not yet exist. The
baseline is `main` at merge commit `ab7dca3` (PR #12); both jobs of its CI
run 37183595394 passed on fixtures (rechecked 2026-10-04). Do not infer
full-Iran capacity from those fixtures or from the PBF size (about 230 MB:
229,580,914 bytes are claimed for the 2026-09-27 file).

**Tier C, never tier D.** Every result is evidence about this VM only
(`docs/operations.md`, evidence tiers). It says nothing about production-host
capacity, limits, objectives, key custody or recovery (tier D). Label reports
and capacity runs `tier-c-vm-…`; the runbook's `tier-d-iran-…` examples are
for the future production host.

**What the review could not check.** The review ran in Claude Code's cloud
container, not on the VM. It observed no VM filesystem, Docker setup, CPU,
memory, storage, existing Karta data or PBF, and records none. The
corrections below come from the repository at `ab7dca3`. Every VM fact under
"Owner inputs" is still open and is collected read-only at the start of the
VM session (procedure step 1).

The owner will open a separate Claude Code session on the VM. Reading this
brief does not authorize connecting to the VM, downloading a PBF, generating
a signing key, changing a running database, or deploying to a production
host. Agree on the VM work and its data source in that session.

## Goals

Measure one **fixed, exact Geofabrik Iran PBF** through Karta's common
verification/build path. Exercise protected local intake first; if the host
preflight and owner-approved writer boundary permit it, exercise the watcher
separately. Confirm that serving remains available across a publication and
rollback, and measure disk, memory, CPU, WAL, time and request latency. Record
both successes and refusals. Keep Stage 3/5 online source and bridge disabled
unless a separate, clearly scoped test with a throwaway key and controlled
source is approved; no production key or automatic Geofabrik polling here.

## Candidate snapshot

The only Iran file identity recorded in the repository is the source of the
Chitgar extract, from that extract's provenance sidecar (`docs/licenses.md`,
`docs/development-data.md`): `iran-260927.osm.pbf`, 229,580,914 bytes,
SHA-256 `fbb1b010efaa16b01ba24baaa40109f90cda4d32cf38bf4be1b5b67bac2e4529`,
Geofabrik replication sequence 4920, timestamp 2026-09-27T20:23:36Z, header
box `44.023033,24.039475,63.35413,39.790447`. These are **claims, never
verified**: the file was not available. No Iran PBF is in Git or was seen by
this review. If the owner still holds that exact file, it is the natural
fixed snapshot: its digest was recorded from the source copy before any VM
transfer, and a match also verifies the Chitgar sidecar's claim. Otherwise
the owner names another dated file and records its SHA-256 and size before
the transfer. A mismatch with the record stops the run.

## Owner inputs before anything on the VM changes

1. **VM and Windows host:** hypervisor (Hyper-V, VMware, VirtualBox, WSL2),
   guest OS, CPU, RAM, swap; whether Docker Engine runs inside the guest
   (with Docker Desktop, Docker's data root is in Docker Desktop's own VM and
   bind mounts cross into it: the watcher is then out of scope and capacity
   figures describe that VM); Docker and Compose versions (Compose 2.24 or
   later);
   filesystem types of the checkout, the Docker data root and any landing
   area; free and total space in the guest **and** free space on the Windows
   volume behind a dynamically expanding virtual disk (a check inside the VM
   cannot see the host); clock synchronization and whether the VM may be
   suspended or checkpointed.
2. **Existing Karta:** whether a checkout, Compose project (default name
   `karta`), images, volumes or database exist; their commit and registry
   schema version; what must be preserved (a Chitgar release, audit history)
   and whether that stack may be stopped. See "Existing database".
3. **Snapshot:** the PBF's path on the VM or permission for one specified
   download, plus origin URL, observation time, expected SHA-256 and size
   recorded independently of the copy ("Candidate snapshot"). Record
   Geofabrik's `.md5`, `Last-Modified`, header timestamp and box as
   metadata, not as a signature. Never use the moving `iran-latest` URL as
   the identity of a fixed snapshot.
4. **Disk:** how much the rehearsal may use, the storage budget value
   ("Capacity"), the abort thresholds, where backups go (outside the
   filesystem being measured, ideally off the VM), and whether an isolated
   restore project fits (it needs room for the whole cluster again).
5. **Access:** the person holding the named `intake_submit` credential and,
   only if the watcher is in scope, the landing account and its writers.
   Token files stay under the private `secrets/` directory:
   `scripts/operator-credential.sh` makes them mode 0644 (readable by
   container UID 65532), so a custom token path under a traversable
   directory is unsafe.
6. **Scope:** whether a second Iran release is in scope (step 5a; the only
   same-region rollback target, at the cost of its disk), and the request
   mix and limits for the load run.

Do not put the PBF, tokens, keys, `.env`, backups, VM paths, UIDs, host names
or unredacted logs in Git. `.env` is **not** git-ignored: never `git add -A`
on the VM. The only file meant for a commit is the reviewed
`config/regions/iran.json`, through a branch and PR. Record nonsecret evidence
in a separate report.

## Existing database: pre-upgrade backup and schema v5

Merged registry schemas: v1 (Stage 1), v2 (Stage 2), v3 (Stages 3 and 4,
through `5b6f8ec`), **v5** (Stage 5, `ab7dca3`); v4 existed only on unmerged
Stage 5 branch commits. The Stage 5 publisher, `karta import`,
`karta registry-summary` (which `make backup` runs **before** its base
backup) and `karta restore-check` (also inside `make restore`) migrate the
registry to v5 when they start, in one transaction, without asking. There is
no downgrade path. Stage 2 to 4 builds refuse a newer registry in their
publisher, importer and, where present, `registry-summary` and
`restore-check` ("newer than this build supports"). So after an upgrade the
old build can neither publish nor back up; its API may keep serving. Stage
1's behaviour was not checked. The only way back is a backup taken before
the upgrade, restored with the old checkout and images.

If a Karta database exists on the VM:

* **Default: do not upgrade it.** Leave its checkout, project, volumes and
  images untouched; no Stage 5 container may connect to it. The rehearsal
  runs in a separate checkout, project and image tag (step 2).
* **Pre-upgrade backup first**, as a safety net, with the existing
  deployment's **own** checkout, scripts and images, before any Stage 5
  image is built. Where that checkout has the backup tooling (`028cc55`,
  PR #9, or later): `make backup BACKUP_DEST=<agreed location>`. It verifies
  the base backup with `pg_verifybackup` and writes `SHA256SUMS` and a
  `MANIFEST` naming the commit and image ids to restore with. Check
  `SHA256SUMS` again after any copy off the VM. An older deployment has no
  backup script: the owner chooses a cold copy or VM checkpoint with that
  stack stopped; do not improvise one. Also record, read-only, its schema
  version (`SELECT max(version) FROM registry.schema_migrations`; the table
  is absent on a Stage 1 registry) and its active release.
* **An in-place upgrade is a separate owner decision**, not part of this
  rehearsal. If chosen, the rollback is: check out the backup's
  `karta_commit`, rebuild those images, `make restore BACKUP=… REPLACE=1`
  (it deletes the upgraded database; everything after the backup is lost).
  Rehearse that restore first in an isolated project **with the old checkout
  and images**: restoring the old backup with Stage 5 tooling migrates it to
  v5 and proves only the forward path.
* Never run `restore … REPLACE=1`, `make reset`, `docker compose down -v`,
  a volume prune, or replace an existing release as a shortcut.

## Capacity (defaults at `ab7dca3`)

* **Copies of the PBF:** the source on the VM; for an intake delivery, a full
  copy in the `intake` volume and the publisher's staging copy (volume
  `staging`). `karta import` stages into the importer's `/tmp`, a 1 GiB tmpfs
  that counts against the importer's 4 GiB memory limit.
* **Storage budget**, checked after staging and the full PBF scan, before
  any build: all release and candidate databases plus
  max(40 × PBF + 32 MiB, 1.25 × the region's largest ready, active or
  retired release) must fit `KARTA_RELEASE_STORAGE_BUDGET_MB` (10240). For
  229,580,914 bytes the estimate is about 8.6 GiB. So the first Iran build
  fits only while the cluster's other release databases stay under about
  1.4 GiB. **Every later Iran submission, a duplicate of a built release
  included, is refused `insufficient_storage` once the first Iran release
  exceeds about 1.4 GiB**, unless the budget is raised: the check runs before
  the build finds an existing release. The budget is a number, not free
  space.
* **No real disk check by default:** `compose.yaml` does not pass
  `KARTA_DB_VOLUME_PATH`. In tier A/B a full disk failed the build safely,
  but sustained writes on a full disk can stop PostgreSQL, and other Docker
  data on the same filesystem is affected too.
* **Memory:** osm2pgsql runs non-slim (cache 800 MB, 2 processes) inside the
  publisher or importer (4 GiB limit each); db 2 GiB with 256 MB shared
  buffers; api 512 MiB. During a `karta import` with the stack up, the
  memory limits add up to 10.5 GiB. An OOM kill is a result to record, not
  to work around silently.
* **Settings that `.env` does not reach:** `KARTA_OSM2PGSQL_SLIM`,
  `KARTA_DB_VOLUME_PATH`, `KARTA_CANDIDATE_SIZE_FACTOR` and
  `KARTA_MAX_INPUT_MB` are read by the code but not passed to the publisher
  or importer by `compose.yaml`. Change them only with
  `docker compose run -e …` for the one-off importer, or an uncommitted
  override file, and record that. `KARTA_RELEASE_STORAGE_BUDGET_MB`,
  `KARTA_PUBLISH_TIMEOUT` (6 h) and the memory limits are passed.

## Procedure for Claude Code

1. **Read-only inventory.** Sync to the reviewed commit and read
   `docs/runbook.md` (local intake, command-line import, backup/restore,
   storage), `docs/operations.md` (evidence tiers, upgrade, capacity, Iran
   region, owner inputs), ADR 0006, `config/regions`, the scripts and the
   Compose files. On the VM, read only: `uname -a`, `/etc/os-release`,
   `nproc`, `free -h`, `df -hT` (checkout and the Docker data root from
   `docker info`), `docker version`, `docker compose version`, `docker info`,
   `docker compose ls -a`, `docker ps -a`, `docker volume ls`,
   `docker image ls 'karta-*'`, `timedatectl`, and any existing checkout's
   `git rev-parse HEAD` and `git status`. Report every difference from this
   brief and stop for the owner's decisions before changing anything.
2. **Isolation.** Use a separate checkout at the reviewed commit, with its own
   `.env`, `secrets/`, `data/` and `config/regions`. `compose.yaml` pins
   `name: karta`, so a second checkout without a project name attaches to
   the existing project's volumes. Name the rehearsal project in every
   command (`-p`, `COMPOSE="docker compose -p …"` for make targets and
   scripts, or `COMPOSE_PROJECT_NAME` in that `.env`), and confirm it with
   `docker compose config` and `docker compose ls` before the first `up`. If
   another stack runs, use free ports (`KARTA_HTTP_PORT`,
   `KARTA_OPERATOR_PORT`, `KARTA_API_METRICS_PORT`, a matching
   `KARTA_PUBLIC_BASE_URL`, `BASE_URL`/`BASE` for `make wait-ready`,
   `make smoke` and `scripts/capacity-run.sh`). `make build` always tags
   `karta-api:local` and `karta-importer:local`, which an existing
   deployment also uses: if one exists, record its image ids and take its
   backup first. Then build the rehearsal images under another tag (the
   Makefile's two `docker build` commands with `-t karta-…:<tag>`) and set
   `KARTA_IMAGE_TAG=<tag>` in the rehearsal `.env` **and** the shell
   (`make intake-check`, `scripts/backup.sh` and `scripts/capacity-run.sh`
   read it from the environment only).
3. **Snapshot and region draft.** Hash the PBF on the VM and compare its
   SHA-256 and size with the independent record; stop on a mismatch. Draft
   the region: `docker compose -p <project> run --rm -T --no-deps
   -v <dir>:/in:ro --entrypoint /usr/local/bin/karta importer region-draft
   --snapshot /in/<file> --id iran --name "Iran"`, with stdout to
   `config/regions/iran.json` and stderr kept separate. It needs no
   database, scans the whole file, prints the box source and counts, pins
   `expected_sha256`, sets `require_provenance` only if a sidecar is present
   (keep it off for a raw Geofabrik file unless a reviewed sidecar is
   supplied), and centres the default view in the box (with the claimed box,
   about 53.7°E 31.9°N at zoom 4). Review the pin and box. Do not copy
   Chitgar checks.
4. **Budget, then the measured no-activate import.** Agree the budget, the
   abort thresholds (free space on the Docker data root and on the Windows
   host volume, OOM kill, swap, the deadline) and the sampling first. Start
   the rehearsal project with `KARTA_PUBLISH_REGION=iran`. Where another
   region is active, `karta import` needs `--allow-region-change` even with
   `--no-activate`: the forward rule runs before the build. Run
   `docker compose -p <project> run --rm -T importer --snapshot
   /data/local/<file> --region /config/regions/iran.json --no-activate
   --reason "…"` and keep its stdout: that JSON holds the import report. The
   importer mounts `./data/local`; `--report` would write into its tmpfs.
   Meanwhile sample every few seconds `docker stats` of the project's
   containers, `df` of the Docker data root, and, as `postgres` in `db`,
   `SELECT sum(size) FROM pg_ls_waldir()` and the database sizes. Record
   phase timings, peak CPU and memory per container, disk and WAL growth,
   the release database size, feature counts and the validation output.
   Report a refusal (`insufficient_storage`, timeout, OOM) as a result; do
   not weaken checks to force the build through.
5. **Thresholds, and a validated activation.** Derive `min_counts`,
   `max_drop_fraction`, Persian searches and tile checks from the inspected
   release, and review the region file in a branch/PR. **The reviewed
   checks never run on the no-activate release**: thresholds are not part
   of the release id, and a resubmission of the same digest reuses the
   `ready` release without validating it again. Owner and Codex choose:
   a. the reviewed file also changes the release identity (for example the
      default view on Tehran instead of the desert centre). The intake
      delivery of the same digest then builds and validates a **new**
      release with the reviewed checks, a real intake build, and the
      no-activate release stays as a ready rollback target. This needs disk
      for two Iran releases plus WAL, and a budget of at least the first
      release + max(8.6 GiB, 1.25 × the first release);
   b. the identity stays the same. The intake delivery then activates the
      no-activate release unvalidated (label it "ready-release activation;
      reviewed checks not exercised"). It still needs the budget of 5a,
      because the capacity check comes first. Exercise the reviewed checks
      in a fresh isolated project instead.
   Then make one delivery as the named `intake_submit` holder, with the
   independent digest and size (`make intake-submit … SHA256=… SIZE=…`),
   with `KARTA_INTAKE_DIR=/data/intake` and the publisher on region `iran`.
   The file must be world-readable. Record the audit trail and the outcome.
   Avoid the last-resort `ATTEST` path.
6. **Watcher, only if in scope and Docker Engine runs in the guest.** Put
   the landing area on the VM's local POSIX filesystem, owned by the landing
   account. Files must be owned by that account with mode 0644: a copy made
   as another user is refused `wrong_owner`. Restrict writers, run
   `make intake-check` and require `ok: true`. The supported transfer is
   SFTP from Windows, with PowerShell hashing the **original** file (see the
   runbook); upload under a temporary name and write the `.complete` marker
   last. A hypervisor or SMB share is only an untrusted transfer space,
   never the landing area. A marker computed from a copy on the share proves
   nothing: `scripts/deliver.sh` hashes the file it is given, so set
   `EXPECTED_SHA256` to the independent record. Refusal tests (incomplete
   marker, size or digest mismatch, unsafe mode) stop before any
   authorization and build nothing. A new delivery of bytes already built
   still copies, stages and scans the whole file and must pass the capacity
   check before it is reported as a duplicate.
7. **Serving, publication and rollback.** With an active Iran release, run a
   bounded load with
   `scripts/capacity-run.sh tier-c-vm-iran-… -duration … -concurrency …`
   (`BASE` and `COMPOSE` set; `-follow` across a switch). `karta-load` draws
   tiles uniformly from the manifest bounds, mostly sparse land at Iran
   scale, and its default queries are Tehran-centric. That makes the load
   synthetic, not a traffic model. A publication or rollback under load
   needs a second Iran release (5a); without one there is no same-region
   rollback target: report that rather than rolling back to another region.
   Record request counts, errors, p50/p95/p99 latency and resource use.
   Check release pinning, the active pointer, the audit trail, the pause of
   watcher activation after a rollback, and serving after network loss. Do
   not claim a performance objective until the owner chooses workload and
   limits.
8. **Backup and isolated restore.** Run `make backup` of the rehearsal
   project to the agreed location. Restore it into a second isolated project
   with other ports (runbook, "Backup and restore") and confirm
   `restore-check` passes there. Check the clock (`timedatectl`) before and
   after any VM suspend or resume, and do not suspend during a build.

## Evidence and exit criteria

Deliver a concise report with:

* exact commit, image tags and digests, and project name;
* the VM resource inventory, including the Windows host volume;
* the fixed PBF's provenance, SHA-256, size, timestamp and box, and whether
  they match the recorded claim;
* the budget, abort thresholds and any override, with their values;
* before and after storage, and backup verification;
* the region configuration diff, and which option of step 5 was used;
* the full import report and the sampled metrics;
* the command and watcher outcomes;
* serving and rollback observations;
* failures and unresolved risks.

Redact credentials, paths and host identifiers. Label the results **tier C
on this VM**. Tier D production-host capacity, key custody, online timing,
SLOs and target-host recovery remain open. Return the VM to an agreed state
and report which services, projects, images and volumes remain. Do not
silently enable the bridge or the watcher.

The owner and Codex review this brief before execution. Claude Code may
propose corrections from the actual VM, then Codex reviews the revised
brief and closes it before any development branch or measured run proceeds.
