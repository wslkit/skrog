package provision_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wsl"
)

// DistroRunning answers from the listing alone (#518): it must never exec,
// which would boot the distro it is asking about (#82).
func TestDistroRunningReadsTheListingOnly(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"Running", true},
		{"Stopped", false},
		{"Installing", false},
	} {
		f := &fakeWSL{distros: []wsl.Distro{{Name: "skrog-engine", State: tc.state}}}
		p := &provision.Provisioner{WSL: f}
		got, err := p.DistroRunning(context.Background(), provision.Options{Distro: "skrog-engine"})
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.state, err)
		}
		if got != tc.want {
			t.Errorf("%s: DistroRunning = %v, want %v", tc.state, got, tc.want)
		}
		if len(f.execs) != 0 {
			t.Errorf("%s: DistroRunning exec'd into the distro: %v", tc.state, f.execs)
		}
	}
}

// "Cannot tell" and "not registered" are errors, never "stopped": the
// supervisor reads false as "stopped from outside, leave it", and a broken
// install or a failing listing must not be left down on that account.
func TestDistroRunningUnknownIsAnError(t *testing.T) {
	p := &provision.Provisioner{WSL: &fakeWSL{listErr: errors.New("wslservice not answering")}}
	if _, err := p.DistroRunning(context.Background(), provision.Options{Distro: "skrog-engine"}); err == nil {
		t.Error("a failed listing was not reported as an error")
	}
	p = &provision.Provisioner{WSL: &fakeWSL{distros: []wsl.Distro{{Name: "Ubuntu", State: "Running"}}}}
	if _, err := p.DistroRunning(context.Background(), provision.Options{Distro: "skrog-engine"}); err == nil {
		t.Error("an unregistered distro was reported as merely stopped")
	}
}
