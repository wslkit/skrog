package doctor

import (
	"runtime"
	"strings"
	"testing"
)

// The case the check exists for (#480): the user asked for emulation, the
// engine is up, and no handler is registered. Before this check, doctor said
// nothing at all and `multi-arch` reported "nothing is wrong here".
func TestCheckEmulationWarnsWhenTheHandlerIsMissing(t *testing.T) {
	r := checkEmulation().Run(Facts{
		EmulationPlatforms: "linux/arm64",
		MultiArch:          MultiArchInfo{Probed: true},
	})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: the setting is on and the handler is not live", r.Status)
	}
	if !strings.Contains(r.Summary, "arm64") {
		t.Errorf("summary does not name the architecture: %q", r.Summary)
	}
	// The remedy has to name the likeliest cause, which a user cannot guess:
	// the emulator is in the engine image (#479).
	for _, want := range []string{"29.8.1-3", "engine upgrade"} {
		if !strings.Contains(r.Remedy, want) {
			t.Errorf("remedy should mention %q: %q", want, r.Remedy)
		}
	}
}

func TestCheckEmulationOKWhenTheHandlerIsLive(t *testing.T) {
	r := checkEmulation().Run(Facts{
		EmulationPlatforms: "linux/arm64",
		MultiArch:          MultiArchInfo{Probed: true, Handlers: []string{"qemu-aarch64"}},
	})
	if r.Status != OK {
		t.Fatalf("status = %v, want OK: the requested handler is registered", r.Status)
	}
}

// Silent unless asked. A machine that never set the key must not grow a line
// about a feature it does not use — that is the noise checkMultiArch's comment
// is about, and this check would be worse at it because it can warn.
func TestCheckEmulationSaysNothingWhenNotConfigured(t *testing.T) {
	for _, setting := range []string{"", "   "} {
		r := checkEmulation().Run(Facts{
			EmulationPlatforms: setting,
			MultiArch:          MultiArchInfo{Probed: true},
		})
		if r.Status != Skip {
			t.Errorf("setting %q: status = %v, want Skip", setting, r.Status)
		}
	}
}

// doctor never boots a distro to answer (#82), so with the engine down the
// table was not read and "no handler" would be a lie rather than a finding.
func TestCheckEmulationSkipsWhenTheEngineIsDown(t *testing.T) {
	r := checkEmulation().Run(Facts{
		EmulationPlatforms: "linux/arm64",
		MultiArch:          MultiArchInfo{Probed: false},
	})
	if r.Status != Skip {
		t.Errorf("status = %v, want Skip when nothing was probed", r.Status)
	}
}

// Asking to emulate the machine's own architecture is the mistake most worth
// catching: a handler for the host's own ELF type routes binaries the CPU runs
// natively through an emulator, which is a machine that still works and is
// several times slower for no reported reason. Parse refuses the whole setting,
// and doctor must say so rather than report a missing handler.
func TestCheckEmulationWarnsWhenAskedToEmulateTheHostItself(t *testing.T) {
	native := "linux/" + runtime.GOARCH
	r := checkEmulation().Run(Facts{
		EmulationPlatforms: native,
		MultiArch:          MultiArchInfo{Probed: true},
	})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn for %q", r.Status, native)
	}
	if !strings.Contains(r.Remedy, "Supported") {
		t.Errorf("remedy should list what is supported: %q", r.Remedy)
	}
}

// An unusable setting is the user's typo, not a missing handler, and the two
// want different answers.
func TestCheckEmulationWarnsOnAnUnusableSetting(t *testing.T) {
	r := checkEmulation().Run(Facts{
		EmulationPlatforms: "linux/sparc",
		MultiArch:          MultiArchInfo{Probed: true},
	})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Remedy), "supported") {
		t.Errorf("remedy should list what is supported: %q", r.Remedy)
	}
}

// A check nobody runs is not a check. The tests above call checkEmulation()
// directly, which passes just as happily when it is not in Registry() -- the
// shape of defect this repo has shipped before: a correct helper, and nothing
// calling it.
func TestEmulationCheckIsRegistered(t *testing.T) {
	for _, c := range Registry() {
		if c.Name == "emulation" {
			return
		}
	}
	t.Error("checkEmulation is not in Registry(), so `skrog doctor` never runs it")
}
