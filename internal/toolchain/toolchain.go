// Package toolchain reads the versions of the database components that shape
// a release's tiles and search results, so they can be recorded in the
// release identity at import and compared with the serving database before
// a release is served.
//
// Tiles and normalized search terms are computed on the serving PostgreSQL
// at request time. If the database image is upgraded, a release built with
// other PostgreSQL, PostGIS, GEOS, PROJ, pg_trgm or ICU versions could return
// different bytes at the same immutable URL. The API therefore refuses to
// serve a release whose recorded serving toolchain differs from the running
// one (Compare), and the publisher refuses to activate or roll back to it.
package toolchain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ServingKeys are the recorded components that are used at request time.
// osm2pgsql is recorded too, but only runs at import.
var ServingKeys = []string{"postgresql", "postgis", "geos", "proj", "pg_trgm", "icu"}

// Querier runs one query.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Read returns the serving toolchain of the database q is connected to. The
// ICU version is the one of the loaded ICU library for the root collation
// (pg_collation_actual_version), not the version recorded in the catalog when
// the database was created, so a library change after the template was built
// is detected.
func Read(ctx context.Context, q Querier) (map[string]string, error) {
	var pg, postgis, geos, proj, trgm, icu string
	err := q.QueryRow(ctx, `
SELECT split_part(current_setting('server_version'), ' ', 1),
       public.postgis_lib_version(),
       public.postgis_geos_version(),
       split_part(public.postgis_proj_version(), ' ', 1),
       (SELECT extversion FROM pg_extension WHERE extname = 'pg_trgm'),
       COALESCE((SELECT pg_collation_actual_version(oid) FROM pg_collation WHERE collname = 'und-x-icu'), '')`).Scan(
		&pg, &postgis, &geos, &proj, &trgm, &icu)
	if err != nil {
		return nil, fmt.Errorf("read database toolchain versions: %w", err)
	}
	return map[string]string{"postgresql": pg, "postgis": postgis, "geos": geos, "proj": proj, "pg_trgm": trgm, "icu": icu}, nil
}

// Compare returns an error naming every serving component whose recorded
// version differs from the running one (or is missing from the record).
func Compare(recorded, running map[string]string) error {
	var diffs []string
	keys := append([]string(nil), ServingKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		r, ok := recorded[k]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s not recorded", k))
		case r != running[k]:
			diffs = append(diffs, fmt.Sprintf("%s %s at import, %s now", k, r, running[k]))
		}
	}
	if len(diffs) > 0 {
		return fmt.Errorf("serving database toolchain differs from the one the release was built with (%s); re-import the snapshot on this database image",
			strings.Join(diffs, "; "))
	}
	return nil
}
