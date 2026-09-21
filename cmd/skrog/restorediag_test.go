package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wsl"
)

// diagWSL drives engineRunning's two outcomes from a test: List failing is
// "the probe cannot tell", and a distro absent from the list is "not
// running". The interface is embedded and left nil so anything else panics
// rather than passing quietly.
type diagWSL struct {
	wsl.WSL
	distros []wsl.Distro
	listErr error
	// ping is what the engine _ping exec returns; "200 OK" in it is what
	// engineRunning treats as alive.
	ping string
}

func (d diagWSL) List(context.Context) ([]wsl.Distro, error) {
	return d.distros, d.listErr
}

func (d diagWSL) Exec(context.Context, string, string, ...string) (string, error) {
	return d.ping, nil
}

// The message is the deliverable. #468 produced a bug report full of
// hypotheses because "restored engine did not come back within 2m0s" named
// nothing -- not the distro, not what the probe saw, not whether anything was
// even going to start it.
//
// The three cases below need three different actions from whoever reads them,
// which is the whole reason they are distinguished.
func TestRestoreDiagnosis(t *testing.T) {
	opts := provision.Options{Distro: "skrog-engine", StateDir: `C:\state`}

	t.Run("a failing probe is not a stopped engine", func(t *testing.T) {
		// The distinction #437 drew, applied to the one caller that still
		// collapsed it. This path just unregistered and re-imported the
		// distro, so a probe that errors is the LIKELY failure, and reporting
		// it as "the engine did not start" sends the reader after the wrong
		// thing entirely.
		p := &provision.Provisioner{WSL: diagWSL{listErr: errors.New("wsl: the service cannot be started")}}
		got := restoreDiagnosis(context.Background(), p, opts, true)

		if !strings.Contains(got, "probe is FAILING") {
			t.Errorf("a failing probe is not reported as such:\n%s", got)
		}
		if !strings.Contains(got, "service cannot be started") {
			t.Errorf("the underlying error is dropped, so the reader learns nothing:\n%s", got)
		}
	})

	t.Run("came back just too late says so", func(t *testing.T) {
		// The difference between "this is broken" and "this timeout is too
		// short for this machine" is the difference between a bug fix and a
		// constant, and only the message can tell them apart.
		p := &provision.Provisioner{WSL: diagWSL{
			distros: []wsl.Distro{{Name: "skrog-engine", State: "Running"}},
			ping:    "HTTP/1.1 200 OK",
		}}
		got := restoreDiagnosis(context.Background(), p, opts, true)
		if !strings.Contains(got, "too short") {
			t.Errorf("an engine that came back late is not distinguished from one that "+
				"never came back:\n%s", got)
		}
	})

	t.Run("genuinely down, with a supervisor, points at its log", func(t *testing.T) {
		p := &provision.Provisioner{WSL: diagWSL{
			distros: []wsl.Distro{{Name: "skrog-engine", State: "Stopped"}},
		}}
		got := restoreDiagnosis(context.Background(), p, opts, true)
		if !strings.Contains(got, "supervisor.log") || !strings.Contains(got, `C:\state`) {
			t.Errorf("no pointer to the log that would explain it:\n%s", got)
		}
	})

	t.Run("no supervisor says nothing was going to start it", func(t *testing.T) {
		p := &provision.Provisioner{WSL: diagWSL{
			distros: []wsl.Distro{{Name: "skrog-engine", State: "Stopped"}},
		}}
		got := restoreDiagnosis(context.Background(), p, opts, false)
		if !strings.Contains(got, "nothing was going to start it") {
			t.Errorf("a missing supervisor is the whole explanation and is not stated:\n%s", got)
		}
	})

	// Whatever else it says, it must name the distro. A machine with a custom
	// --distro gets a message about "the engine" that could mean any of them.
	t.Run("always names the distro", func(t *testing.T) {
		p := &provision.Provisioner{WSL: diagWSL{listErr: errors.New("boom")}}
		if got := restoreDiagnosis(context.Background(), p, opts, true); !strings.Contains(got, "skrog-engine") {
			t.Errorf("the distro is not named:\n%s", got)
		}
	})
}
