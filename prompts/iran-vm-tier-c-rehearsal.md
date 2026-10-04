# Draft brief: full-Iran rehearsal on the owner's Windows-hosted VM (tier C)

Status: **draft for owner and Claude Code review**. This is a measurement and
recovery rehearsal of the merged Stage 5 code, not a new implementation stage
or a production acceptance test. The production host does not yet exist. The
baseline is `main` at merge commit `ab7dca3` (PR #12); both jobs of its CI
run 37183595394 passed on fixtures. Do not infer full-Iran capacity from those
fixtures or from the approximately 200 MB PBF size.

The owner will open a separate Claude Code session on the VM. Reading this
brief does not authorize connecting to the VM, downloading a PBF, generating
a production signing key, changing a running database, or deploying to a
production host. Agree on the VM work and its data source in that session.

## Goals and inputs

Measure one **fixed, exact Geofabrik Iran PBF** through Karta's common
verification/build path. Exercise protected local intake first; if the host
preflight and owner-approved writer boundary permit it, exercise the watcher
separately. Confirm that serving remains available across a publication and
rollback, and measure disk, memory, CPU, WAL, time and request latency. Record
both successes and refusals. Keep Stage 3/5 online source and bridge disabled
unless a separate, clearly scoped test with a throwaway key and controlled
source is approved; no production key or automatic Geofabrik polling here.

Before commands that mutate the VM, obtain from the owner:

1. The VM access method and scope, OS/filesystem, CPU/RAM, free and total
   storage, Docker/Compose version, and whether an existing Karta database or
   Chitgar release must be preserved.
2. The chosen Iran PBF's path **on the VM** or permission for a specified
   download, plus origin URL, observation time, expected SHA-256 and size
   recorded independently from the source copy. Record Geofabrik's `.md5`,
   `Last-Modified`, header timestamp and box as metadata, not as a signature.
   Never use the moving `iran-latest` URL as the identity of a fixed snapshot.
3. How much VM disk may be used, where to store a backup outside the volume
   being tested, and whether restoring in a second, isolated Compose project
   is feasible. The import is allowed only after a capacity estimate for
   database, candidate, retained release, intake copy and backup.
4. The owner's choice of named `intake_submit` command and, only if wanted,
   the watcher landing account and its writers. Generated token files stay
   under the private `secrets/` directory. A custom token path under a
   traversable directory is unsafe with the current script (mode 0644).

Do not put the PBF, token, private key, `.env`, backup, or unredacted VM logs
in Git. Record nonsecret evidence and exact versions in a separate report.

## Procedure for Claude Code

1. Sync and inspect the selected commit and the actual VM before modifying
   anything. Read `docs/runbook.md` (local intake, command-line import,
   backup/restore, capacity), `docs/operations.md` (evidence tiers, upgrade,
   Iran region, owner inputs), ADR 0006, `config/regions`, the relevant
   scripts and Compose settings. Report any difference from this brief.
2. If a database exists, make and verify a **pre-upgrade backup** before
   starting the v5 publisher. The old code cannot read a v5 registry; a
   rollback to the old build requires restoring the pre-upgrade backup.
   Prefer a separate Compose project/volumes for the Iran rehearsal. Do not
   run `restore ... REPLACE=1`, prune volumes, reset data, or replace an
   existing release as a shortcut. Rehearse restore only in an isolated
   project and confirm `restore-check` there.
3. Confirm the exact PBF digest and size against the independent record and
   collect its header timestamp and bounding box. Stop on mismatch. Draft
   `config/regions/iran.json` with `karta region-draft` against that fixed
   file as documented in operations; review the generated pin and box. Do
   not copy Chitgar count or map checks. Leave `require_provenance` off for a
   raw Geofabrik file unless a reviewed sidecar is supplied.
4. Establish a disk/time budget and explicit abort conditions before a full
   build. Use `karta import --no-activate` for the first measured Iran build
   as documented in the runbook (if another region is active, review the
   explicit `--allow-region-change` flag first). Preserve the current active
   release. Record
   phase timings, peak CPU/RSS, disk and WAL growth, release database size,
   the import report, feature counts and validation failures. If capacity or
   policy refuses it, report the refusal; do not weaken checks to force it
   through.
5. Derive Iran-specific `min_counts`, `max_drop_fraction`, representative
   Persian place searches and tile locations from the measured, inspected
   release. Review the region configuration change in a branch/PR before
   activation. Then exercise one authorized local delivery with the named
   `intake_submit` credential and independent source digest/size. Record its
   audit and outcome. A submission of the same digest after the no-activate
   build may exercise duplicate/ready handling rather than a fresh build:
   label it accordingly. A full intake-build rehearsal needs a fresh isolated
   project or another vetted snapshot and its own disk budget. Avoid the
   last-resort `ATTEST` path for this rehearsal.
6. If the watcher is in scope, place its landing directory on the VM's own
   supported POSIX filesystem, restrict writers, run `make intake-check` and
   require `ok: true`. Transfer the PBF from Windows or a shared folder into
   that directory under a temporary name and write the producer `.complete`
   marker **last** with the source-copy digest and size. Do not mount a
   hypervisor/SMB shared folder as the trusted landing area. Test duplicate
   delivery, incomplete marker, changed bytes and safe refusal without
   unnecessarily rebuilding the full Iran database each time.
7. With a serving Iran release, run a bounded request mix of search, style
   and tiles during a controlled publication/rollback where feasible. Record
   request counts, errors, p50/p95/p99 latency and resource use. Check
   release pinning, active pointer, audit, rollback pause of watcher
   activation and offline serving after network loss. Do not claim a
   performance objective until the owner chooses workload and limits.

## Evidence and exit criteria

Deliver a concise report with: exact commit and image digests; VM resource
inventory; fixed PBF provenance, SHA-256, size, timestamp and box; before/
after storage and backup verification; region configuration diff; full
import report and metrics; command/watcher outcomes; serving and rollback
observations; failures and unresolved risks. Redact credentials and host
identifiers. Label results **tier C on this VM**. Tier D production-host
capacity, key custody, online timing, SLOs and target-host recovery remain
open. Return the VM to an agreed state and report which services and
volumes remain running; do not silently enable bridge or watcher.

The owner and Codex review this brief before execution. Claude Code may
propose corrections from the actual VM, then Codex reviews the revised
brief and closes it before any development branch or measured run proceeds.
