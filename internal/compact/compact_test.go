package compact_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/compact"
	"github.com/wslkit/skrog/internal/vhdx"
	"github.com/wslkit/skrog/internal/wsl"
)

// fakeWSL records what was asked of it and answers from canned state.
type fakeWSL struct {
	distros   []wsl.Distro
	fstrimOut string
	fstrimErr error
	termErr   error
	calls     []string
}

func (f *fakeWSL) Exec(_ context.Context, distro, user string, args ...string) (string, error) {
	f.calls = append(f.calls, "exec:"+distro+":"+strings.Join(args, " "))
	// WSL 3.0's relay does not search /sbin for `wsl --exec`, though /sbin is
	// on the PATH the process gets (#560).
	if args[0] == "fstrim" {
		return "execvpe(fstrim) failed: No such file or directory", errors.New("exit status 1")
	}
	return f.fstrimOut, f.fstrimErr
}

func (f *fakeWSL) Terminate(_ context.Context, distro string) error {
	f.calls = append(f.calls, "terminate:"+distro)
	return f.termErr
}

func (f *fakeWSL) List(_ context.Context) ([]wsl.Distro, error) { return f.distros, nil }

// fakeDisk is held for the first freeAfter compaction attempts, so the wait
// loop is exercised without real time passing.
type fakeDisk struct {
	size      uint64
	after     uint64
	probes    int
	freeAfter int
	compacted int
	err       error
}

// Free answers the way opening the disk does on WSL 3.0: yes, even while the
// utility VM holds it (#560). Nothing should rely on it.
func (d *fakeDisk) Free(string) bool { return true }

func (d *fakeDisk) Compact(path string) (vhdx.Result, error) {
	d.probes++
	if d.probes <= d.freeAfter {
		return vhdx.Result{}, fmt.Errorf("%w: %s", vhdx.ErrInUse, path)
	}
	d.compacted++
	if d.err != nil {
		return vhdx.Result{}, d.err
	}
	return vhdx.Result{Before: d.size, After: d.after}, nil
}

func (d *fakeDisk) SizeOnDisk(string) (uint64, error) { return d.size, nil }

func opts() compact.Options {
	return compact.Options{
		Distro:   "skrog-engine",
		DiskPath: `C:\data\ext4.vhdx`,
		Wait:     time.Second,
		Poll:     time.Millisecond,
	}
}

func TestHappyPathOrdersTrimStopTerminateCompact(t *testing.T) {
	w := &fakeWSL{
		distros:   []wsl.Distro{{Name: "skrog-engine", State: "Running"}, {Name: "Ubuntu", State: "Stopped"}},
		fstrimOut: "/: 1078939029504 bytes were trimmed",
	}
	d := &fakeDisk{size: 14 << 30, after: 9 << 30}
	var stopped, started bool
	r := &compact.Runner{
		WSL:   w,
		Disk:  d,
		Stop:  func(context.Context) error { stopped = true; return nil },
		Start: func(context.Context) error { started = true; return nil },
	}
	o := opts()
	o.Restart = true

	rep, err := r.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stopped {
		t.Error("the engine was not stopped before terminating the distro")
	}
	if !started {
		t.Error("--restart did not start the engine again")
	}
	if got := strings.Join(w.calls, "|"); !strings.Contains(got, "exec:skrog-engine:/sbin/fstrim -v /") ||
		!strings.Contains(got, "terminate:skrog-engine") {
		t.Errorf("calls = %s", got)
	}
	// fstrim's figure must be reported as offered, never as reclaimed.
	if rep.OfferedBytes != 1078939029504 {
		t.Errorf("OfferedBytes = %d", rep.OfferedBytes)
	}
	if rep.ReclaimedBytes != 5<<30 {
		t.Errorf("ReclaimedBytes = %d, want %d", rep.ReclaimedBytes, 5<<30)
	}
	if !rep.Trimmed || !rep.Restarted {
		t.Errorf("report flags: %+v", rep)
	}
}

func TestRefusesWhenAnotherDistroIsRunning(t *testing.T) {
	// The whole point: Skrog never runs `wsl --shutdown`, so it refuses and
	// names who is holding the disk instead of killing someone's containers.
	w := &fakeWSL{distros: []wsl.Distro{
		{Name: "skrog-engine", State: "Running"},
		{Name: "docker-desktop", State: "Running"},
		{Name: "Ubuntu", State: "Running"},
	}}
	d := &fakeDisk{size: 1 << 30}
	r := &compact.Runner{WSL: w, Disk: d}

	_, err := r.Run(context.Background(), opts())
	var held *compact.ErrHeldByOthers
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want *ErrHeldByOthers", err)
	}
	if len(held.Holders) != 2 {
		t.Errorf("Holders = %v, want the two other distros", held.Holders)
	}
	if strings.Contains(strings.Join(held.Holders, ","), "skrog-engine") {
		t.Error("our own distro was counted as a holder")
	}
	// Nothing destructive may have happened.
	if len(w.calls) != 0 || d.compacted != 0 {
		t.Errorf("a refusal still acted: calls=%v compacted=%d", w.calls, d.compacted)
	}
}

func TestReportsStillHeldWhenNothingElseRuns(t *testing.T) {
	// Different message on purpose: telling someone to stop distros they have
	// already stopped is no help.
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 1 << 30, freeAfter: 1 << 30} // never frees
	r := &compact.Runner{WSL: w, Disk: d}

	_, err := r.Run(context.Background(), opts())
	var still *compact.ErrStillHeld
	if !errors.As(err, &still) {
		t.Fatalf("err = %v, want *ErrStillHeld", err)
	}
	if !strings.Contains(err.Error(), "about a minute") {
		t.Errorf("the message should explain the timing: %v", err)
	}
	if d.compacted != 0 {
		t.Error("compacted a disk that was never released")
	}
}

func TestWaitsForReleaseThenCompacts(t *testing.T) {
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 10 << 30, after: 8 << 30, freeAfter: 3}
	r := &compact.Runner{WSL: w, Disk: d}

	rep, err := r.Run(context.Background(), opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.probes < 4 {
		t.Errorf("probes = %d, want the wait loop to have run", d.probes)
	}
	if d.compacted != 1 {
		t.Errorf("compacted = %d, want 1", d.compacted)
	}
	if rep.ReclaimedBytes != 2<<30 {
		t.Errorf("ReclaimedBytes = %d", rep.ReclaimedBytes)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 12 << 30}
	var stopped bool
	r := &compact.Runner{WSL: w, Disk: d, Stop: func(context.Context) error { stopped = true; return nil }}
	o := opts()
	o.DryRun = true
	o.Restart = true

	rep, err := r.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stopped || len(w.calls) != 0 || d.compacted != 0 {
		t.Errorf("--dry-run acted: stopped=%v calls=%v compacted=%d", stopped, w.calls, d.compacted)
	}
	if len(rep.Steps) != 5 {
		t.Errorf("Steps = %v, want the five planned steps", rep.Steps)
	}
	if rep.BeforeBytes != rep.AfterBytes {
		t.Error("--dry-run reported a size change")
	}
}

func TestNoTrimSkipsFstrim(t *testing.T) {
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 5 << 30, after: 5 << 30}
	r := &compact.Runner{WSL: w, Disk: d}
	o := opts()
	o.NoTrim = true

	rep, err := r.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Trimmed {
		t.Error("Trimmed set with --no-trim")
	}
	for _, c := range w.calls {
		if strings.Contains(c, "fstrim") {
			t.Errorf("fstrim ran anyway: %v", w.calls)
		}
	}
}

func TestAGrownDiskIsAFailure(t *testing.T) {
	// If the file ended up bigger, something wrote to the disk mid-compaction
	// and the result cannot be trusted.
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 8 << 30, after: 9 << 30}
	r := &compact.Runner{WSL: w, Disk: d}

	rep, err := r.Run(context.Background(), opts())
	if err == nil {
		t.Fatal("a disk that grew was reported as success")
	}
	if !strings.Contains(err.Error(), "grew") {
		t.Errorf("err = %v", err)
	}
	if rep.ReclaimedBytes != 0 {
		t.Errorf("ReclaimedBytes = %d, want 0", rep.ReclaimedBytes)
	}
}

func TestFstrimFailureStopsBeforeTerminating(t *testing.T) {
	w := &fakeWSL{
		distros:   []wsl.Distro{{Name: "skrog-engine", State: "Running"}},
		fstrimErr: errors.New("exit status 1"),
		fstrimOut: "fstrim: /: FITRIM ioctl failed: Operation not supported",
	}
	d := &fakeDisk{size: 3 << 30}
	r := &compact.Runner{WSL: w, Disk: d}

	if _, err := r.Run(context.Background(), opts()); err == nil {
		t.Fatal("expected the fstrim failure to surface")
	}
	for _, c := range w.calls {
		if strings.HasPrefix(c, "terminate:") {
			t.Error("terminated the distro after fstrim failed")
		}
	}
}

func TestParsesFstrimlessOutput(t *testing.T) {
	// Some fstrim builds say nothing parseable; that must not be an error.
	w := &fakeWSL{
		distros:   []wsl.Distro{{Name: "skrog-engine", State: "Running"}},
		fstrimOut: "",
	}
	d := &fakeDisk{size: 4 << 30, after: 3 << 30}
	r := &compact.Runner{WSL: w, Disk: d}

	rep, err := r.Run(context.Background(), opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.OfferedBytes != 0 {
		t.Errorf("OfferedBytes = %d, want 0", rep.OfferedBytes)
	}
	if !rep.Trimmed {
		t.Error("Trimmed should still be true: fstrim ran and succeeded")
	}
}

func TestDryRunReportsHoldersInsteadOfRefusing(t *testing.T) {
	// "What would happen?" deserves an answer, and the answer is "it would
	// refuse, because these are running" -- not an error.
	w := &fakeWSL{distros: []wsl.Distro{
		{Name: "skrog-engine", State: "Running"},
		{Name: "docker-desktop", State: "Running"},
	}}
	d := &fakeDisk{size: 6 << 30}
	r := &compact.Runner{WSL: w, Disk: d}
	o := opts()
	o.DryRun = true

	rep, err := r.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("a dry run refused: %v", err)
	}
	if len(rep.Holders) != 1 || rep.Holders[0] != "docker-desktop" {
		t.Errorf("Holders = %v, want [docker-desktop]", rep.Holders)
	}
	if len(rep.Steps) == 0 {
		t.Error("a dry run should still print the plan")
	}
}

func TestHoldersAreReportedOnASuccessfulRunToo(t *testing.T) {
	// Nothing else running: Holders must be empty, so a caller can trust it as
	// "would this refuse?" rather than having to interpret an error.
	w := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}}}
	d := &fakeDisk{size: 7 << 30, after: 6 << 30}
	r := &compact.Runner{WSL: w, Disk: d}

	rep, err := r.Run(context.Background(), opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Holders) != 0 {
		t.Errorf("Holders = %v, want empty", rep.Holders)
	}
}
