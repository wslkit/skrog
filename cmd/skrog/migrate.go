package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/wslkit/skrog/internal/migrate"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
)

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	var (
		fromDesktop = fs.Bool("from-desktop", false, "migrate from Docker Desktop (the desktop-linux context)")
		fromRancher = fs.Bool("from-rancher", false, "migrate from Rancher Desktop (the rancher-desktop context; needs its dockerd backend)")
		fromPodman  = fs.Bool("from-podman", false, "migrate from Podman (its Docker-compatible API on the machine's named pipe)")
		fromContext = fs.String("from-context", "", "source docker context, instead of one of the --from-* engines")
		fromHost    = fs.String("from-host", "", "source engine endpoint, e.g. npipe:////./pipe/podman-machine-<name>")
		dryRun      = fs.Bool("dry-run", false, "list what would move and how big it is, then stop")
		only        multiFlag
		stateDir    = fs.String("state-dir", "", "override Skrog's state directory")
		dockerExe   = fs.String("docker", "", "path to the docker CLI (default: docker on PATH)")
		destHost    = fs.String("docker-host", "", "destination engine (default: the Skrog pipe this install serves)")
	)
	fs.Var(&only, "only", "limit to images/volumes whose name contains this (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog migrate --from-desktop|--from-rancher|--from-podman [--dry-run]

Copies images and named volumes from another engine into the Skrog engine, so
trying Skrog does not mean starting from an empty one.

  skrog migrate --from-desktop --dry-run    # what would move, and how big
  skrog migrate --from-rancher
  skrog migrate --from-podman

The copy is one-way and non-destructive: nothing in the source is changed or
removed, and an interrupted migration leaves it exactly as it was. Run it again
to resume — anything already copied is skipped. Images stream via docker
save|load, volumes via a streamed tar; neither stages a file on disk.

Sources:
  --from-desktop   Docker Desktop, via its desktop-linux context
  --from-rancher   Rancher Desktop, via its rancher-desktop context. Needs the
                   dockerd (moby) backend: with containerd selected there is no
                   Docker API to read, and nothing here can work around that
  --from-podman    Podman, via its Docker-compatible API on the machine's named
                   pipe. Nothing shells out to podman; the docker CLI talks to
                   it directly. For a non-default machine, --from-host
                   npipe:////./pipe/podman-machine-<name>

  --from-context / --from-host address any other engine directly.

Build cache is not migrated: it is not portable through save/load. Anonymous
(unnamed) volumes are skipped — they belong to specific containers, which do
not move.

Exit codes: 0 ok, %d error, %d usage.

flags:
`, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	source, err := resolveMigrateSource(map[string]bool{
		"from-desktop": *fromDesktop,
		"from-rancher": *fromRancher,
		"from-podman":  *fromPodman,
	}, *fromContext, *fromHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	log := cliLogger(false)

	p := &provision.Provisioner{Logger: log}
	if _, ok := requireDistroInstall(p, opts); !ok {
		return exitNotFound
	}

	// The destination is addressed by the engine's own pipe, not the shared
	// "skrog" context: the context may be absent (install could not create
	// it) or point at a different engine, whereas the pipe this install serves
	// is unambiguous. --docker-host overrides it.
	dockerHost := *destHost
	if dockerHost == "" {
		pipe, _ := pipeproxy.SelectPipeName("")
		dockerHost = pipeproxy.DockerHostFor(pipe)
	}

	src := migrate.DockerCLI{Exe: *dockerExe, Context: source.Context, Host: source.Host}
	dst := migrate.DockerCLI{Exe: *dockerExe, Host: dockerHost}
	transfer := migrate.CLITransfer{Src: src, Dest: dst}
	m := &migrate.Migrator{Source: src, Dest: dst, Transfer: transfer, Logger: log, Only: only}

	// Ctrl-C must cancel rather than kill: a half-streamed volume has to be
	// cleaned up, and a killed process runs no deferred cleanup at all (#236).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	plan, err := m.Plan(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: reading %s: %v\n", source.Label, err)
		fmt.Fprintf(os.Stderr, "  %s\n", source.Hint)
		return exitError
	}

	printPlan(plan)
	if *dryRun {
		fmt.Println("\nDry run: nothing was copied. Re-run without --dry-run to migrate.")
		return exitOK
	}
	if plan.Empty() {
		return exitOK
	}

	// The tar helper image must exist on the destination before volume moves.
	if len(plan.Volumes) > 0 {
		if err := transfer.EnsureTarImage(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
	}

	fmt.Println()
	result := m.Run(ctx, plan)
	fmt.Printf("\nMigrated %d image(s) and %d volume(s).\n", result.ImagesMoved, result.VolumesMoved)
	if len(result.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d item(s) did not migrate:\n", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		fmt.Fprintln(os.Stderr, "Docker Desktop is unchanged; re-run to retry the rest.")
		return exitError
	}
	fmt.Println("Docker Desktop was not modified.")
	return exitOK
}

func printPlan(p migrate.Plan) {
	if p.Empty() {
		fmt.Println("Nothing to migrate: the Skrog engine already has everything the source does.")
		if p.Unreferenced > 0 {
			fmt.Printf("(%d dangling source image(s) skipped — they have no tag to move.)\n", p.Unreferenced)
		}
		return
	}
	fmt.Printf("Would migrate %s in total:\n", migrate.ByteSize(p.TotalBytes()))
	if len(p.Images) > 0 {
		fmt.Printf("\nImages (%d):\n", len(p.Images))
		for _, img := range p.Images {
			fmt.Printf("  %-50s %s\n", img.Ref, migrate.ByteSize(img.Size))
		}
	}
	if len(p.Volumes) > 0 {
		fmt.Printf("\nVolumes (%d):\n", len(p.Volumes))
		for _, v := range p.Volumes {
			fmt.Printf("  %-50s %s\n", v.Name, migrate.ByteSize(v.Size))
		}
	}
	if n := len(p.SkippedImgs) + len(p.SkippedVols); n > 0 {
		fmt.Printf("\nAlready on the Skrog engine, will skip: %d image(s), %d volume(s).\n",
			len(p.SkippedImgs), len(p.SkippedVols))
	}
	if p.Unreferenced > 0 {
		fmt.Printf("Dangling source images skipped (no tag to move): %d.\n", p.Unreferenced)
	}
}
