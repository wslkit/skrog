package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/version"
)

func autostartFacts(installed, registered bool, desired, intent string) Facts {
	f := Facts{
		Report:          &version.Report{},
		Desired:         desired,
		Session0:        Session0Info{AutostartConfigured: registered},
		AutostartIntent: intent,
	}
	f.Report.Engine.Installed = installed
	return f
}

// The case #515 was found by, on an install that never recorded a choice:
// installed, set to run, and nothing registered to start it after a reboot.
// doctor used to report that only as a session0 skip that said it did not
// matter on a desktop.
func TestAutostartMissingWhileDesiredRunningWarns(t *testing.T) {
	r := checkAutostart().Run(autostartFacts(true, false, "running", ""))
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: %s", r.Status, r.Summary)
	}
	if !strings.Contains(r.Summary, "will not start at logon") {
		t.Errorf("summary should say what will happen: %q", r.Summary)
	}
	// With no recorded choice the remedy offers both ways out, including the
	// one that makes the warning go away for someone who meant it.
	for _, want := range []string{"config set autostart on", "config set autostart off"} {
		if !strings.Contains(r.Remedy, want) {
			t.Errorf("remedy should offer %q: %q", want, r.Remedy)
		}
	}
}

// An empty desired state is what an install that never ran `skrog stop`
// reads as, and it means running.
func TestAutostartMissingWithNoDesiredStateWarns(t *testing.T) {
	if r := checkAutostart().Run(autostartFacts(true, false, "", "")); r.Status != Warn {
		t.Errorf("status = %v with no desired-state file, want Warn", r.Status)
	}
}

// Recorded "on" and missing is the drift itself, and says so -- and it is
// warned about even with the engine stopped, because the user's own choice
// is what is being contradicted.
func TestAutostartAskedForButMissingWarns(t *testing.T) {
	for _, desired := range []string{"running", "stopped"} {
		r := checkAutostart().Run(autostartFacts(true, false, desired, "on"))
		if r.Status != Warn {
			t.Errorf("desired %s: status = %v, want Warn", desired, r.Status)
			continue
		}
		if !strings.Contains(r.Summary, "autostart is on") || !strings.Contains(r.Remedy, "--fix") {
			t.Errorf("desired %s: should name the contradiction and the fix: %q / %q", desired, r.Summary, r.Remedy)
		}
	}
}

func TestAutostartQuietCases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		installed  bool
		registered bool
		desired    string
		intent     string
		want       Status
	}{
		{"registered, never recorded", true, true, "running", "", OK},
		{"registered and on", true, true, "running", "on", OK},
		{"off by choice", true, false, "running", "off", OK},
		{"stopped by request, never recorded", true, false, "stopped", "", OK},
		{"no engine", false, false, "running", "on", Skip},
	} {
		r := checkAutostart().Run(autostartFacts(tc.installed, tc.registered, tc.desired, tc.intent))
		if r.Status != tc.want {
			t.Errorf("%s: status = %v, want %v (%s)", tc.name, r.Status, tc.want, r.Summary)
		}
	}
}

// --fix repairs only what the user's recorded choice asks for. Re-registering
// an entry nobody said they wanted would override a choice they may have made.
func TestAutostartFixOnlyForRecordedIntent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		intent     string
		wantCall   bool
	}{
		{"on and missing", false, "on", true},
		{"never recorded and missing", false, "", false},
		{"off by choice", false, "off", false},
		{"on and registered", true, "on", false},
	} {
		called := false
		f := autostartFacts(true, tc.registered, "running", tc.intent)
		f.enableAutostart = func() error { called = true; return nil }
		done, err := checkAutostart().Fix(context.Background(), f)
		if err != nil {
			t.Errorf("%s: fix error: %v", tc.name, err)
		}
		if called != tc.wantCall {
			t.Errorf("%s: enable called = %v, want %v", tc.name, called, tc.wantCall)
		}
		if tc.wantCall && done == "" {
			t.Errorf("%s: a repair that ran should say what it did", tc.name)
		}
		if !tc.wantCall && done != "" {
			t.Errorf("%s: reported %q without repairing anything", tc.name, done)
		}
	}
}

func TestAutostartFixReportsFailure(t *testing.T) {
	f := autostartFacts(true, false, "running", "on")
	f.enableAutostart = func() error { return errors.New("no skrogw.exe") }
	if _, err := checkAutostart().Fix(context.Background(), f); err == nil {
		t.Error("a failed re-registration was reported as success")
	}
	f.enableAutostart = nil
	if _, err := checkAutostart().Fix(context.Background(), f); err == nil {
		t.Error("with no way to register, the fix should say so rather than claim success")
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
