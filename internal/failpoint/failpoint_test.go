package failpoint

import (
	"testing"
	"time"
)

func TestHitOnlyEnabledFailpoints(t *testing.T) {
	saved, savedExit := enabled, exit
	defer func() { enabled, exit = saved, savedExit }()
	var code int
	exit = func(c int) { code = c }

	enabled = parse("")
	Hit("build.after_rename")
	if code != 0 {
		t.Fatal("disabled failpoint exited")
	}
	enabled = parse(" build.after_rename , bogus")
	Hit("activate.before_commit")
	if code != 0 {
		t.Fatal("other failpoint exited")
	}
	Hit("build.after_rename")
	if code != ExitCode {
		t.Fatalf("exit code %d", code)
	}
	on, unknown := Enabled()
	if len(on) != 1 || on[0] != "build.after_rename" || len(unknown) != 1 || unknown[0] != "bogus" {
		t.Fatalf("enabled %v unknown %v", on, unknown)
	}
}

func TestClock(t *testing.T) {
	t.Setenv(ClockVariable, "")
	if now, err := Clock(); now != nil || err != nil {
		t.Fatalf("unset: clock set %v, %v", now != nil, err)
	}
	t.Setenv(ClockVariable, "2026-10-04T05:00:00Z")
	now, err := Clock()
	if err != nil || now == nil || !now().Equal(time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("set: %v", err)
	}
	t.Setenv(ClockVariable, "yesterday")
	if _, err := Clock(); err == nil {
		t.Fatal("an unparsable clock was accepted")
	}
}
