package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/config"
)

// A failed registration must not be recorded as "on": that record, over a
// machine with no entry, is the drift #515 was, written down.
//
// Safe to run on a developer machine: the test binary has no skrogw.exe
// beside it, so autostart.Enable refuses before it opens the Run key, and the
// user's real logon entry is never touched.
func TestApplyAutostartDoesNotRecordAFailedEnable(t *testing.T) {
	// The precondition is checked BEFORE the call, not inferred from its
	// result: a skip on "it succeeded" would also hide a mutant that stops
	// returning the error -- which is how this test first passed vacuously.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "skrogw.exe")); err == nil {
		t.Skip("skrogw.exe is beside the test binary, so Enable would write the real Run key")
	}

	dir := t.TempDir()
	if err := applyAutostart(dir, true); err == nil {
		t.Error("applyAutostart reported success with no skrogw.exe to register")
	}
	if v, _ := config.Get(dir, config.KeyAutostart); v != "" {
		t.Errorf("recorded %q after a failed enable, want nothing recorded", v)
	}
}

// install records the choice even when registration fails, so doctor can say
// the asked-for autostart is missing.
func TestRecordAutostartStoresTheChoice(t *testing.T) {
	dir := t.TempDir()
	for _, on := range []bool{true, false} {
		if err := recordAutostart(dir, on); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{true: "on", false: "off"}[on]
		if v, _ := config.Get(dir, config.KeyAutostart); v != want {
			t.Errorf("recorded %q, want %q", v, want)
		}
	}
}

func TestAutostartStateEffectiveAndDescribe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		s          autostartState
		effective  string
		describeIn string
	}{
		{"on and registered", autostartState{Recorded: "on", Registered: true}, "on", "on"},
		{"off and not registered", autostartState{Recorded: "off"}, "off", "off"},
		{"on but missing", autostartState{Recorded: "on"}, "on", "no logon entry is registered"},
		{"off but registered", autostartState{Recorded: "off", Registered: true}, "off", "a logon entry is registered"},
		{"never recorded, registered", autostartState{Registered: true}, "on", "not recorded"},
		{"never recorded, missing", autostartState{}, "off", "not recorded"},
	} {
		if got := tc.s.Effective(); got != tc.effective {
			t.Errorf("%s: Effective() = %q, want %q", tc.name, got, tc.effective)
		}
		if got := tc.s.Describe(); !strings.Contains(got, tc.describeIn) {
			t.Errorf("%s: Describe() = %q, want it to contain %q", tc.name, got, tc.describeIn)
		}
	}
}
