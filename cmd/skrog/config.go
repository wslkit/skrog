package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/engineconfig"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/wsl"
)

func runConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON (list)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog config                    list all settings
       skrog config get <key>          print one value
       skrog config set <key> <value>  change one value
       skrog config export             print the install as a skrog.yaml

Skrog settings apply live: the supervisor re-reads this file when it changes,
so nothing here needs a restart to take effect. Where "live" needs a
qualifier, the setting below says so — settings that configure the engine
itself land when the engine next starts, which `+"`skrog restart`"+` asks for.

`)
		helpRows(os.Stderr, [][2]string{
			{config.KeyIdleTimeout, `how long the bridge must be quiet (no connections, no running
containers) before the engine is stopped to reclaim its RAM; the
next docker command starts it again. A duration like 20m or 1h,
or "off". Defaults to 5m, like Docker Desktop's Resource Saver.`},
			{config.KeyAudit, `record container-affecting API calls to audit.log in the state
dir; on/off ("off" by default). Takes effect on the next docker
call. See ` + "`skrog audit tail`" + `.`},
			{config.KeyAutostart, `start the supervisor at logon; on/off. Setting it writes or
removes the per-user Run entry at once, the same as ` + "`skrog autostart\nenable|disable`" + `, and records the choice so ` + "`skrog doctor`" + ` can tell
"turned off on purpose" from "went missing" -- and put a missing
entry back with --fix.`},
			{config.KeyVerifySignature, `refuse the rootfs at install time unless its signature verifies`},
			{config.KeyDiskWarnBelow, `free-space floor under which ` + "`skrog doctor`" + ` warns, e.g. 10GB`},
			{config.KeyPruneEvery, `how often the supervisor reclaims disk on its own: a duration
like 168h, or "off" (the default). It prunes stopped containers
and unused images older than prune.keep-since, skipping entirely
while containers are running. NEVER volumes.`},
			{config.KeyPruneKeepSince, `how much history an automatic prune keeps; nothing younger is
touched. Defaults to 168h, and cannot be turned off — clear the
key to restore the default.`},
			{config.KeyPruneBuildCache, `also drop the BuildKit cache on an automatic prune; on/off
("off" by default, because the cache is expensive to rebuild).`},
			{config.KeyEmulationPlatforms, `run containers built for another CPU architecture, e.g.
"linux/amd64" (empty by default). For docker run --platform;
cross-architecture BUILDS already work without this, see
docs/docker-cli.md. Registering an emulator affects EVERY WSL2
distro on the machine, not just Skrog's -- they share one kernel
-- which is why it is opt-in. Removed again on skrog stop and
uninstall.`},
			{config.KeyPublishScope, `how far a published container port reaches: "loopback" (the
default, what WSL does on its own -- the port answers on 127.0.0.1
and nowhere else) or "lan", where the supervisor relays published
ports to every interface so another device can reach them. Opt-in:
"lan" puts your containers on the network. See docs/ports.md.`},
			{config.KeyGPU, `install the vendor CDI spec in the engine on every start, so a
container can use the GPU; on/off ("off" by default).
` + "`skrog enable-gpu`" + ` sets this for you and checks the driver.`},
			{config.KeyGPUVendor, `which vendor's CDI spec ` + "`gpu`" + ` installs: nvidia (the default)
or amd. Separate from gpu so switching vendors does not mean
turning the feature off and on.`},
		})
		fmt.Fprintf(os.Stderr, `
Engine settings (engine.<key>) are written into the engine's daemon.json,
validated with `+"`dockerd --validate`"+` before they replace the live file, and
applied by bouncing the engine (rolled back if it does not come back). Set an
empty value to clear a key. Lists are comma-separated; maps are k=v,k=v.

`)
		for _, k := range engineconfig.KeyHelp() {
			fmt.Fprintf(os.Stderr, "  engine.%-24s %s\n", k.Name, k.Help)
		}
		fmt.Fprintf(os.Stderr, `
Lifecycle hooks (hook.<event>) run a script on an engine event, time-bounded
and best-effort (a failure is logged, never blocks the lifecycle). The script
gets SKROG_EVENT and SKROG_STATE_DIR in its environment. Set an empty value
to clear one. Events:
  %s   after the engine starts (recovery or first start)
  %s     before the engine stops on `+"`skrog stop`"+`
  %s  after the idle timeout stops the engine
  %s      after the engine cold-starts on demand
`, config.KeyHookPostStart, config.KeyHookPreStop, config.KeyHookOnIdleStop, config.KeyHookOnWake)
		fmt.Fprintf(os.Stderr, `
Corporate network (applied to the engine on its next start; `+"`skrog restart`"+`
asks for one):
  %s          http(s):// proxy for the engine's registry pulls
  %s       proxy bypass list (comma-separated)
  %s   trust the host's root CA store inside the engine
                             (the fix for a TLS-inspecting proxy; on/off)
`, config.KeyProxy, config.KeyNoProxy, config.KeyImportHostCAs)
		// wsl.* keys are settable HERE but land in ~/.wslconfig rather than
		// Skrog's own settings, so they are documented under `skrog
		// wsl-config` where the apply step and the machine-wide consequences
		// are. Cross-referenced rather than duplicated: `skrog config set
		// wsl.virtiofs true` is a real command, and a help page listing every
		// other family but not this one reads as though it were not.
		fmt.Fprintf(os.Stderr, `
WSL2 VM (%s, %s, %s, %s, %s) are set here but written to
~/.wslconfig, which every WSL2 distro on the machine shares. They need
`+"`skrog wsl-config apply`"+` to be written and a `+"`wsl --shutdown`"+` to take effect,
so they live under `+"`skrog wsl-config --help`"+` with the diff and the caveats.
`, config.KeyWSLMemory, config.KeyWSLProcessors, config.KeyWSLSwap,
			config.KeyWSLAutoMemoryReclaim, config.KeyWSLVirtiofs)
		fmt.Fprintf(os.Stderr, "\nExit codes: 0 ok, %d error, %d usage.\n", exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	rest := fs.Args()

	switch {
	case len(rest) == 0:
		return listAllConfig(opts, *asJSON)

	case rest[0] == "get" && len(rest) == 2:
		return getConfig(opts, rest[1])

	case rest[0] == "set" && len(rest) == 3:
		return setConfig(opts, rest[1], rest[2])

	case rest[0] == "export" && len(rest) == 1:
		return exportConfig(opts)

	default:
		fmt.Fprintf(os.Stderr, "skrog: config %s: unrecognized; see `skrog config --help`\n",
			strings.Join(rest, " "))
		return exitUsage
	}
}

// exportConfig emits the current install as a skrog.yaml, so an existing
// setup round-trips into a file that reproduces it with `skrog install --config`.
func exportConfig(opts provision.Options) int {
	f, err := exportSkrogFile(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitNotFound
	}
	out, err := f.Marshal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	os.Stdout.Write(out)
	return exitOK
}

func listAllConfig(opts provision.Options, asJSON bool) int {
	all, err := config.All(opts.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	// autostart is reported as it is, not as a bare stored string: the
	// recorded choice, or what is registered when nothing was recorded (#515).
	as := readAutostart(opts.StateDir)
	all[config.KeyAutostart] = as.Effective()

	// Engine settings live in the distro; list them only when one is installed,
	// so `skrog config` still works on a machine with no engine.
	var eng map[string]string
	if m, ok := engineManager(opts); ok {
		eng, err = m.List(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: reading engine config: %v\n", err)
			return exitError
		}
		if eng == nil {
			eng = map[string]string{} // installed with nothing set is {}, not null
		}
	}

	if asJSON {
		return emitJSON(configListJSON{Settings: all, Engine: eng})
	}

	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := all[k]
		if k == config.KeyAutostart {
			v = as.Describe()
		}
		fmt.Printf("%s = %s\n", k, v)
	}
	ekeys := make([]string, 0, len(eng))
	for k := range eng {
		ekeys = append(ekeys, k)
	}
	sort.Strings(ekeys)
	for _, k := range ekeys {
		fmt.Printf("%s = %s\n", k, eng[k])
	}
	return exitOK
}

func getConfig(opts provision.Options, key string) int {
	if engineconfig.IsEngineKey(key) {
		m, ok := engineManager(opts)
		if !ok {
			fmt.Fprintln(os.Stderr, "skrog: no engine installed; run `skrog install` first")
			return exitNotFound
		}
		v, err := m.Get(context.Background(), engineconfig.StripPrefix(key))
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Println(v)
		return exitOK
	}

	if key == config.KeyAutostart {
		fmt.Println(readAutostart(opts.StateDir).Effective())
		return exitOK
	}

	v, err := config.Get(opts.StateDir, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Println(v)
	return exitOK
}

func setConfig(opts provision.Options, key, value string) int {
	if engineconfig.IsEngineKey(key) {
		m, ok := engineManager(opts)
		if !ok {
			fmt.Fprintln(os.Stderr, "skrog: no engine installed; run `skrog install` first")
			return exitNotFound
		}
		res, err := m.Set(context.Background(), engineconfig.StripPrefix(key), value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Printf("%s = %s\n", key, res.Applied)
		switch {
		case res.Restarted:
			fmt.Println("engine restarted to apply the change")
		case res.PendingRestart:
			fmt.Println("engine is not running; the change applies on the next start")
		}
		return exitOK
	}

	// Not a plain stored value: the Run entry changes first, and the choice is
	// recorded only once it has (#515).
	if key == config.KeyAutostart {
		on, err := config.OnOff(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		if err := applyAutostart(opts.StateDir, on); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Printf("%s = %s (%s)\n", key, readAutostart(opts.StateDir).Describe(), config.Applies(key))
		return exitOK
	}

	if err := config.Set(opts.StateDir, key, value); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	v, _ := config.Get(opts.StateDir, key)
	fmt.Printf("%s = %s (%s)\n", key, v, config.Applies(key))
	return exitOK
}

// engineManager builds an engineconfig.Manager for the installed engine, wiring
// the running-check and restart hooks to the provisioner. Returns false when no
// engine is installed.
func engineManager(opts provision.Options) (*engineconfig.Manager, bool) {
	log := cliLogger(true)
	p := &provision.Provisioner{Logger: log}
	distro, ok := resolveDistro(p, opts)
	if !ok {
		return nil, false
	}
	opts.Distro = distro

	return &engineconfig.Manager{
		WSL:    wsl.NewLocal(),
		Distro: distro,
		EngineRunning: func(ctx context.Context) bool {
			return p.EngineRunning(ctx, opts)
		},
		// A nil return means the engine came back healthy — the signal Set needs
		// to decide whether to roll back.
		Restart: func(ctx context.Context) error {
			return bounceEngine(ctx, p, opts)
		},
	}, true
}

// bounceEngine restarts the engine so dockerd re-reads daemon.json, returning
// nil only once it answers again. When a supervisor holds the lock it is the
// sole owner of the engine's lifecycle, so the bounce goes through the
// desired-state file (stop, wait down, run, wait up) rather than a direct
// stop/start that would race the reconciler. With no supervisor, it drives the
// provisioner directly.
func bounceEngine(ctx context.Context, p *provision.Provisioner, opts provision.Options) error {
	const settle = 90 * time.Second

	if !supervise.Held(opts.StateDir) {
		if err := p.StopEngine(ctx, opts); err != nil {
			return err
		}
		return p.StartEngine(ctx, startOptions(ctx, opts))
	}

	if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredStopped); err != nil {
		return err
	}
	if !waitFor(ctx, settle, func() bool { return !p.EngineRunning(ctx, opts) }) {
		return fmt.Errorf("engine did not stop within %s", settle)
	}
	if err := supervise.WriteDesired(opts.StateDir, supervise.DesiredRunning); err != nil {
		return err
	}
	if !waitFor(ctx, settle, func() bool { return p.EngineRunning(ctx, opts) }) {
		return fmt.Errorf("engine did not come back within %s", settle)
	}
	return nil
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(1 * time.Second):
		}
	}
	return cond()
}
