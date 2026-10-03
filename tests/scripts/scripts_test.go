// Package scripts_test runs scripts/backup.sh and scripts/restore.sh against
// stand-ins for `docker` and `docker compose` that record every call and
// act on host directories. They exercise what is hard to provoke with real
// containers: a failed or interrupted copy of the fetcher's outbox, and the
// restore preflight that must refuse before anything is changed. The real
// end-to-end runs are in tests/integration (TestOperations).
package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeDocker stands in for the docker CLI. State lives in $FAKE_STATE:
// db_contents (what the database volume holds: cluster or partial; absent:
// empty), online (the outbox host directory the publisher mounts; empty:
// none), outbox_copy (ok, fail or hang). Every call is appended to
// $FAKE_STATE/calls.
const fakeDocker = `#!/bin/sh
st=$FAKE_STATE
log() { echo "$*" >> "$st/calls"; }
mount() { # host path mounted at container path $1 in the call's arguments
  want=$1; shift
  while [ $# -gt 0 ]; do
    if [ "$1" = -v ]; then
      spec=${2%:ro}
      case "$spec" in *:"$want") echo "${spec%:"$want"}"; return ;; esac
    fi
    shift
  done
}
case "$1" in
  inspect)
    case "$3" in
      *Config.Image*) echo postgis/postgis:test ;;
      *compose.project*) echo karta ;;
      */var/lib/postgresql*) echo pgdata-vol ;;
      */data/online*) cat "$st/online" 2>/dev/null ;;
    esac ;;
  image) echo sha256:test ;;
  volume) log "volume rm $3"; rm -f "$st/db_contents" ;;
  run)
    all="$*"
    case "$all" in
      *pg_basebackup*)
        d=$(mount /backup "$@"); log "pg_basebackup rate=$(eval echo \${$#})"
        echo base > "$d/base.tar.gz"; echo wal > "$d/pg_wal.tar.gz"; echo manifest > "$d/backup_manifest" ;;
      *pg_verifybackup*) log "pg_verifybackup" ;;
      *PG_VERSION*) log "inspect database"; cat "$st/db_contents" 2>/dev/null || echo empty ;;
      *"ls -A /v"*) log "inspect outbox"; if [ -z "$(ls -A "$(mount /v "$@")")" ]; then echo empty; else echo files; fi ;;
      *"tar -xzf /backup/base.tar.gz"*) log "extract database"; echo cluster > "$st/db_contents" ;;
      *"--exclude=./.partial"*)
        log "copy outbox"
        case "$(cat "$st/outbox_copy" 2>/dev/null)" in
          fail) exit 2 ;;
          hang) touch "$st/hanging"; exec sleep 600 ;;
        esac
        tar -cf "$(mount /dst "$@")/online.tar" -C "$(mount /src "$@")" . ;;
      *"-xpf /online.tar"*)
        log "restore outbox"; d=$(mount /dst "$@")
        find "$d" -mindepth 1 -delete; tar -xpf "$(mount /online.tar "$@")" -C "$d" ;;
      *"find /dst -mindepth 1 -delete"*) log "empty outbox"; find "$(mount /dst "$@")" -mindepth 1 -delete ;;
      *) log "unexpected docker run: $all"; exit 99 ;;
    esac ;;
  *) log "unexpected docker: $*"; exit 99 ;;
esac
`

// fakeCompose stands in for `docker compose`. $FAKE_STATE/fetcher_running
// and publisher_running mark running services (down stops both; with
// $FAKE_STATE/write_on_down, the fetcher writes its outbox as it stops);
// registry-summary prints $FAKE_STATE/summary.json.
const fakeCompose = `#!/bin/sh
st=$FAKE_STATE
log() { echo "$*" >> "$st/calls"; }
while [ $# -gt 0 ]; do
  case "$1" in --profile|-p|-f) shift 2 ;; *) break ;; esac
done
case "$*" in
  "ps -q db"|"ps -a -q db") echo dbc ;;
  "ps -a -q publisher") echo pubc ;;
  "ps -q --status running fetcher") [ ! -f "$st/fetcher_running" ] || echo fetc ;;
  "stop fetcher") log "stop fetcher"; rm -f "$st/fetcher_running" ;;
  "start fetcher") log "start fetcher"; touch "$st/fetcher_running" ;;
  down)
    log "compose down"
    if [ -f "$st/write_on_down" ] && [ -f "$st/fetcher_running" ]; then echo late > "$(cat "$st/online")/late-delivery"; fi
    rm -f "$st/fetcher_running" "$st/publisher_running" ;;
  "up -d api publisher") log "compose up -d api publisher"; touch "$st/publisher_running" ;;
  *registry-summary*) cat "$st/summary.json" ;;
  *restore-check*) log "restore-check $*"; echo '{"passed": true}' ;;
  "exec -T db postgres --version") echo "postgres (PostgreSQL) 18.6" ;;
  *healthcheck*) ;;
  *) log "compose $*" ;;
esac
`

type env struct {
	t                        *testing.T
	root, bin, state, outbox string
	backups                  string
}

// setup builds a checkout with the two scripts, a stand-in for
// rotate-db-password.sh, configuration and secrets, and the fake tools.
func setup(t *testing.T) *env {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the scripts and their stand-ins assume Linux tools")
	}
	base := t.TempDir()
	e := &env{t: t, root: filepath.Join(base, "repo"), bin: filepath.Join(base, "bin"), state: filepath.Join(base, "state"),
		outbox: filepath.Join(base, "outbox"), backups: filepath.Join(base, "backups")}
	for _, d := range []string{"scripts", "config/regions", "config/sources", "secrets"} {
		must(t, os.MkdirAll(filepath.Join(e.root, d), 0o755))
	}
	for _, d := range []string{e.bin, e.state, e.outbox} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for _, s := range []string{"backup.sh", "restore.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", s))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(e.root, "scripts", s), b, 0o755))
	}
	write(t, filepath.Join(e.root, "scripts", "rotate-db-password.sh"), "#!/bin/sh\necho \"$2: password set\"\n", 0o755)
	write(t, filepath.Join(e.root, "config", "regions", "x.json"), "{}\n", 0o644)
	for _, s := range []string{"db_superuser_password", "db_importer_password", "db_api_password", "operator_tokens"} {
		write(t, filepath.Join(e.root, "secrets", s), "secret-"+s+"\n", 0o600)
	}
	write(t, filepath.Join(e.bin, "docker"), fakeDocker, 0o755)
	write(t, filepath.Join(e.bin, "compose"), fakeCompose, 0o755)
	write(t, filepath.Join(e.state, "summary.json"), `{"active_release_id": "ra"}`+"\n", 0o644)
	e.mountOutbox(true)
	return e
}

func (e *env) mountOutbox(on bool) {
	v := ""
	if on {
		v = e.outbox + "\n"
	}
	write(e.t, filepath.Join(e.state, "online"), v, 0o644)
}

func (e *env) set(name string, on bool) {
	p := filepath.Join(e.state, name)
	if on {
		write(e.t, p, "", 0o644)
	} else {
		_ = os.Remove(p)
	}
}

// database sets what the database volume holds: "cluster", "partial" or
// "" (empty).
func (e *env) database(contents string) {
	p := filepath.Join(e.state, "db_contents")
	if contents == "" {
		_ = os.Remove(p)
		return
	}
	write(e.t, p, contents+"\n", 0o644)
}

func (e *env) databaseContents() string {
	b, _ := os.ReadFile(filepath.Join(e.state, "db_contents"))
	return strings.TrimSpace(string(b))
}

func (e *env) has(name string) bool {
	_, err := os.Stat(filepath.Join(e.state, name))
	return err == nil
}

func (e *env) command(script string, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.Command(filepath.Join(e.root, "scripts", script), args...)
	cmd.Env = append(os.Environ(), "PATH="+e.bin+":"+os.Getenv("PATH"), "COMPOSE="+filepath.Join(e.bin, "compose"),
		"FAKE_STATE="+e.state, "KARTA_RESTORE_REPORTS="+filepath.Join(e.backups, "reports"))
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

func (e *env) run(script string, extraEnv []string, args ...string) (string, int) {
	e.t.Helper()
	out, err := e.command(script, extraEnv, args...).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	must(e.t, err)
	return string(out), 0
}

// calls returns the recorded calls and forgets them.
func (e *env) calls() []string {
	p := filepath.Join(e.state, "calls")
	b, _ := os.ReadFile(p)
	_ = os.Remove(p)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// backup takes a successful backup and returns its directory.
func (e *env) backup(withOutbox bool) string {
	e.t.Helper()
	e.mountOutbox(withOutbox)
	write(e.t, filepath.Join(e.outbox, "backed-up-delivery"), "from the backup\n", 0o644)
	out, code := e.run("backup.sh", nil, e.backups)
	if code != 0 {
		e.t.Fatalf("backup: %d %s", code, out)
	}
	entries, err := filepath.Glob(filepath.Join(e.backups, "karta-*"))
	must(e.t, err)
	if len(entries) != 1 {
		e.t.Fatalf("backups: %v", entries)
	}
	must(e.t, os.Remove(filepath.Join(e.outbox, "backed-up-delivery")))
	e.mountOutbox(true)
	e.calls()
	return entries[0]
}

func (e *env) noBackupLeft() {
	e.t.Helper()
	entries, _ := os.ReadDir(e.backups)
	for _, d := range entries {
		if d.Name() != "reports" {
			e.t.Errorf("left behind in the backup destination: %s", d.Name())
		}
	}
}

func TestBackupRestartsTheFetcherItStopped(t *testing.T) {
	t.Run("the outbox copy fails", func(t *testing.T) {
		e := setup(t)
		e.set("fetcher_running", true)
		write(t, filepath.Join(e.state, "outbox_copy"), "fail", 0o644)
		out, code := e.run("backup.sh", nil, e.backups)
		calls := e.calls()
		if code != 1 || !strings.Contains(out, "copying the fetcher outbox failed") {
			t.Fatalf("exit %d, want the copy failure (1): %s", code, out)
		}
		if count(calls, "stop fetcher") != 1 || count(calls, "start fetcher") != 1 || !e.has("fetcher_running") {
			t.Errorf("fetcher not started again: %v", calls)
		}
		e.noBackupLeft()
	})

	t.Run("a fetcher that was stopped stays stopped", func(t *testing.T) {
		e := setup(t)
		write(t, filepath.Join(e.state, "outbox_copy"), "fail", 0o644)
		out, code := e.run("backup.sh", nil, e.backups)
		calls := e.calls()
		if code != 1 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if count(calls, "stop fetcher")+count(calls, "start fetcher") != 0 || e.has("fetcher_running") {
			t.Errorf("a stopped fetcher was touched: %v", calls)
		}
		e.noBackupLeft()
	})

	t.Run("the backup is interrupted while it copies the outbox", func(t *testing.T) {
		e := setup(t)
		e.set("fetcher_running", true)
		write(t, filepath.Join(e.state, "outbox_copy"), "hang", 0o644)
		cmd := e.command("backup.sh", nil, e.backups)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		must(t, cmd.Start())
		deadline := time.Now().Add(20 * time.Second)
		for !e.has("hanging") {
			if time.Now().After(deadline) {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatalf("the outbox copy did not start: %s", out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		// As a terminal's Ctrl-C or a service manager's stop would: the
		// whole process group.
		must(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM))
		err := cmd.Wait()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 143 {
			t.Fatalf("exit %v, want 143: %s", err, out.String())
		}
		calls := e.calls()
		if count(calls, "start fetcher") != 1 || !e.has("fetcher_running") {
			t.Errorf("fetcher not started again after the interrupt: %v\n%s", calls, out.String())
		}
		e.noBackupLeft()
	})

	t.Run("a successful backup starts it again once", func(t *testing.T) {
		e := setup(t)
		e.set("fetcher_running", true)
		out, code := e.run("backup.sh", []string{"KARTA_BACKUP_MAX_RATE=8M"}, e.backups)
		calls := e.calls()
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if count(calls, "stop fetcher") != 1 || count(calls, "start fetcher") != 1 || !e.has("fetcher_running") {
			t.Errorf("fetcher calls: %v", calls)
		}
		if count(calls, "pg_basebackup rate=8M") != 1 {
			t.Errorf("the read-rate limit did not reach pg_basebackup: %v", calls)
		}
		if b, _ := filepath.Glob(filepath.Join(e.backups, "karta-*", "online.tar")); len(b) != 1 {
			t.Errorf("no outbox in the backup: %v", b)
		}
	})

	t.Run("an invalid read-rate limit is refused before anything runs", func(t *testing.T) {
		e := setup(t)
		e.set("fetcher_running", true)
		out, code := e.run("backup.sh", []string{"KARTA_BACKUP_MAX_RATE=8M --foo"}, e.backups)
		calls := e.calls()
		if code != 2 || count(calls, "stop fetcher")+count(calls, "pg_basebackup") != 0 {
			t.Fatalf("exit %d, calls %v: %s", code, calls, out)
		}
	})
}

func TestRestorePreflightChangesNothingWhenItRefuses(t *testing.T) {
	changed := func(calls []string) []string {
		var bad []string
		for _, c := range calls {
			for _, p := range []string{"extract database", "restore outbox", "empty outbox", "volume rm", "compose up", "compose down", "stop fetcher"} {
				if strings.HasPrefix(c, p) {
					bad = append(bad, c)
				}
			}
		}
		return bad
	}
	cases := []struct {
		name       string
		withOutbox bool   // the backup has online.tar
		db         string // what the database volume holds ("": empty)
		outboxFile bool
		want       []string
	}{
		{name: "the database volume is lost but the outbox holds files", withOutbox: true, outboxFile: true,
			want: []string{"the outbox " /* path */, "holds files; nothing was changed"}},
		{name: "the database volume holds a cluster", withOutbox: true, db: "cluster",
			want: []string{"holds a database; nothing was changed"}},
		{name: "the database volume holds a partial extraction without PG_VERSION", withOutbox: true, db: "partial",
			want: []string{"holds files but no database (an interrupted restore or other data); nothing was changed"}},
		{name: "both hold data", withOutbox: true, db: "cluster", outboxFile: true,
			want: []string{"holds a database; the outbox", "holds files; nothing was changed"}},
		{name: "the backup has no outbox and the destination outbox holds files", outboxFile: true,
			want: []string{"holds files (fetcher state or deliveries the backup would not replace); nothing was changed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			b := e.backup(c.withOutbox)
			e.database(c.db)
			e.set("fetcher_running", true)
			e.set("publisher_running", true)
			if c.outboxFile {
				write(t, filepath.Join(e.outbox, "stale-state"), "stale\n", 0o644)
			}
			out, code := e.run("restore.sh", nil, b)
			calls := e.calls()
			if code == 0 {
				t.Fatalf("the restore was not refused: %s", out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if bad := changed(calls); len(bad) > 0 {
				t.Errorf("the refused restore changed something: %v", bad)
			}
			if !e.has("fetcher_running") || !e.has("publisher_running") {
				t.Error("the refused restore stopped services")
			}
			if c.outboxFile {
				if _, err := os.Stat(filepath.Join(e.outbox, "stale-state")); err != nil {
					t.Errorf("the outbox was changed: %v", err)
				}
			}
			if got := e.databaseContents(); got != c.db {
				t.Errorf("the database volume was changed: %q, was %q", got, c.db)
			}
		})
	}
}

func TestRestoreReplace(t *testing.T) {
	t.Run("a backup without an outbox empties the destination outbox", func(t *testing.T) {
		e := setup(t)
		b := e.backup(false)
		write(t, filepath.Join(e.outbox, "stale-state"), "stale\n", 0o644)
		out, code := e.run("restore.sh", nil, b, "--replace")
		calls := e.calls()
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if entries, _ := os.ReadDir(e.outbox); len(entries) != 0 {
			t.Errorf("stale outbox entries survived: %v", entries)
		}
		if count(calls, "empty outbox") != 1 || count(calls, "extract database") != 1 {
			t.Errorf("calls: %v", calls)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "restore-check") && strings.Contains(c, "--fetcher-outbox") {
				t.Errorf("restore-check was given an outbox the backup does not have: %s", c)
			}
		}
	})

	t.Run("a backup with an outbox replaces the destination outbox and database", func(t *testing.T) {
		e := setup(t)
		b := e.backup(true)
		e.database("cluster")
		write(t, filepath.Join(e.outbox, "stale-state"), "stale\n", 0o644)
		out, code := e.run("restore.sh", nil, b, "--replace")
		calls := e.calls()
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if _, err := os.Stat(filepath.Join(e.outbox, "stale-state")); err == nil {
			t.Error("a stale outbox entry survived")
		}
		if got, err := os.ReadFile(filepath.Join(e.outbox, "backed-up-delivery")); err != nil || string(got) != "from the backup\n" {
			t.Errorf("the outbox was not restored: %q %v", got, err)
		}
		if count(calls, "volume rm pgdata-vol") != 1 || count(calls, "extract database") != 1 || count(calls, "restore outbox") != 1 {
			t.Errorf("calls: %v", calls)
		}
	})

	// The documented case: the database volume was lost while the API,
	// publisher and fetcher kept running. Everything stops before either
	// destination changes, and the fetcher stays stopped.
	t.Run("a lost database with a surviving outbox and running services", func(t *testing.T) {
		e := setup(t)
		b := e.backup(true)
		e.set("fetcher_running", true)
		e.set("publisher_running", true)
		write(t, filepath.Join(e.outbox, "stale-state"), "stale\n", 0o644)
		out, code := e.run("restore.sh", nil, b, "--replace")
		calls := e.calls()
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		down, extract, outbox := index(calls, "compose down"), index(calls, "extract database"), index(calls, "restore outbox")
		if down < 0 || extract < down || outbox < down {
			t.Errorf("the stack did not stop before the restore changed anything: %v", calls)
		}
		if count(calls, "volume rm") != 0 {
			t.Errorf("an empty database volume was deleted: %v", calls)
		}
		if e.has("fetcher_running") || count(calls, "start fetcher") != 0 {
			t.Errorf("the fetcher was started: %v", calls)
		}
		if !e.has("publisher_running") || !strings.Contains(out, "the fetcher and the monitoring profile are stopped") {
			t.Errorf("serving was not started again, or the stopped fetcher not reported: %s", out)
		}
	})

	t.Run("a partial database volume is deleted before the base backup is extracted", func(t *testing.T) {
		e := setup(t)
		b := e.backup(true)
		e.database("partial")
		out, code := e.run("restore.sh", nil, b, "--replace")
		calls := e.calls()
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		rm, extract := index(calls, "volume rm pgdata-vol"), index(calls, "extract database")
		if rm < 0 || extract < rm || !strings.Contains(out, "deleting the current database volume pgdata-vol (partial)") {
			t.Errorf("the partial volume was not deleted first: %v\n%s", calls, out)
		}
	})

	t.Run("an outbox the fetcher writes while the stack stops is refused without --replace", func(t *testing.T) {
		e := setup(t)
		b := e.backup(false)
		e.set("fetcher_running", true)
		e.set("write_on_down", true)
		out, code := e.run("restore.sh", nil, b)
		calls := e.calls()
		if code == 0 || !strings.Contains(out, "the stack was stopped, but no data was changed") {
			t.Fatalf("exit %d: %s", code, out)
		}
		if index(calls, "extract database") >= 0 || index(calls, "empty outbox") >= 0 {
			t.Errorf("data was changed: %v", calls)
		}
		if _, err := os.Stat(filepath.Join(e.outbox, "late-delivery")); err != nil {
			t.Errorf("the late delivery was removed: %v", err)
		}
	})

	t.Run("empty destinations need no --replace", func(t *testing.T) {
		e := setup(t)
		b := e.backup(true)
		out, code := e.run("restore.sh", nil, b)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		calls := e.calls()
		if count(calls, "extract database") != 1 || count(calls, "restore outbox") != 1 || count(calls, "volume rm") != 0 ||
			index(calls, "compose down") < 0 || index(calls, "compose down") > index(calls, "extract database") {
			t.Errorf("calls: %v", calls)
		}
	})
}

func index(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(content), mode))
}
