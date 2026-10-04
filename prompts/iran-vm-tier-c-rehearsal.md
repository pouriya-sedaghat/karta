# Draft brief: full-Iran rehearsal on the owner's Windows-hosted VM (tier C)

Status: **repository review complete; SSH inventory supplied by the owner;
pending Windows/source evidence and measured-run decisions**. Claude Code reviewed this brief on 2026-10-04 and Codex checked
the revised claims against the merged code. No VM results are claimed.
This is a measurement and recovery rehearsal of the merged Stage 5 code, not a new implementation stage or a
production acceptance test. The production host does not yet exist. The
baseline is `main` at merge commit `ab7dca3` (PR #12); both jobs of its CI
run 37183595394 passed on fixtures (rechecked 2026-10-04). Do not infer
full-Iran capacity from those fixtures or from the roughly 230 MB
`iran-261002.osm.pbf` file.

**Tier C, never tier D.** Every result is evidence about this VM only
(`docs/operations.md`, evidence tiers). It says nothing about production-host
capacity, limits, objectives, key custody or recovery (tier D). Label reports
and capacity runs `tier-c-vm-…`; the runbook's `tier-d-iran-…` examples are
for the future production host.

**SSH inventory (2026-10-04).** The owner supplied a Claude Desktop SSH
report from the Ubuntu VM. Codex has not directly accessed that VM. The
following are observations attributed to Claude's report, not independent
measurements by Codex: VMware, Ubuntu 26.04.1, 4 vCPUs, 7.20 GiB RAM,
4 GiB swap, and about 81.5 GiB free on `/`. Docker Engine runs in the
guest (29.8.1, Compose 5.5.1); `/var/lib/docker`, `/var/lib/containerd`,
swap and the PBF share the ext4 root. The unprivileged inventory found no
Karta checkout, project, container, image, volume or database; about 2.644
GB of old build cache and eight build records remain. Root-owned Docker
directories were not read directly, though Docker's object listings were
queried. No cache cleanup is necessary for the inventory.

The reported VM copy of `iran-261002.osm.pbf` is 229,655,710
bytes, SHA-256
`4cbf2a95cffee119615e6862fd2257817356c263c15729477e21a4f5b8aedc5d`.
Its header reports 2026-10-02T20:21:34Z, sequence 4925 and box
44.0230330,24.0394750 to 63.3541300,39.7904470. Only the header was
read; an independent digest from the original Windows file and full-file
structural verification remain open. The reported MD5 has not been compared
with Geofabrik's sidecar. Never mount the user's home directory into a container: it
contains unrelated private files, and its reported 0750 mode blocks the
container UID. Stage verified bytes in a dedicated input directory for the run.

Chrony was synchronized at inventory time, but the journal reportedly
shows 13 forward time jumps and a gap consistent with guest suspension or
host sleep. Establish the cause and prevent either during measured runs.
The Windows host's RAM, free space on the volume holding the virtual disk,
disk growth mode, snapshots and sleep settings are not observable from the
guest and remain owner inputs.

Claude runs on the guest in SSH mode and needs its model connection. For
application-disconnected tests, isolate Karta's network while leaving the
SSH/Claude control path available; a fully disconnected guest requires
the agent to run on the Windows host and control the guest over SSH.
No PBF download, key generation, migration or publication starts until
the remaining owner inputs and resource budget have been reviewed.

## Goals

First verify and manually publish a fixed, exact Geofabrik Iran PBF through
Karta's common verification/build path. The named `intake_submit` command
is the initial local delivery; the protected-folder watcher is a separate
optional check after its host preflight and writer boundary are reviewed.
Once the manual release passes Iran-specific acceptance and the VM has room
for another complete release, test an opt-in online update through the
controlled bridge from a **genuinely newer** Geofabrik Iran extract.
The bridge's signing key for this VM is a throwaway test key, never a
production key. Both paths must preserve serving and audit. Measure disk,
memory, CPU, WAL, time and latency; only claim full-Iran rollback if two
separately accepted releases exist. Neither path is production acceptance.

## Candidate snapshot

The owner reports downloading `iran-261002.osm.pbf` on 2026-10-03. At the
time of this brief revision, Geofabrik's Iran listing names this dated PBF
as 229,655,710 bytes, modified 2026-10-02 22:37 UTC. The VM copy's size, SHA-256 and header have been reported above. Record
SHA-256 and size from the original Windows copy, compare them with the VM
copy, and record Geofabrik's MD5 as distributor metadata (not authentication).
The dated filename does not prove that the local bytes are complete.

The repository also records an older `iran-260927.osm.pbf` digest as an
unverified claim from the Chitgar extract sidecar. That digest does not
authorize or identify the owner's new `iran-261002.osm.pbf`. Derive the
Iran region box and initial pin from the verified new file. The bridge
will later inspect `iran-latest.osm.pbf` and publish its own immutable
copy if the extract is newer and passes the same region and time rules.

## Owner inputs before anything on the VM changes

1. **Windows host and final VM settings:** VMware product/version, host RAM,
   free space on the volume holding the virtual disk, disk growth mode,
   existing snapshots and host sleep settings. Verify filesystem and free
   space for the new checkout and landing area. Compose 5.5.1 meets the
   brief's minimum but has not been exercised with Karta here. Investigate
   the recorded time jumps; prevent sleep, suspend and checkpoints during
   measured runs and check the clock before and after.
2. **Existing Karta:** the SSH inventory found no Karta checkout, Compose
   project, images, volumes or database, only build cache and records. If
   any later check finds a database, record its commit and schema, identify
   what to preserve and consult "Existing database" before connecting a
   Stage 5 process. Cache cleanup is optional.
3. **Snapshot:** get SHA-256 and size for the original Windows file, then
   compare them with the reported VM copy. Record the dated Geofabrik URL,
   observation time and `.md5` (if reachable), header timestamp and box
   as metadata, not as a signature.
   A future `iran-latest` URL may identify the distributor for the bridge,
   never the fixed identity of the manually delivered bytes.
4. **Disk:** how much the rehearsal may use, the storage budget value
   ("Capacity"), the abort thresholds, where backups go (outside the
   measured filesystem, preferably off the VM), and whether an isolated
   restore project fits (it needs room for the whole cluster again). No
   second guest filesystem was reported. A second logical volume on the
   same virtual disk can separate measurement but does not protect a
   backup from failure of that disk or its Windows host volume.
5. **Access:** the person holding the named `intake_submit` credential and,
   only if the watcher is in scope, the landing account and its writers.
   Token files stay under the private `secrets/` directory:
   `scripts/operator-credential.sh` makes them mode 0644 (readable by
   container UID 65532), so a custom token path under a traversable
   directory is unsafe.
6. **Network and scope:** determine and document the route for image builds
   and the future bridge container to reach registries and Geofabrik. The
   reported Docker proxy listens on host loopback, which a normal container
   cannot use through its own loopback address. Earlier registry 403 errors
   followed by successful builds do not establish that this is the only
   working route. Obtain owner approval for a one-off live Geofabrik
   update test after the manual baseline; agree on a real contact in the
   bridge user agent, custody of a disposable VM test key, polling and stop
   time, enough space for two releases plus bridge spool/outbox/staging,
   and the load mix and limits.
   The bridge and fetcher remain off until this phase.

Do not put the PBF, tokens, keys, `.env`, backups, VM paths, UIDs, host names
or unredacted logs in Git. At the `ab7dca3` baseline `.env` is not ignored;
PR #13 proposes an ignore rule, but verify the actual checkout and never
stage `.env` or secrets regardless. During VM execution the only intended
repository change is the reviewed `config/regions/iran.json`, through a
branch and PR. Record nonsecret evidence in a separate report.

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
  the listed 229,655,710-byte `iran-261002` file, the estimate is about
  8.6 GiB. So the first Iran build
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
  memory limits add up to 10.5 GiB, while the reported VM has 7.20 GiB RAM
  and 4 GiB swap on `/`. Limits are ceilings, not predicted use. Increase
  RAM if feasible or agree a swap/OOM abort rule before the run. An OOM
  kill is a result to record, not to work around silently.
* **Settings that `.env` does not reach:** `KARTA_OSM2PGSQL_SLIM`,
  `KARTA_DB_VOLUME_PATH`, `KARTA_CANDIDATE_SIZE_FACTOR` and
  `KARTA_MAX_INPUT_MB` are read by the code but not passed to the publisher
  or importer by `compose.yaml`. Change them only with
  `docker compose run -e …` for the one-off importer, or an uncommitted
  override file, and record that. `KARTA_RELEASE_STORAGE_BUDGET_MB`,
  `KARTA_PUBLISH_TIMEOUT` (6 h) and the memory limits are passed.

## Procedure for Claude Code

1. **Read-only inventory (SSH report received).** Review the observed VM
   facts and obtain the missing Windows/source inputs before changes. The
   prior session fetched only this brief over HTTPS and found no checkout.
   If a follow-up inventory is needed, use read-only commands such as
   `uname -a`, `free -h`, `df -hT`, `docker info`, `docker compose ls -a`,
   `docker ps -a`, `docker volume ls`, `timedatectl` and chrony status.
   Read the runbook, operations guide, ADR 0006, scripts, region config and
   Compose files at the agreed commit via HTTPS if the checkout is absent.
   Report any difference and resolve resource and network questions before
   checking out or running Karta.
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
3. **Snapshot and region draft.** Compare the original Windows file's SHA-256
   and size with the reported VM copy; stop on a mismatch. Stage the PBF
   alone in a dedicated directory accessible to container UID 65532; do
   not mount the home directory or any unrelated private files. Draft
   the region: `docker compose -p <project> run --rm -T --no-deps
   -v <dir>:/in:ro --entrypoint /usr/local/bin/karta importer region-draft
   --snapshot /in/<file> --id iran --name "Iran"`, with stdout to
   `config/regions/iran.json` and stderr kept separate. It needs no
   database, scans the whole file, prints the box source and counts, pins
   `expected_sha256`, sets `require_provenance` only if a sidecar is present
   (keep it off for a raw Geofabrik file unless a reviewed sidecar is
   supplied), and centres the default view in the box. Review the actual `261002`
   pin, box and view; do not assume they match the older `260927` file. Do not copy
   Chitgar checks.
4. **Budget, then the measured no-activate import.** Agree the budget, the
   abort thresholds (free space on `/` and on the Windows host volume, OOM
   kill, swap, the deadline) and the sampling first. Prevent guest suspend
   and host sleep during measurement, and check chrony and the clock before
   and after. Start
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
5. **Thresholds and a validated activation.** Derive `min_counts`,
   `max_drop_fraction`, representative Persian searches and tile checks
   from the inspected release, and review the region file in a branch/PR.
   The no-activate release was built under the draft's empty checks. The
   release id omits the validation policy, and a resubmission with the same
   identity returns that `ready` release without rerunning the new checks.
   **Do not count its activation as acceptance under the reviewed policy.**
   Choose the route based on the actual reviewed configuration:

   * If a legitimate reviewed change to the default view or other served
     identity value yields a new release id, the named intake delivery of
     the same pinned snapshot can make a fresh build with the reviewed
     checks. Do not change the view merely to force a new id. Budget for
     two full Iran databases, WAL and the candidate estimate.
   * If the identity remains unchanged, build the snapshot anew with the
     reviewed checks in a separate isolated project. Alternatively,
     implement and review a code fix that revalidates an existing `ready`
     release before activation. A repeated submission in the first project
     only demonstrates reuse or refusal; it does not prove the new checks.

   In either case, use an independently recorded SHA-256 and size and a
   named `intake_submit` credential (`make intake-submit … SHA256=… SIZE=…`).
   Set `KARTA_INTAKE_DIR=/data/intake` and the publisher's region to
   `iran`; the input file must be readable by container UID 65532.
   Record authorization, audit, report and outcome. Avoid the last-resort
   `ATTEST` path. A draft-policy `ready` release is not a validated
   rollback target for the reviewed policy merely because the API allows
   switching to it.
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
7. **Serving, publication and optional rollback.** With an active
   reviewed Iran release, run a bounded load with
   `scripts/capacity-run.sh tier-c-vm-iran-… -duration … -concurrency …`
   (`BASE` and `COMPOSE` set; use `-follow` only across a real switch).
   `karta-load` samples tiles uniformly across Iran's bounds and defaults
   to Tehran-centric searches, so its workload is synthetic, not a
   production traffic model. Record requests, errors, p50/p95/p99 latency
   and resource use during serving and any publication.

   A meaningful full-Iran rollback under load needs two separately
   reviewed and accepted releases. Prefer a second dated Iran snapshot
   with independent provenance and enough disk. A legitimate output
   revision of the same PBF can test the control-plane switch, but report
   explicitly that it is not a data-version rollback. The draft-policy
   no-activate release alone does not satisfy this acceptance condition.
   If there is only one accepted release, omit the full-Iran rollback
   experiment and rely on the fixture CI for that behavior. Check the
   active pointer, audit and release pinning; check watcher activation
   pause only if a rollback was actually performed. Test Karta's disconnected serving by isolating its network without ending
   the Claude Desktop SSH session; a guest-wide internet cut needs a host-run
   agent instead. Do not claim a performance objective until the owner
   chooses the workload and limits.
8. **Optional live online update, after manual acceptance.** First establish
   and review the image-build and bridge-container network routes; Docker's
   reported proxy is bound to host loopback and containers do not inherit
   it. Earlier `gcr.io` 403s followed by successful builds do not prove
   that the proxy is the only usable route. Leave Claude's SSH/model route
   available. Verify that
   Geofabrik currently serves an extract whose **PBF header timestamp** is
   newer than the active `iran-261002` release; the dated filename alone
   is not the forward-rule evidence. Review the distributor's current
   download terms and configure a real contact in `user_agent`.
   First check capacity for another complete candidate and retained
   release, bridge spool, fetcher outbox, staging and a backup; abort if
   the agreed disk or memory reserve cannot be maintained. Use the
   bridge's separate acquire/sign/serve roles with a disposable signing
   key whose public key is pinned in Karta's reviewed source config.
   Never commit the private key. The bridge alone contacts Geofabrik;
   Karta's fetcher trusts only the bridge's signed manifest. With
   `require_operator_authorization` left false in the source config,
   that signature authorizes the newer digest; do not silently broaden
   any other publication path. Confirm acquire checks, exact downloaded
   digest/size, provenance record, manifest serial, signer verification,
   fetcher and publisher checks, audit, serving during switch and active
   release. A Geofabrik boundary/box change or a non-newer extract is a
   recorded refusal/no-update, not a reason to weaken region or forward
   policy. If no newer extract exists when tested, report that the online
   update path could not be demonstrated live. Stop bridge/fetcher after
   the bounded test unless the owner explicitly chooses to keep them on.
   Only now consider rollback to the already accepted manual release,
   with load and the normal activation-pause audit.
9. **Backup and isolated restore.** Run `make backup` of the rehearsal
   project to the agreed location. Restore it into a second isolated
   project with other ports (runbook, "Backup and restore") and confirm
   `restore-check` passes there. Check the clock (`timedatectl`) before
   and after any VM suspend or resume, and do not suspend during a build.

## Evidence and exit criteria

Deliver a concise report with:

* exact commit, image tags and digests, and project name;
* the VM resource inventory, including the Windows host volume;
* the manual `iran-261002` provenance, SHA-256, size, timestamp and box,
  and whether the local file matches the independently recorded bytes;
* if the online phase ran, the newer Geofabrik snapshot's digest, size,
  timestamp, bridge manifest serial/key id, source checks, publication
  outcome and final bridge/fetcher state;
* the budget, abort thresholds and any override, with their values;
* before and after storage, and backup verification;
* the region configuration diff and how the reviewed checks were executed;
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
