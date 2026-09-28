package schema

import (
	"regexp"
	"strings"
	"testing"
)

func TestContract(t *testing.T) {
	first, second := Revision(), Revision()
	if !regexp.MustCompile(`^s1-[0-9a-f]{16}$`).MatchString(first) || first != second {
		t.Fatalf("revision %q / %q", first, second)
	}
	c := Layers()
	if c.MinZoom != 0 || c.MaxZoom != 16 || c.Extent != 4096 || len(c.Layers) != 7 {
		t.Fatalf("catalog %+v", c)
	}
	post := PostImportSQL()
	if len(post) == 0 || post[0].Name != "010_repair.sql" || SetupSQL().Name != "000_setup.sql" {
		t.Fatalf("post-import order %v", post)
	}
	var tileSQL string
	for _, f := range post {
		if f.Name == "060_tiles.sql" {
			tileSQL = f.SQL
		}
	}
	// Every catalogued layer is emitted by karta.tile() under the same name.
	for _, l := range c.Layers {
		if !strings.Contains(tileSQL, "'"+l.ID+"', 4096") {
			t.Errorf("layer %s is not emitted by karta.tile()", l.ID)
		}
		if len(l.Fields) == 0 || l.Geometry == "" {
			t.Errorf("layer %s lacks fields or geometry", l.ID)
		}
	}
	if !strings.Contains(string(FlexConfig()), "osm2pgsql.process_relation") {
		t.Error("flex config incomplete")
	}
}
