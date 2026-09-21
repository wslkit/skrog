package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/wsl"
)

// probeWSL answers List and Exec from canned values. The interface is
// embedded and left nil so any other call panics loudly.
type probeWSL struct {
	wsl.WSL
	distros []wsl.Distro
	listErr error
	out     string
	execErr error
	ranCmd  string
}

func (p *probeWSL) List(context.Context) ([]wsl.Distro, error) { return p.distros, p.listErr }

func (p *probeWSL) Exec(_ context.Context, _, _ string, args ...string) (string, error) {
	if len(args) > 0 {
		p.ranCmd = args[len(args)-1]
	}
	return p.out, p.execErr
}

func running(name string) []wsl.Distro { return []wsl.Distro{{Name: name, State: "Running"}} }

// The distinction the supervisor acts on, and the one that broke (#468).
//
// Since #437 the supervisor SKIPS ITS TICK when the probe returns an error,
// so that "cannot tell" never causes it to start an engine that is probably
// already running. That is right — but it makes it critical that "dockerd is
// not listening" is NOT reported as an error, because it is an answer.
//
// It was. socat exits non-zero when the socket is missing, that reached
// engineRunning as an error, and the supervisor stopped repairing a dead
// engine whenever the distro stayed up. Its whole job, silently off, on the
// case #82's comment has always claimed to cover.
func TestEngineProbeSeparatesCannotAskFromNotRunning(t *testing.T) {
	const distro = "skrog-engine"
	opts := Options{Distro: distro}

	t.Run("dockerd answering is up", func(t *testing.T) {
		p := &Provisioner{WSL: &probeWSL{
			distros: running(distro),
			out:     "HTTP/1.1 200 OK\r\n\r\n",
		}}
		up, err := p.engineRunning(context.Background(), opts.withDefaults())
		if err != nil || !up {
			t.Errorf("up=%v err=%v, want true/nil", up, err)
		}
	})

	// The case that broke. socat now exits 0 thanks to `|| true`, so this
	// arrives as empty output rather than an error -- but assert the contract
	// the supervisor depends on, not the mechanism: no socket means DOWN with
	// NO error, so the next tick starts the engine.
	t.Run("no socket is down, not an error", func(t *testing.T) {
		p := &Provisioner{WSL: &probeWSL{
			distros: running(distro),
			out:     "socat[585] E connect(, AF=1 \"/var/run/docker.sock\", 22): No such file or directory",
		}}
		up, err := p.engineRunning(context.Background(), opts.withDefaults())
		if up {
			t.Error("reported up with no socket")
		}
		if err != nil {
			t.Errorf("err=%v, want nil.\n"+
				"  A missing socket is the ANSWER 'not running', not a failure to ask. "+
				"Reported as an error, the supervisor skips its tick and never repairs "+
				"the engine (#468).", err)
		}
	})

	// A genuinely unanswerable probe must still error, or #437 regresses the
	// other way: the supervisor would start an engine it could not see.
	t.Run("a broken exec is still an error", func(t *testing.T) {
		p := &Provisioner{WSL: &probeWSL{
			distros: running(distro),
			execErr: errors.New("wsl: the distribution failed to start"),
		}}
		if _, err := p.engineRunning(context.Background(), opts.withDefaults()); err == nil {
			t.Error("an exec that could not run reported no error; the supervisor would " +
				"treat 'cannot tell' as 'down' and start an engine it cannot see (#437)")
		}
	})

	t.Run("a failing list is still an error", func(t *testing.T) {
		p := &Provisioner{WSL: &probeWSL{listErr: errors.New("wsl: service not running")}}
		if _, err := p.engineRunning(context.Background(), opts.withDefaults()); err == nil {
			t.Error("a failing distro list reported no error")
		}
	})

	// A stopped distro answers without exec'ing, because exec BOOTS it (#82)
	// and that would defeat idle-stop.
	t.Run("a stopped distro is down and is never exec'd into", func(t *testing.T) {
		w := &probeWSL{distros: []wsl.Distro{{Name: distro, State: "Stopped"}}}
		p := &Provisioner{WSL: w}
		up, err := p.engineRunning(context.Background(), opts.withDefaults())
		if up || err != nil {
			t.Errorf("up=%v err=%v, want false/nil", up, err)
		}
		if w.ranCmd != "" {
			t.Errorf("exec'd into a stopped distro (%q), which boots it and defeats idle-stop", w.ranCmd)
		}
	})
}

// The `|| true` is the fix, and a future tidy-up that removes it would
// silently restore the bug: socat's exit status would reach engineRunning as
// an error again, and the supervisor would go back to skipping ticks.
func TestEnginePingSwallowsSocatsExitStatus(t *testing.T) {
	if !strings.Contains(enginePing, "|| true") {
		t.Error("enginePing no longer ends in `|| true`.\n" +
			"  socat exits non-zero whenever dockerd is not listening, and that exit " +
			"status reaches engineRunning as an ERROR rather than the answer 'no'. " +
			"The supervisor skips its tick on a probe error (#437), so it stops " +
			"repairing a dead engine whenever the distro stays up (#468).")
	}
}
