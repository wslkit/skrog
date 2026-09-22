package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/config"
)

func pruneIdleResult(t *testing.T, idle, every time.Duration) Result {
	t.Helper()
	return checkPruneIdle().Run(Facts{IdleTimeout: idle, PruneEvery: every})
}

// The case the check exists for: a prune interval that resets the idle window
// before it can expire, so the engine never returns its RAM (#496).
func TestPruneShorterThanIdleWarns(t *testing.T) {
	r := pruneIdleResult(t, 30*time.Minute, 10*time.Minute)
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn", r.Status)
	}
	// The message has to name both numbers, or the reader cannot tell which
	// of their two settings to change.
	for _, want := range []string{"10m", "30m", "never idle-stop"} {
		if !strings.Contains(r.Summary, want) {
			t.Errorf("summary should mention %q: %q", want, r.Summary)
		}
	}
	if r.Remedy == "" {
		t.Error("a warning with no remedy is the kind doctor output people learn to skim")
	}
}

// Equal is the same failure: the window is reset exactly as it would expire.
func TestPruneEqualToIdleWarns(t *testing.T) {
	if r := pruneIdleResult(t, 20*time.Minute, 20*time.Minute); r.Status != Warn {
		t.Errorf("status = %v for equal durations, want Warn", r.Status)
	}
}

func TestPruneLongerThanIdleIsOK(t *testing.T) {
	if r := pruneIdleResult(t, 20*time.Minute, 24*time.Hour); r.Status != OK {
		t.Errorf("status = %v, want OK", r.Status)
	}
}

// Both are off by default, and a check that warned on a default machine is a
// check people stop reading.
func TestPruneIdleSkipsWhenEitherIsOff(t *testing.T) {
	if r := pruneIdleResult(t, 0, 10*time.Minute); r.Status != Skip {
		t.Errorf("idle-timeout off: status = %v, want Skip", r.Status)
	}
	if r := pruneIdleResult(t, 30*time.Minute, 0); r.Status != Skip {
		t.Errorf("prune.every off: status = %v, want Skip", r.Status)
	}
	if r := pruneIdleResult(t, 0, 0); r.Status != Skip {
		t.Errorf("both off: status = %v, want Skip", r.Status)
	}
}

// The remedy must clear the timeout by a margin, not by a second: a value that
// only just wins the comparison still races a sweep that runs long.
func TestTheSuggestedIntervalClearsTheTimeoutByAMargin(t *testing.T) {
	idle := 30 * time.Minute
	got, err := time.ParseDuration(suggest(idle))
	if err != nil {
		t.Fatalf("the suggested interval does not parse as a duration: %v", err)
	}
	if got <= idle {
		t.Errorf("suggested %s for an idle timeout of %s", got, idle)
	}
	// And it must be a value `skrog config set prune.every` accepts, which is
	// what makes the remedy copy-pasteable rather than decorative.
	if strings.Contains(suggest(idle), "µ") || strings.Contains(suggest(idle), "ns") {
		t.Errorf("suggested %q is not a value anyone would type", suggest(idle))
	}
}

// Registered, not merely written.
//
// This is the failure checkEmulation had: the check, its helper and its tests
// were all correct and nothing called it from Registry, so `skrog doctor` was
// silent about the thing the check existed to report.
func TestPruneIdleCheckIsRegistered(t *testing.T) {
	for _, c := range Registry() {
		if c.Name == "prune-idle" {
			return
		}
	}
	t.Fatal("checkPruneIdle is not in Registry(), so `skrog doctor` never runs it")
}

// ...and the settings must actually REACH Facts from the config file.
//
// Every test above builds Facts by hand, which proves the check and proves
// nothing about whether `skrog doctor` ever sees a real value. That gap is how
// a correct check with correct tests reports nothing on a real machine -- the
// same shape as checkEmulation, which was registered nowhere while its own
// tests passed.
func TestIdleAndPruneSettingsReachFactsFromConfig(t *testing.T) {
	dir := t.TempDir()
	if err := config.Set(dir, config.KeyIdleTimeout, "30m"); err != nil {
		t.Fatal(err)
	}
	if err := config.Set(dir, config.KeyPruneEvery, "10m"); err != nil {
		t.Fatal(err)
	}

	f := Gather(context.Background(), GatherOptions{StateDir: dir})
	if f.IdleTimeout != 30*time.Minute {
		t.Errorf("IdleTimeout = %v, want 30m; the setting never reached doctor", f.IdleTimeout)
	}
	if f.PruneEvery != 10*time.Minute {
		t.Errorf("PruneEvery = %v, want 10m; the setting never reached doctor", f.PruneEvery)
	}
	// And the check therefore fires on this machine.
	if r := checkPruneIdle().Run(f); r.Status != Warn {
		t.Errorf("status = %v on a machine configured to never idle-stop, want Warn", r.Status)
	}
}

// Durations read the way the settings are written, and the obvious
// implementation is wrong: TrimSuffix("1m30s", "0s") is "1m3".
func TestShortRendersDurationsTheWayPeopleWriteThem(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "10m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "25h"},
		{30 * time.Minute, "30m"},
		{90 * time.Second, "1m30s"},
		{45 * time.Second, "45s"},
	} {
		if got := short(tc.in); got != tc.want {
			t.Errorf("short(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
