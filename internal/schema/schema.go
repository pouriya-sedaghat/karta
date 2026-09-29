// Package schema holds the versioned import contract of a Karta release: the
// osm2pgsql flex configuration, the SQL run around it and the vector tile
// layer catalog. Its content digest is part of every release identifier.
package schema

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Major is the schema compatibility version. The API serves only releases
// whose schema major version it supports; bump it for any change that
// alters table, function or tile-layer contracts consumed by the API.
const Major = 1

//go:embed flex/karta.lua sql/*.sql layers.json
var files embed.FS

// SQLFile is one embedded SQL script.
type SQLFile struct {
	Name string
	SQL  string
}

// Layer describes one vector tile layer emitted by karta.tile().
type Layer struct {
	ID          string            `json:"id"`
	Geometry    string            `json:"geometry"`
	MinZoom     int               `json:"minzoom"`
	Description string            `json:"description"`
	Fields      map[string]string `json:"fields"`
}

// Catalog is the tile contract: zoom range, extent and layers.
type Catalog struct {
	MinZoom int     `json:"minzoom"`
	MaxZoom int     `json:"maxzoom"`
	Extent  int     `json:"extent"`
	Buffer  int     `json:"buffer"`
	Layers  []Layer `json:"layers"`
}

// Layer returns the layer with the given id.
func (c Catalog) Layer(id string) (Layer, bool) {
	for _, l := range c.Layers {
		if l.ID == id {
			return l, true
		}
	}
	return Layer{}, false
}

// FlexConfig returns the osm2pgsql flex Lua configuration.
func FlexConfig() []byte {
	b, err := files.ReadFile("flex/karta.lua")
	if err != nil {
		panic(err) // embedded at build time
	}
	return b
}

// SetupSQL returns the script that runs before osm2pgsql.
func SetupSQL() SQLFile {
	return mustSQL("sql/000_setup.sql")
}

// PostImportSQL returns the scripts that run after osm2pgsql, in order.
func PostImportSQL() []SQLFile {
	names, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		panic(err)
	}
	sort.Strings(names)
	var out []SQLFile
	for _, n := range names {
		if path.Base(n) == "000_setup.sql" {
			continue
		}
		out = append(out, mustSQL(n))
	}
	return out
}

// Layers returns the tile layer catalog.
func Layers() Catalog {
	var c Catalog
	b, err := files.ReadFile("layers.json")
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		panic(fmt.Sprintf("schema: invalid layers.json: %v", err))
	}
	return c
}

// Revision is a digest over every embedded schema file. Any change to the
// flex configuration, SQL or layer catalog produces a new revision and
// therefore a new release identifier for the same source data.
func Revision() string {
	var names []string
	if err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	}); err != nil {
		panic(err)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	return fmt.Sprintf("s%d-%s", Major, hex.EncodeToString(h.Sum(nil))[:16])
}

func mustSQL(name string) SQLFile {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return SQLFile{Name: strings.TrimPrefix(name, "sql/"), SQL: string(b)}
}
