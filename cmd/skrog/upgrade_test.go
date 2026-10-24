package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/upgrade"
)

// upgradeRun is one drive of upgradeSteps.run with the machine faked out: what
// was printed, what was asked, and what was applied, in order.
type upgradeRun struct {
	code   int
	out    string
	events []string
}

func driveUpgrade(t *testing.T, rep upgrade.Report, exe string, apply, yes, force bool) upgradeRun {
	t.Helper()
	var out strings.Builder
	var r upgradeRun
	r.code = upgradeSteps{
		apply: apply, yes: yes, force: force, out: &out,
		// The real ownership check, against a path the test controls.
		ownerOf: func() (upgrade.Owner, bool) { return upgrade.OwnedBy(exe) },
		confirm: func(string) bool {
			r.events = append(r.events, "confirm")
			return true
		},
		applyPlan: func(p []upgrade.Action) int {
			for _, a := range p {
				r.events = append(r.events, "apply "+a.Stream)
			}
			return exitOK
		},
		applyApp: func(v string) int {
			r.events = append(r.events, "apply app "+v)
			return exitOK
		},
	}.run(rep)
	r.out = out.String()
	return r
}

// wingetExe is a skrog.exe inside a winget portable package directory, with
// LOCALAPPDATA pointed at a temp dir so OwnedBy finds that root.
func wingetExe(t *testing.T) string {
	t.Helper()
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	dir := filepath.Join(local, "Microsoft", "WinGet", "Packages", "wslkit.skrog_Microsoft.Winget.Source_8wekyb3d8bbwe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "skrog.exe")
}

// plainExe is a zip unpacked by hand: owned by nobody.
func plainExe(t *testing.T) string {
	t.Helper()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	return filepath.Join(t.TempDir(), "Programs", "skrog", "skrog.exe")
}

func behindReport(engineBehind bool) upgrade.Report {
	engine := upgrade.Stream{Name: "engine", Current: "29.9.1", Latest: "29.9.1", Status: upgrade.StatusCurrent}
	if engineBehind {
		engine = upgrade.Stream{Name: "engine", Current: "29.8.0", Latest: "29.9.1", Status: upgrade.StatusAvailable}
	}
	return upgrade.Report{Streams: []upgrade.Stream{
		{Name: "app", Current: "0.8.0", Latest: "0.10.0", Status: upgrade.StatusAvailable},
		engine,
		{Name: "cli", Current: "29.9.1", Latest: "29.9.1", Status: upgrade.StatusCurrent},
	}}
}

// #545: under winget, --apply used to list the app step, upgrade the engine,
// and only then refuse the app and exit non-zero. The plan the user confirms
// must leave the app out, name the manager's command, and the run must exit 0
// having done exactly what it showed.
func TestUpgradeApplyUnderWingetDecidesOwnershipBeforeThePlan(t *testing.T) {
	r := driveUpgrade(t, behindReport(true), wingetExe(t), true, false, false)

	if r.code != exitOK {
		t.Errorf("exit %d, want %d\n%s", r.code, exitOK, r.out)
	}
	if got, want := strings.Join(r.events, ", "), "confirm, apply engine"; got != want {
		t.Errorf("events = %q, want %q (the app step must never run)", got, want)
	}
	shown, _, _ := strings.Cut(r.out, "will not run:")
	if strings.Contains(shown, "app") {
		t.Errorf("the plan to confirm lists the app step:\n%s", shown)
	}
	if !strings.Contains(r.out, "managed by winget — run: winget upgrade wslkit.skrog") {
		t.Errorf("the plan does not name winget's command:\n%s", r.out)
	}
}

// Nothing to apply except the app, and the app is the manager's: say so and
// exit 0 without asking a question whose only answer changes nothing.
func TestUpgradeApplyUnderWingetWithOnlyTheAppBehind(t *testing.T) {
	r := driveUpgrade(t, behindReport(false), wingetExe(t), true, false, false)

	if r.code != exitOK {
		t.Errorf("exit %d, want %d\n%s", r.code, exitOK, r.out)
	}
	if len(r.events) != 0 {
		t.Errorf("events = %v, want none: no prompt, nothing applied", r.events)
	}
	if !strings.Contains(r.out, "winget upgrade wslkit.skrog") {
		t.Errorf("output does not name winget's command:\n%s", r.out)
	}
}

// --yes shows no plan, so the manager's command is printed after the run.
func TestUpgradeApplyYesUnderWingetSaysWhatToRunAfter(t *testing.T) {
	r := driveUpgrade(t, behindReport(true), wingetExe(t), true, true, false)

	if r.code != exitOK {
		t.Errorf("exit %d, want %d\n%s", r.code, exitOK, r.out)
	}
	if got, want := strings.Join(r.events, ", "), "apply engine"; got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
	if !strings.Contains(r.out, "upgrade it with:  winget upgrade wslkit.skrog") {
		t.Errorf("output does not name winget's command:\n%s", r.out)
	}
}

// --force keeps the old behaviour: the app is replaced anyway.
func TestUpgradeApplyForceUnderWingetReplacesTheApp(t *testing.T) {
	r := driveUpgrade(t, behindReport(true), wingetExe(t), true, false, true)

	if got, want := strings.Join(r.events, ", "), "confirm, apply engine, apply app 0.10.0"; got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
	if !strings.Contains(r.out, "replaces skrog.exe") {
		t.Errorf("the plan does not show the app step:\n%s", r.out)
	}
}

// An unmanaged install is unchanged by #545: app last, after the plan.
func TestUpgradeApplyUnmanagedReplacesTheAppLast(t *testing.T) {
	r := driveUpgrade(t, behindReport(true), plainExe(t), true, false, false)

	if got, want := strings.Join(r.events, ", "), "confirm, apply engine, apply app 0.10.0"; got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
	if strings.Contains(r.out, "managed by") {
		t.Errorf("an unmanaged install is reported as managed:\n%s", r.out)
	}
}
