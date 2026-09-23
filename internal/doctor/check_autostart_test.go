package doctor

import (
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/version"
)

func autostartFacts(installed, registered bool, desired string) Facts {
	f := Facts{
		Report:   &version.Report{},
		Desired:  desired,
		Session0: Session0Info{AutostartConfigured: registered},
	}
	f.Report.Engine.Installed = installed
	return f
}

// The case #515 was found by: installed, set to run, and nothing registered to
// start it after a reboot. doctor used to report that only as a session0
// skip that said it did not matter on a desktop.
func TestAutostartMissingWhileDesiredRunningWarns(t *testing.T) {
	r := checkAutostart().Run(autostartFacts(true, false, "running"))
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: %s", r.Status, r.Summary)
	}
	if !strings.Contains(r.Summary, "will not start at logon") {
		t.Errorf("summary should say what will happen: %q", r.Summary)
	}
	if !strings.Contains(r.Remedy, "skrog autostart enable") {
		t.Errorf("remedy should name the command: %q", r.Remedy)
	}
}

// An empty desired state is what an install that never ran `skrog stop`
// reads as, and it means running.
func TestAutostartMissingWithNoDesiredStateWarns(t *testing.T) {
	if r := checkAutostart().Run(autostartFacts(true, false, "")); r.Status != Warn {
		t.Errorf("status = %v with no desired-state file, want Warn", r.Status)
	}
}

func TestAutostartQuietCases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		installed  bool
		registered bool
		desired    string
		want       Status
	}{
		{"registered", true, true, "running", OK},
		{"stopped by request", true, false, "stopped", OK},
		{"no engine", false, false, "running", Skip},
	} {
		if r := checkAutostart().Run(autostartFacts(tc.installed, tc.registered, tc.desired)); r.Status != tc.want {
			t.Errorf("%s: status = %v, want %v (%s)", tc.name, r.Status, tc.want, r.Summary)
		}
	}
}

// No fix: nothing records whether autostart was turned off on purpose, so
// re-registering it is not a safe remedy for --fix to apply.
func TestAutostartCheckHasNoFix(t *testing.T) {
	if checkAutostart().Fix != nil {
		t.Error("the autostart check must not auto-fix: the missing entry may be deliberate")
	}
}

func TestAutostartCheckIsRegistered(t *testing.T) {
	for _, c := range Registry() {
		if c.Name == "autostart" {
			return
		}
	}
	t.Error("autostart is not in the doctor registry")
}
