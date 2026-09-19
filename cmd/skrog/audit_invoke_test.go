package main

import (
	"os"
	"path/filepath"
	"testing"
)

// runAudit, not splitSubcommand.
//
// TestSplitSubcommand covers the helper, and the helper was never wrong. The
// bug was downstream: with a flag on BOTH sides of the subcommand --
// `skrog audit --state-dir X tail -n 5` -- Parse stopped at the subcommand, so
// `-n 5` was still unparsed when it reached the `len(rest) == 0` guard. The
// command printed its own usage and exited 2, for a spelling that usage line
// advertises.
//
// The e2e suite found this the first time it ran in CI (#11). Nothing at this
// level had ever invoked the command, so the exact same shape as the prune
// guard: a correct helper with a good test, and no test on the path the
// product takes.
func TestAuditAcceptsFlagsOnBothSidesOfTheSubcommand(t *testing.T) {
	dir := t.TempDir()
	// One real record, so tail has something to print and cannot pass by
	// taking the "no log yet" early return.
	line := `{"time":"2026-09-19T04:00:00Z","action":"container-create","image":"alpine"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "audit.log"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"subcommand first", []string{"tail", "--state-dir", dir}},
		{"subcommand first, then its own flag", []string{"tail", "-n", "5", "--state-dir", dir}},
		{"global flag first", []string{"--state-dir", dir, "tail"}},
		{"the older spelling", []string{"--json", "--state-dir", dir, "tail"}},
		// The one that failed. A flag before AND after the subcommand.
		{"flags on both sides", []string{"--state-dir", dir, "tail", "-n", "5"}},
		{"flags on both sides, --since", []string{"--state-dir", dir, "tail", "--since", "1h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAudit(tc.args); got == exitUsage {
				t.Errorf("`skrog audit %v` was rejected as a usage error; "+
					"the usage line advertises this spelling", tc.args)
			}
		})
	}
}

// ...and a genuinely wrong invocation must still be refused, or the fix above
// would have bought correctness by accepting anything.
func TestAuditStillRefusesNonsense(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown subcommand", []string{"--state-dir", dir, "wibble"}},
		{"no subcommand", []string{"--state-dir", dir}},
		{"tail with a stray argument", []string{"--state-dir", dir, "tail", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAudit(tc.args); got != exitUsage {
				t.Errorf("`skrog audit %v` returned %d, want exitUsage (%d)", tc.args, got, exitUsage)
			}
		})
	}
}
