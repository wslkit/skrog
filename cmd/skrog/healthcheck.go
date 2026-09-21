package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
)

// runHealthcheck is `skrog healthcheck` (#146): a yes/no readiness probe for
// runner warm-up scripts, orchestrators and setup-skrog. Ready means a docker
// command will succeed: the supervisor is serving the pipe and the engine is
// running — or idle, which wakes on the next call. It never boots anything
// itself: --wait polls while a supervisor brings the engine up (`skrog start`
// is what asks for that), and the probe is host-side (#82).
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		wait     = fs.Duration("wait", 0, "keep probing until ready or this long has passed (e.g. 2m)")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog healthcheck [--wait <duration>] [--json]

A readiness probe: exits 0 when a docker command would succeed right now —
the supervisor is serving the pipe and the engine is running or idle (an idle
engine wakes on the next docker call). Nothing is started; --wait keeps
probing while `+"`skrog start`"+` (or the logon autostart) brings the engine up.

  skrog healthcheck --wait 2m && docker run --rm hello-world

Exit codes: 0 ready, %d not ready, %d usage, %d not installed.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: cliLogger(true)}

	target, ok := resolveEngineTarget(p, opts)
	if !ok {
		return healthcheckReport(healthcheckJSON{
			Supervisor: "stopped", Engine: "stopped", Reason: "not installed; run `skrog install`",
		}, *asJSON, exitNotFound)
	}
	opts.Distro = target.Distro

	deadline := time.Now().Add(*wait)
	for {
		hc := probeHealth(context.Background(), p, opts)
		if hc.Ready {
			return healthcheckReport(hc, *asJSON, exitOK)
		}
		if !time.Now().Before(deadline) {
			return healthcheckReport(hc, *asJSON, exitError)
		}
		time.Sleep(time.Second)
	}
}

// probeHealth reads the same facts `skrog status` does and applies the one
// readiness rule.
func probeHealth(ctx context.Context, p *provision.Provisioner, opts provision.Options) healthcheckJSON {
	hc := healthcheckJSON{Installed: true, Supervisor: "stopped", Engine: "stopped"}
	if supervise.Held(opts.StateDir) {
		hc.Supervisor = "running"
	}
	// Never boots the engine to answer: a probe that did would defeat the
	// idle timeout it is supposed to observe (#82).
	switch {
	case p.EngineRunning(ctx, opts):
		hc.Engine = "running"
	case supervise.ReadEngineState(opts.StateDir) == supervise.EngineIdle:
		hc.Engine = "idle"
	}
	hc.Ready, hc.Reason = readiness(hc.Supervisor == "running", hc.Engine)
	return hc
}

// readiness is the rule, kept pure so it is tested exhaustively: the pipe must
// be served, and the engine must be able to answer. Idle counts — the next
// docker call wakes it — which is exactly the state a runner sits in between
// jobs when the idle timeout is on.
func readiness(supervisorRunning bool, engine string) (ready bool, reason string) {
	switch {
	case !supervisorRunning:
		return false, "supervisor not running; run `skrog start`"
	case engine == "stopped":
		return false, "engine stopped; run `skrog start`"
	case engine == "idle":
		return true, "engine idle; wakes on the next docker command"
	default:
		return true, "engine running"
	}
}

func healthcheckReport(hc healthcheckJSON, asJSON bool, code int) int {
	if asJSON {
		if c := emitJSON(hc); c != exitOK {
			return c
		}
		return code
	}
	if hc.Ready {
		fmt.Println("ready: " + hc.Reason)
	} else {
		fmt.Fprintln(os.Stderr, "not ready: "+hc.Reason)
	}
	return code
}
