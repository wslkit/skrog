package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/prune"
	"github.com/wslkit/skrog/internal/supervise"
)

// runPrune is `skrog prune` (#145): reclaim disk on the engine — stopped
// containers, unused images, optionally volumes and the BuildKit cache — through
// whatever docker currently targets. Runners die of full disks; this is what a
// post-job step or a scheduled task calls.
func runPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	var (
		all        = fs.Bool("all", false, "remove all unused images, not only dangling (untagged) ones")
		until      = fs.Duration("until", 0, "only remove objects older than this, e.g. 168h (0 = any age)")
		buildCache = fs.Bool("build-cache", false, "also prune the BuildKit cache")
		volumes    = fs.Bool("volumes", false, "also prune unused volumes — they hold data, so off by default")
		asJSON     = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog prune [--all] [--until <age>] [--build-cache] [--volumes] [--json]

Frees disk on the engine: stopped containers first (so their images become
unused), then unused images; --build-cache and --volumes widen it. A failed step
never stops the others. Acts on whatever docker currently targets — the local
engine or the remote selected with `+"`skrog remote use`"+`.

  skrog prune --until 168h               # keep anything from the last week
  skrog prune --all --build-cache        # the full sweep after a job

Exit codes: 0 ok, %d a step failed, %d usage.

flags:
`, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(fs.Args()) != 0 {
		fs.Usage()
		return exitUsage
	}

	ctx, stop := interruptible()
	defer stop()

	if err := (&dockerctx.Manager{}).Available(ctx); err != nil {
		var noCLI *dockerctx.ErrNoDockerCLI
		if errors.As(err, &noCLI) {
			fmt.Fprintf(os.Stderr, "skrog: %v\n  (`skrog cli install` provides one)\n", err)
			return exitError
		}
	}

	res := prune.Run(ctx, prune.DockerRunner{}, prune.Options{
		All: *all, Until: *until, BuildCache: *buildCache, Volumes: *volumes,
	})

	report := pruneJSON{ReclaimedBytes: res.ReclaimedBytes, Failed: res.Failed, Steps: []pruneStepJSON{}}
	for _, st := range res.Steps {
		report.Steps = append(report.Steps, pruneStepJSON{Name: st.Name, ReclaimedBytes: st.ReclaimedBytes, Error: st.Err})
	}
	code := exitOK
	if res.Failed > 0 {
		code = exitError
	}

	if *asJSON {
		if c := emitJSON(report); c != exitOK {
			return c
		}
		return code
	}

	for _, st := range report.Steps {
		if st.Error != "" {
			fmt.Printf("  %-12s FAILED: %s\n", st.Name, st.Error)
			continue
		}
		fmt.Printf("  %-12s reclaimed %s\n", st.Name, humanBytes(st.ReclaimedBytes))
	}
	fmt.Printf("\nreclaimed %s in total\n", humanBytes(res.ReclaimedBytes))
	return code
}

// autoPrune adapts internal/prune for the supervisor's scheduled reclaim
// (#393). It is the same code path `skrog prune` uses, with two differences
// that are the whole safety story:
//
//   - Volumes is never set, and there is no setting that could set it. A human
//     who types `--volumes` has decided to risk data; a timer has not.
//   - Until always carries the policy's guard, which config refuses to let
//     anyone zero.
//
// All is on: an automatic sweep that only removed dangling layers would leave
// the tagged images that actually fill a disk, and the age guard is what keeps
// that honest rather than the dangling-only filter.
func autoPrune(log *slog.Logger) func(context.Context, supervise.PrunePolicy) (uint64, error) {
	return func(ctx context.Context, p supervise.PrunePolicy) (uint64, error) {
		res := prune.Run(ctx, prune.DockerRunner{}, prune.Options{
			All:        true,
			Until:      p.KeepSince,
			BuildCache: p.BuildCache,
		})
		for _, st := range res.Steps {
			if st.Err != "" {
				log.Warn("automatic prune step failed", "step", st.Name, "error", st.Err)
			}
		}
		if res.Failed > 0 && res.ReclaimedBytes == 0 {
			return 0, fmt.Errorf("every prune step failed (%d)", res.Failed)
		}
		if res.ReclaimedBytes < 0 {
			return 0, nil
		}
		return uint64(res.ReclaimedBytes), nil
	}
}
