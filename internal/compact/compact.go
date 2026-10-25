// Package compact reclaims disk space the engine's virtual disk is holding but
// not using (#64).
//
// The mechanics and their gotchas live in internal/vhdx. This package is the
// orchestration, and the order matters:
//
//	fstrim (needs the engine up)  ->  stop it  ->  wait for WSL to let the disk
//	go  ->  CompactVirtualDisk  ->  optionally start it again
//
// Two refusals are deliberate. Skrog stops its own distro and nothing else,
// ever -- so when another distro is running (Docker Desktop's count) the disk
// cannot be released and this refuses, naming who is holding it, instead of
// running `wsl --shutdown` and killing someone's containers. And when nothing
// is running but the wait still ran out, it says that instead, because
// "stop your distros" is useless advice to someone who already has.
package compact

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/vhdx"
	"github.com/wslkit/skrog/internal/wsl"
)

// ErrHeldByOthers reports that other distros are running, so WSL will not
// release the disk. Holders names them.
type ErrHeldByOthers struct{ Holders []string }

func (e *ErrHeldByOthers) Error() string {
	return "the WSL utility VM keeps every disk open while any distro runs, and these are running: " +
		strings.Join(e.Holders, ", ")
}

// ErrStillHeld reports that the disk was not released within the wait, with
// nothing else running -- the utility VM was still winding down.
type ErrStillHeld struct{ Waited time.Duration }

func (e *ErrStillHeld) Error() string {
	return fmt.Sprintf("the WSL utility VM still had the disk after %s; it releases it about "+
		"a minute after the last distro stops", e.Waited.Round(time.Second))
}

// Distros is the slice of wsl.WSL this package needs.
type Distros interface {
	Exec(ctx context.Context, distro, user string, args ...string) (string, error)
	Terminate(ctx context.Context, distro string) error
	List(ctx context.Context) ([]wsl.Distro, error)
}

// Disk is the virtual-disk half, injected so the flow is testable without a
// real .vhdx.
type Disk interface {
	// Free reports whether the disk can be opened for compaction now.
	Free(path string) bool
	// Compact shrinks it.
	Compact(path string) (vhdx.Result, error)
	// SizeOnDisk is the file's footprint on the volume.
	SizeOnDisk(path string) (uint64, error)
}

// RealDisk is the production Disk, backed by internal/vhdx.
type RealDisk struct{}

func (RealDisk) Free(path string) bool                    { return vhdx.Free(path) }
func (RealDisk) Compact(path string) (vhdx.Result, error) { return vhdx.Compact(path) }
func (RealDisk) SizeOnDisk(path string) (uint64, error)   { return vhdx.SizeOnDisk(path) }

// Options is one compaction request.
type Options struct {
	// Distro is the engine distro to compact; DiskPath is its .vhdx.
	Distro   string
	DiskPath string
	// NoTrim skips the in-guest fstrim. Only useful if you just trimmed:
	// without a trim, compaction has almost nothing to reclaim.
	NoTrim bool
	// DryRun reports the steps and changes nothing.
	DryRun bool
	// Restart starts the engine distro again afterwards.
	Restart bool
	// Wait is how long to wait for WSL to release the disk. Zero uses
	// DefaultWait, which outlasts WSL's 60s vmIdleTimeout.
	Wait time.Duration
	// Poll is the interval between release probes; zero uses 2s.
	Poll time.Duration
}

// DefaultWait outlasts WSL's default vmIdleTimeout (60s): the utility VM was
// measured releasing disks at ~66s after the last distro stopped.
const DefaultWait = 90 * time.Second

func (o Options) wait() time.Duration {
	if o.Wait <= 0 {
		return DefaultWait
	}
	return o.Wait
}

func (o Options) poll() time.Duration {
	if o.Poll <= 0 {
		return 2 * time.Second
	}
	return o.Poll
}

// Report is the outcome, shaped for both the human and --json output.
type Report struct {
	Distro string `json:"distro"`
	Path   string `json:"path"`
	// Trimmed is whether fstrim ran; OfferedBytes is the number fstrim printed.
	// It is NOT space reclaimed -- fstrim reports the free extent of the whole
	// virtual disk, which for a 1 TB default disk is off by three orders of
	// magnitude. Named "offered" so no caller mistakes it for a result.
	Trimmed      bool   `json:"trimmed"`
	OfferedBytes uint64 `json:"offeredBytes,omitempty"`
	// BeforeBytes/AfterBytes are the file's size on disk; ReclaimedBytes is
	// the difference, and the only honest measure of what happened.
	BeforeBytes    uint64 `json:"beforeBytes"`
	AfterBytes     uint64 `json:"afterBytes"`
	ReclaimedBytes uint64 `json:"reclaimedBytes"`
	// WaitedSeconds is how long WSL took to release the disk.
	WaitedSeconds float64 `json:"waitedSeconds"`
	Restarted     bool    `json:"restarted"`
	DryRun        bool    `json:"dryRun"`
	// Steps is the human-readable trace, also what --dry-run prints.
	Steps []string `json:"steps,omitempty"`
	// Holders are the other running distros keeping WSL from releasing the
	// disk. Non-empty means a real run would refuse.
	Holders []string `json:"held,omitempty"`
}

// Runner performs a compaction.
type Runner struct {
	WSL  Distros
	Disk Disk
	// Stop is called to stop the engine before the distro is terminated
	// (dockerd first, so it is not killed mid-write). Optional.
	Stop func(ctx context.Context) error
	// Start is called after a successful compaction when Options.Restart is
	// set. Optional.
	Start func(ctx context.Context) error
}

// Run executes the compaction. A refusal (*ErrHeldByOthers, *ErrStillHeld) is
// returned with the partial report, so a caller can print what did happen.
func (r *Runner) Run(ctx context.Context, opts Options) (Report, error) {
	rep := Report{Distro: opts.Distro, Path: opts.DiskPath, DryRun: opts.DryRun}
	if opts.Distro == "" || opts.DiskPath == "" {
		return rep, errors.New("compact: distro and disk path are required")
	}

	before, err := r.Disk.SizeOnDisk(opts.DiskPath)
	if err != nil {
		return rep, err
	}
	rep.BeforeBytes = before

	holders, err := otherRunning(ctx, r.WSL, opts.Distro)
	if err != nil {
		return rep, err
	}
	rep.Holders = holders

	// A dry run answers "what would happen", and "it would refuse, because
	// these are running" is that answer -- so it reports rather than refuses.
	if opts.DryRun {
		rep.Steps = []string{
			"fstrim / inside " + opts.Distro + " (skipped: --no-trim)",
			"stop the engine and terminate " + opts.Distro,
			fmt.Sprintf("wait up to %s for WSL to release %s", opts.wait(), filepath.Base(opts.DiskPath)),
			"compact the disk",
		}
		if !opts.NoTrim {
			rep.Steps[0] = "fstrim / inside " + opts.Distro
		}
		if opts.Restart {
			rep.Steps = append(rep.Steps, "start the engine again")
		}
		rep.AfterBytes = before
		return rep, nil
	}

	// Refuse when someone else is holding the VM: no amount of waiting helps,
	// and stopping their distros is not Skrog's call.
	if len(holders) > 0 {
		return rep, &ErrHeldByOthers{Holders: holders}
	}

	// 1. Trim: the guest tells the disk what it no longer uses. Without this,
	//    compaction has nothing to reclaim.
	if !opts.NoTrim {
		// By path: WSL 3.0's `wsl --exec` does not find /sbin tools by name
		// (#560). The rootfs is ours, so the path is fixed.
		out, err := r.WSL.Exec(ctx, opts.Distro, "root", "/sbin/fstrim", "-v", "/")
		if err != nil {
			return rep, fmt.Errorf("compact: fstrim in %s: %w: %s", opts.Distro, err, strings.TrimSpace(out))
		}
		rep.Trimmed = true
		rep.OfferedBytes = parseFstrimBytes(out)
		rep.Steps = append(rep.Steps, "trimmed the guest filesystem")
	}

	// 2. Stop the engine, then the distro. dockerd first so it is not killed
	//    mid-write.
	if r.Stop != nil {
		if err := r.Stop(ctx); err != nil {
			return rep, fmt.Errorf("compact: stopping the engine: %w", err)
		}
	}
	if err := r.WSL.Terminate(ctx, opts.Distro); err != nil {
		return rep, fmt.Errorf("compact: terminating %s: %w", opts.Distro, err)
	}
	rep.Steps = append(rep.Steps, "stopped the engine and terminated the distro")

	// 3. Wait for the utility VM to let go.
	waited, ok := r.waitForRelease(ctx, opts)
	rep.WaitedSeconds = waited.Seconds()
	if !ok {
		// Re-check who is running: something may have started while we waited,
		// which is a different (and fixable) situation from a slow wind-down.
		// Cancellation is not a diagnosis. Ctrl-C during the wait used to
		// produce the "still winding down, re-run or raise --wait" advice,
		// which describes a timeout that never occurred (#243).
		if ctx.Err() != nil {
			return rep, fmt.Errorf("compact: interrupted after %s, before the disk was compacted: %w", waited.Round(time.Second), ctx.Err())
		}
		if holders, err := otherRunning(ctx, r.WSL, opts.Distro); err == nil && len(holders) > 0 {
			return rep, &ErrHeldByOthers{Holders: holders}
		}
		return rep, &ErrStillHeld{Waited: waited}
	}
	rep.Steps = append(rep.Steps, fmt.Sprintf("WSL released the disk after %s", waited.Round(time.Second)))

	// 4. Compact.
	res, err := r.Disk.Compact(opts.DiskPath)
	if err != nil {
		return rep, err
	}
	rep.BeforeBytes, rep.AfterBytes = res.Before, res.After
	rep.ReclaimedBytes = res.Reclaimed()
	rep.Steps = append(rep.Steps, "compacted the disk")
	if res.Grew() {
		return rep, fmt.Errorf("compact: the disk grew from %d to %d bytes; something wrote to it "+
			"during the compaction", res.Before, res.After)
	}

	// 5. Optionally bring the engine back.
	if opts.Restart && r.Start != nil {
		if err := r.Start(ctx); err != nil {
			return rep, fmt.Errorf("compact: the disk was compacted, but starting the engine failed: %w", err)
		}
		rep.Restarted = true
		rep.Steps = append(rep.Steps, "started the engine again")
	}
	return rep, nil
}

// waitForRelease polls until the disk can be opened, returning how long it took.
func (r *Runner) waitForRelease(ctx context.Context, opts Options) (time.Duration, bool) {
	start := time.Now()
	deadline := start.Add(opts.wait())
	for {
		if r.Disk.Free(opts.DiskPath) {
			return time.Since(start), true
		}
		if time.Now().After(deadline) {
			return time.Since(start), false
		}
		select {
		case <-ctx.Done():
			// Distinguished from the deadline by the caller: reporting "the
			// utility VM still had the disk after 11s" to someone who pressed
			// Ctrl-C diagnoses a problem that did not happen (#243).
			return time.Since(start), false
		case <-time.After(opts.poll()):
		}
	}
}

// otherRunning returns the running distros that are not ours. Those are what
// keep the utility VM -- and therefore every disk -- alive.
func otherRunning(ctx context.Context, d Distros, mine string) ([]string, error) {
	list, err := d.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("compact: listing distros: %w", err)
	}
	var out []string
	for _, dd := range list {
		if dd.Name != mine && dd.Running() {
			out = append(out, dd.Name)
		}
	}
	return out, nil
}

var fstrimBytes = regexp.MustCompile(`(\d+)\s+bytes`)

// parseFstrimBytes pulls the byte count out of `fstrim -v` output. The number
// is the free extent of the virtual disk, not space reclaimed; see
// Report.OfferedBytes.
func parseFstrimBytes(out string) uint64 {
	m := fstrimBytes.FindStringSubmatch(out)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
