package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/pouriya-sedaghat/karta/internal/intake"
	"github.com/pouriya-sedaghat/karta/internal/operator"
)

// checkout copies the named scripts into a fresh directory tree.
func checkout(t *testing.T, scripts ...string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the scripts assume Linux tools")
	}
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))
	for _, s := range scripts {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", s))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(root, "scripts", s), b, 0o755))
	}
	return root
}

func run(t *testing.T, root string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, "scripts", name), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	must(t, err)
	return fi.Sys().(*syscall.Stat_t).Ino
}

func credentials(t *testing.T, root string) map[string]operator.Credential {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "secrets", "operator_tokens"))
	must(t, err)
	cs, err := operator.ParseCredentials(b)
	if err != nil {
		t.Fatalf("the publisher would refuse secrets/operator_tokens: %v\n%s", err, b)
	}
	m := map[string]operator.Credential{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

// Extra credentials are added to and removed from operator_tokens in place
// (the publisher's bind mount keeps the inode and reloads the file), and
// the result always parses with the publisher's own rules.
func TestOperatorCredentialScript(t *testing.T) {
	root := checkout(t, "gen-secrets.sh", "operator-credential.sh")
	if out, err := run(t, root, nil, "gen-secrets.sh"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tokens := filepath.Join(root, "secrets", "operator_tokens")
	ino := inode(t, tokens)
	if out, err := run(t, root, nil, "operator-credential.sh", "add", "local-intake", "intake_watch", "secrets/intake_watch_token"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(t, root, nil, "operator-credential.sh", "add", "alice", "intake_submit"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cs := credentials(t, root)
	tok, err := os.ReadFile(filepath.Join(root, "secrets", "alice.token"))
	must(t, err)
	a, ok := cs["alice"]
	if !ok || a.IntakeChannel() != operator.ScopeIntakeSubmit || cs["local-intake"].IntakeChannel() != operator.ScopeIntakeWatch {
		t.Fatalf("credentials %+v", cs)
	}
	if c, err := operator.Authenticate([]operator.Credential{a}, "Bearer "+strings.TrimSpace(string(tok))); err != nil || c.Name != "alice" {
		t.Fatal("alice's token file does not authenticate as alice")
	}
	if inode(t, tokens) != ino {
		t.Fatal("operator_tokens was replaced, not rewritten in place: the running publisher would not see it")
	}
	// Idempotent; another scope under the same name is refused.
	if out, err := run(t, root, nil, "operator-credential.sh", "add", "alice", "intake_submit"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := run(t, root, nil, "operator-credential.sh", "add", "alice", "status"); err == nil {
		t.Fatal("alice's scope was changed in place")
	}
	for _, name := range []string{"operator", "monitor", "Alice", "a b", "x" + strings.Repeat("y", 32)} {
		if _, err := run(t, root, nil, "operator-credential.sh", "add", name, "intake_submit"); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, err := run(t, root, nil, "operator-credential.sh", "add", "bob", "publish"); err == nil {
		t.Error("a publish credential was created by the narrow-credential script")
	}
	// Removal: refused from the publisher's next read, file deleted, inode kept.
	if out, err := run(t, root, nil, "operator-credential.sh", "remove", "alice"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := credentials(t, root)["alice"]; ok {
		t.Fatal("alice is still a credential")
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "alice.token")); !os.IsNotExist(err) {
		t.Fatal("alice's token file is still there")
	}
	if inode(t, tokens) != ino {
		t.Fatal("operator_tokens was replaced on removal")
	}
	out, err := run(t, root, nil, "operator-credential.sh", "list")
	if err != nil || strings.TrimSpace(out) != "local-intake intake_watch" {
		t.Fatalf("list: %q %v", out, err)
	}
	// Rotating the operator tokens keeps the extra credentials.
	must(t, os.Remove(filepath.Join(root, "secrets", "operator_token")))
	if out, err := run(t, root, nil, "gen-secrets.sh"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if cs := credentials(t, root); len(cs) != 3 || cs["local-intake"].Name == "" {
		t.Fatalf("after rotation: %+v", cs)
	}
}

// deliver.sh writes the completion marker last, with the source copy's
// digest and size, in the format the watcher parses.
func TestDeliverScript(t *testing.T) {
	root := checkout(t, "deliver.sh")
	src := filepath.Join(root, "src")
	landing := filepath.Join(root, "landing")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.MkdirAll(landing, 0o755))
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixture", "snapshots", "karta-fixture-a.osm.pbf"))
	must(t, err)
	snap := filepath.Join(src, "iran-2026-10-01.osm.pbf")
	must(t, os.WriteFile(snap, data, 0o644))
	must(t, os.WriteFile(snap+".provenance.json", []byte("{}\n"), 0o644))
	if out, err := run(t, root, nil, "deliver.sh", snap, landing); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	marker, err := os.ReadFile(filepath.Join(landing, "iran-2026-10-01.osm.pbf.complete"))
	must(t, err)
	c, err := intake.ParseCompletion(marker, "iran-2026-10-01.osm.pbf")
	if err != nil {
		t.Fatalf("the watcher would refuse the marker: %v\n%s", err, marker)
	}
	sum := sha256.Sum256(data)
	if c.SHA256 != hex.EncodeToString(sum[:]) || c.SizeBytes != int64(len(data)) {
		t.Fatalf("marker %+v", c)
	}
	var names []string
	es, err := os.ReadDir(landing)
	must(t, err)
	for _, e := range es {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "iran-2026-10-01.osm.pbf iran-2026-10-01.osm.pbf.complete iran-2026-10-01.osm.pbf.provenance.json" {
		t.Fatalf("landing holds %v (temporary names left behind?)", names)
	}
	// The marker is never older than the snapshot (completion_stale).
	mi, _ := os.Stat(filepath.Join(landing, "iran-2026-10-01.osm.pbf.complete"))
	si, _ := os.Stat(filepath.Join(landing, "iran-2026-10-01.osm.pbf"))
	if mi.ModTime().Before(si.ModTime()) {
		t.Fatal("the marker is older than the snapshot")
	}
	// No overwrite of a delivery; an unexpected digest delivers nothing.
	if _, err := run(t, root, nil, "deliver.sh", snap, landing); err == nil {
		t.Fatal("an existing delivery was overwritten")
	}
	if _, err := run(t, root, []string{"EXPECTED_SHA256=" + strings.Repeat("0", 64)}, "deliver.sh", snap, landing, "other"); err == nil {
		t.Fatal("a file with another digest was delivered")
	}
	if _, err := os.Stat(filepath.Join(landing, "other.osm.pbf")); !os.IsNotExist(err) {
		t.Fatal("a refused delivery left files")
	}
}
