package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wslkit/skrog/internal/engineconfig"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/prune"
	"github.com/wslkit/skrog/internal/regcache"
)

// runCache is `skrog cache` (#385): a pull-through registry cache on the
// engine, so the same layers are fetched from the internet once.
func runCache(args []string) int {
	sub, rest := splitSubcommand(args)

	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		upstream = fs.String("upstream", "", "registry to cache (default: Docker Hub)")
		port     = fs.Int("port", 0, "loopback port inside the engine (default 5000)")
		keepData = fs.Bool("keep-data", false, "on disable, keep the cached layers")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog cache enable|disable|status

Runs a pull-through registry cache on the engine, so a layer is pulled from
the internet once and served locally after that.

  skrog cache enable                    # cache Docker Hub
  skrog cache enable --upstream https://ghcr.io
  skrog cache status --json
  skrog cache disable                   # also deletes the cached layers
  skrog cache disable --keep-data       # ...unless you keep them

Why you would: Docker Hub rate-limits anonymous pulls PER IP, so a corporate
NAT or a runner fleet hits the limit as an organisation rather than as the
developer who sees the error. A slow or TLS-inspecting corporate link pays for
the same bytes every time. Both stop after the first pull.

It is the upstream registry:2 image in proxy mode, pinned by digest, with its
store in the %s volume — so `+"`skrog prune`"+`, `+"`compact`"+` and
`+"`relocate`"+` already account for its disk. dockerd reaches it on loopback,
which Docker treats as insecure-by-default, so nothing is exposed to the
network and no insecure-registries entry is needed.

It does not weaken `+"`skrog policy`"+`: a mirror changes where bytes come
from, not which image was asked for, and the rules judge the reference.

It does not hold the engine awake. The cache container is excluded from the
idle-stop and scheduled-prune probes, because it is infrastructure rather than
work.

Exit codes: 0 ok, %d error, %d usage, %d not installed.

flags:
`, regcache.VolumeName, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	cacheOpts := regcache.Options{Upstream: *upstream, Port: *port}
	runner := prune.DockerRunner{}
	ctx := context.Background()

	switch sub {
	case "enable":
		return cacheEnable(ctx, runner, opts, cacheOpts)
	case "disable":
		return cacheDisable(ctx, runner, opts, *keepData)
	case "status", "":
		return cacheStatus(ctx, runner, opts, *asJSON)
	default:
		fmt.Fprintf(os.Stderr, "skrog cache: unknown subcommand %q (enable|disable|status)\n", sub)
		return exitUsage
	}
}

func cacheEnable(ctx context.Context, r prune.DockerRunner, opts provision.Options, c regcache.Options) int {
	if err := regcache.ValidateUpstream(c.Upstream); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}
	m, ok := engineManager(opts)
	if !ok {
		fmt.Fprintln(os.Stderr, "skrog: no engine installed (run `skrog install`)")
		return exitNotFound
	}

	// Pull BEFORE wiring the mirror. Otherwise the first thing the new mirror
	// is asked for is the image of the cache that is not running yet — which
	// works only because dockerd falls back to the upstream, and relying on a
	// fallback to bootstrap the thing doing the falling back is the kind of
	// ordering that fails on somebody else's network.
	fmt.Printf("pulling %s...\n", regcache.Image)
	if _, err := r.Run(ctx, "pull", regcache.Image); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: pulling the registry image: %v\n", err)
		return exitError
	}

	// A leftover container from a previous enable would make `run` fail on the
	// name; remove it first so enable is idempotent.
	_, _ = r.Run(ctx, "rm", "-f", regcache.ContainerName)
	if _, err := r.Run(ctx, regcache.RunArgs(c)...); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: starting the cache: %v\n", err)
		return exitError
	}

	// Through engineconfig so the change is validated with `dockerd --validate`
	// and rolled back if the engine does not come back — the same contract
	// every other engine setting gets.
	mirrors, err := mirrorsWith(ctx, m, c.MirrorURL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: reading the engine config: %v\n", err)
		return exitError
	}
	if _, err := m.Set(ctx, regcache.MirrorKey, strings.Join(mirrors, ",")); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: wiring the mirror: %v\n", err)
		return exitError
	}

	fmt.Printf("\ncache enabled\n  upstream  %s\n  mirror    %s\n  data      %s (a volume, so `skrog prune` sees it)\n",
		cmp(c.Upstream, regcache.DefaultUpstream), c.MirrorURL(), regcache.VolumeName)
	fmt.Println("\nPulls now go through the cache. The first is no faster; the rest are.")
	return exitOK
}

func cacheDisable(ctx context.Context, r prune.DockerRunner, opts provision.Options, keepData bool) int {
	m, ok := engineManager(opts)
	if !ok {
		fmt.Fprintln(os.Stderr, "skrog: no engine installed")
		return exitNotFound
	}

	// Unwire first: a daemon.json pointing at a mirror that is about to stop
	// existing is the one state that breaks pulls, so it must not be the state
	// we linger in if a later step fails.
	st, _ := regcache.Probe(ctx, r, currentMirrors(ctx, m))
	if st.Wired {
		mirrors := without(currentMirrors(ctx, m), st.MirrorURL)
		if _, err := m.Set(ctx, regcache.MirrorKey, strings.Join(mirrors, ",")); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: unwiring the mirror: %v\n", err)
			return exitError
		}
	}

	if _, err := r.Run(ctx, "rm", "-f", regcache.ContainerName); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: removing the cache container: %v\n", err)
		return exitError
	}
	if !keepData {
		if _, err := r.Run(ctx, "volume", "rm", regcache.VolumeName); err != nil {
			// Not fatal: the container is gone and the mirror is unwired, so
			// the cache is off. A leftover volume is disk, not breakage.
			fmt.Fprintf(os.Stderr, "skrog: note: could not remove %s: %v\n", regcache.VolumeName, err)
		}
	}

	fmt.Println("cache disabled")
	if keepData {
		fmt.Printf("  %s kept; `skrog cache enable` reuses it\n", regcache.VolumeName)
	}
	return exitOK
}

func cacheStatus(ctx context.Context, r prune.DockerRunner, opts provision.Options, asJSON bool) int {
	m, ok := engineManager(opts)
	if !ok {
		if asJSON {
			return emitJSON(regcache.Status{})
		}
		fmt.Fprintln(os.Stderr, "skrog: no engine installed")
		return exitNotFound
	}
	st, err := regcache.Probe(ctx, r, currentMirrors(ctx, m))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if asJSON {
		return emitJSON(st)
	}
	if !st.Enabled {
		fmt.Println("cache      disabled  (`skrog cache enable`)")
		return exitOK
	}
	fmt.Printf("cache      %s\nupstream   %s\nmirror     %s\ndata       %s\n",
		runningWord(st.Running), st.Upstream, st.MirrorURL, humanBytes(int64(st.DataBytes)))
	// The one state worth calling out: a cache that is up and that nothing is
	// pointed at looks healthy and does nothing.
	if !st.Wired {
		fmt.Println("\nwarning: the engine is NOT using it — daemon.json has no matching")
		fmt.Println("registry-mirrors entry. `skrog cache enable` rewires it.")
	}
	return exitOK
}

func runningWord(b bool) string {
	if b {
		return "running"
	}
	return "stopped  (it returns with the engine)"
}

func cmp(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// currentMirrors reads registry-mirrors, treating any failure as "none": the
// callers use it to add or remove one entry, and a read failure must not make
// `cache status` report a wiring problem that does not exist.
func currentMirrors(ctx context.Context, m *engineconfig.Manager) []string {
	v, err := m.Get(ctx, regcache.MirrorKey)
	if err != nil || strings.TrimSpace(v) == "" {
		return nil
	}
	return splitMirrors(v)
}

// mirrorsWith returns the configured mirrors with url first and no duplicate.
// First because dockerd tries them in order, and the local cache should be the
// one that answers.
func mirrorsWith(ctx context.Context, m *engineconfig.Manager, url string) ([]string, error) {
	out := []string{url}
	for _, existing := range currentMirrors(ctx, m) {
		if strings.TrimRight(existing, "/") != url {
			out = append(out, existing)
		}
	}
	return out, nil
}

func without(mirrors []string, url string) []string {
	out := make([]string, 0, len(mirrors))
	for _, m := range mirrors {
		if strings.TrimRight(m, "/") != url {
			out = append(out, m)
		}
	}
	return out
}

func splitMirrors(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
