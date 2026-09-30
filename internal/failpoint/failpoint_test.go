package failpoint

import "testing"

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
