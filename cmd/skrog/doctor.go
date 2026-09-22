package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/wslkit/skrog/internal/autostart"
	"github.com/wslkit/skrog/internal/doctor"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/selfexe"
)

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
		asReport = fs.Bool("report", false, "emit a Markdown report to paste into an issue")
		fix      = fs.Bool("fix", false, "apply the safely auto-remediable fixes, then re-check")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog doctor [--fix] [--json | --report]

Diagnoses the host: WSL2, the docker CLI on PATH, credential helpers, the
engine install, the docker context, supervisor/engine agreement, disk space,
and unattended-startup readiness. Each check prints a remedy when it is not OK.

--fix applies only the changes that are safe without elevation (currently:
setting the default WSL version to 2); everything else prints its remedy.

Exit codes: %d all checks passed or only warnings, %d one or more failed,
%d usage.

flags:
`, exitOK, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *asJSON && *asReport {
		fmt.Fprintln(os.Stderr, "skrog: choose one of --json or --report, not both")
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})

	skrogBin := selfexe.Dir()
	autostartOn, _, _ := autostart.Status()

	ctx := context.Background()
	facts := doctor.Gather(ctx, doctor.GatherOptions{
		StateDir:            opts.StateDir,
		SkrogBin:            skrogBin,
		AppVersion:          buildVersion,
		AutostartConfigured: autostartOn,
		// Listing published ports needs the engine transport, which doctor
		// deliberately does not know about (#507).
		PublishedPorts: publishedPorts(
			engineDialer(installedDistro(opts), "", opts.StateDir, slog.New(slog.DiscardHandler)), nil),
	})

	reg := doctor.Registry()
	results := doctor.Run(reg, facts)
	if *fix {
		results = doctor.ApplyFixes(ctx, reg, facts, results)
	}

	var err error
	switch {
	case *asJSON:
		err = doctor.WriteJSON(os.Stdout, buildVersion, results)
	case *asReport:
		err = doctor.WriteMarkdownReport(os.Stdout, buildVersion, results)
	default:
		err = doctor.WriteText(os.Stdout, buildVersion, results)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	if doctor.Worst(results) == doctor.Fail {
		return exitError
	}
	return exitOK
}
