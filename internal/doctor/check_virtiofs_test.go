package doctor

import (
	"strings"
	"testing"
)

func TestCheckVirtiofs(t *testing.T) {
	run := func(transport string) Result {
		t.Helper()
		return checkVirtiofs().Run(Facts{MountTransport: transport})
	}

	t.Run("9p warns and says what to do", func(t *testing.T) {
		r := run("9p")
		if r.Status != Warn {
			t.Errorf("status = %v, want Warn: 9p is the slow default and the fix is permanent", r.Status)
		}
		if r.Remedy == "" {
			t.Fatal("a warning with no remedy is just a complaint")
		}
		// The remedy has to carry all three steps. Setting the key without
		// `apply` writes nothing to ~/.wslconfig, and applying without
		// `wsl --shutdown` changes nothing until the next VM start -- both
		// leave someone certain they enabled it when they did not.
		for _, want := range []string{"wsl.virtiofs true", "wsl-config apply", "wsl --shutdown"} {
			if !strings.Contains(r.Remedy, want) {
				t.Errorf("remedy omits %q:\n%s", want, r.Remedy)
			}
		}
		// And the blast radius, because this is a machine-wide change and the
		// project treats ~/.wslconfig as needing consent.
		if !strings.Contains(r.Remedy, "every WSL2 distro") {
			t.Errorf("remedy does not say the change is machine-wide:\n%s", r.Remedy)
		}
		// A warning that cannot say why is noise. The numbers are measured and
		// written up; quoting them is what makes this worth acting on.
		joined := strings.Join(r.Detail, "\n")
		if !strings.Contains(joined, "4.0x") {
			t.Errorf("detail does not quote the measured read speedup:\n%s", joined)
		}
	})

	t.Run("virtiofs is OK and does not nag", func(t *testing.T) {
		r := run("virtiofs")
		if r.Status != OK {
			t.Errorf("status = %v, want OK", r.Status)
		}
		if r.Remedy != "" {
			t.Errorf("nothing to remedy, but got: %s", r.Remedy)
		}
	})

	// Doctor never boots a stopped distro to answer (#82), so an unmeasured
	// transport must skip rather than guess. Guessing from ~/.wslconfig would
	// be wrong in exactly the cases someone runs doctor about: WSL below 2.9
	// ignores the key silently, and it needs a shutdown to take effect.
	t.Run("unknown skips rather than guessing", func(t *testing.T) {
		r := run("")
		if r.Status != Skip {
			t.Errorf("status = %v, want Skip", r.Status)
		}
	})
}
