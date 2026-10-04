// Package intake is Karta's trusted local intake (Stage 5): the
// unattended watcher of a protected landing area and the authenticated
// submission command, which share one implementation. Both obtain the
// exact SHA-256 of a complete delivery, authorize exactly that digest
// through the operator API with a narrow credential (scope intake_watch or
// intake_submit), and hand the bytes to the publisher with the inbox
// completion protocol in their own handoff directory. They need no signing
// key, no database credential and no route out. The publisher verifies,
// builds and switches as for any other submission, and checks the
// authorization again at the switch. See docs/adr/0006-stage5-hybrid-intake.md.
//
// # Landing area and the producer's completion contract
//
// A delivery in the landing area is
//
//	NAME.osm.pbf                  the snapshot
//	NAME.osm.pbf.provenance.json  optional provenance sidecar
//	NAME.osm.pbf.complete         completion marker, written last by rename
//
// The marker is a karta-delivery/1 JSON object naming the file with the
// SHA-256 and size the producer computed from its source copy before the
// transfer. A checksum file such as NAME.osm.pbf.sha256 is never a
// completion signal: computed from a truncated destination it would match.
// The watcher refuses a stale marker (older than the snapshot), a size or
// digest that differs, symlinks, FIFOs, devices, directories, hard links
// and files not owned by the landing owner, and processes nothing while the
// landing preflight (owner, mode, parents, filesystem type) fails.
//
// # Handoff
//
// The intake copies the delivery, hashing it, into a hidden file of its
// handoff directory, checks the PBF header against the region (advisory),
// authorizes the digest, renames the copy into place and writes the inbox
// ready marker last. It deletes its handoff files and closes its
// authorization once the publisher's submission is final. Recovery after a
// crash completes a handoff only when the copy still has the authorized
// digest and (watcher) the landing delivery is unchanged; otherwise the
// files are removed and the authorization closed.
package intake
