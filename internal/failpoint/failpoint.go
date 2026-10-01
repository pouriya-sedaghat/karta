// Package failpoint stops the process at named points of the publication
// protocol, to test recovery from a crash at each critical transition.
//
// Failpoints are off unless KARTA_FAILPOINTS lists them (comma-separated).
// Hitting an enabled failpoint exits the process immediately with status 99,
// without running deferred cleanup, like a kill -9 or a power loss at that
// point. The publisher and importer log the enabled list at start. It is a
// test facility: never set KARTA_FAILPOINTS in a deployment.
package failpoint

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ExitCode is the status a failpoint exits with.
const ExitCode = 99

// Known failpoints, in protocol order.
var Known = []string{
	"stage.after_copy",
	"build.after_create",
	"build.after_import",
	"build.after_rename",
	"activate.before_commit",
	"activate.after_commit",
	"cleanup.after_mark",
	"cleanup.after_drop",
	// Stage 3: the publisher, after an online delivery's signed manifest
	// was verified and its serial recorded.
	"online.after_verify",
	// Stage 3: the fetcher.
	"fetch.after_manifest",
	"fetch.mid_download",
	"fetch.after_download",
	"fetch.before_marker",
	"fetch.after_marker",
}

var enabled = parse(os.Getenv("KARTA_FAILPOINTS"))

func parse(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			m[p] = true
		}
	}
	return m
}

// Enabled returns the enabled failpoints and any names that are not known.
func Enabled() (on, unknown []string) {
	known := map[string]bool{}
	for _, k := range Known {
		known[k] = true
	}
	for p := range enabled {
		if known[p] {
			on = append(on, p)
		} else {
			unknown = append(unknown, p)
		}
	}
	sort.Strings(on)
	sort.Strings(unknown)
	return on, unknown
}

// exit is replaced in tests.
var exit = os.Exit

// Hit exits the process if the named failpoint is enabled.
func Hit(name string) {
	if enabled[name] {
		fmt.Fprintf(os.Stderr, `{"level":"ERROR","msg":"failpoint hit, exiting","failpoint":%q,"exit_code":%d}`+"\n", name, ExitCode)
		exit(ExitCode)
	}
}
