// Command karta-fixture converts Karta's committed OSM XML test fixtures into
// the committed PBF snapshots used by the publication tests:
//
//	go run ./cmd/karta-fixture            # rewrite testdata/fixture/snapshots/*.osm.pbf
//	go run ./cmd/karta-fixture -check     # fail if a committed snapshot differs
//
// The conversion is deterministic (package pbfwrite), so regenerating never
// changes committed bytes unless an XML source changed; the region pins in
// config/regions/fixture.json must then be updated too.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pouriya-sedaghat/karta/internal/pbfwrite"
)

// Snapshots maps each committed PBF snapshot to its XML source.
var Snapshots = []struct{ Source, Output string }{
	{"testdata/fixture/karta-fixture.osm", "testdata/fixture/snapshots/karta-fixture-a.osm.pbf"},
	{"testdata/fixture/karta-fixture-b.osm", "testdata/fixture/snapshots/karta-fixture-b.osm.pbf"},
}

func main() {
	check := flag.Bool("check", false, "compare instead of writing")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	failed := false
	for _, s := range Snapshots {
		src, err := os.Open(filepath.Join(*root, s.Source)) // #nosec G304 -- fixed repository paths
		if err != nil {
			fatal(err)
		}
		d, err := pbfwrite.ParseXML(src)
		src.Close()
		if err != nil {
			fatal(fmt.Errorf("%s: %w", s.Source, err))
		}
		b, err := pbfwrite.Encode(d, pbfwrite.Options{})
		if err != nil {
			fatal(fmt.Errorf("%s: %w", s.Source, err))
		}
		sum := sha256.Sum256(b)
		out := filepath.Join(*root, s.Output)
		if *check {
			have, err := os.ReadFile(out) // #nosec G304 -- fixed repository paths
			if err != nil || !bytes.Equal(have, b) {
				fmt.Fprintf(os.Stderr, "%s is not the conversion of %s (run go run ./cmd/karta-fixture)\n", s.Output, s.Source)
				failed = true
				continue
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { // #nosec G301 -- repository directory
				fatal(err)
			}
			if err := os.WriteFile(out, b, 0o644); err != nil { // #nosec G306 -- committed test data
				fatal(err)
			}
		}
		fmt.Printf("%s  %s  %d bytes\n", hex.EncodeToString(sum[:]), s.Output, len(b))
	}
	if failed {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "karta-fixture:", err)
	os.Exit(1)
}
