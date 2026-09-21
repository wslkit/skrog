package provision

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/emulation"
	"github.com/wslkit/skrog/internal/wsl"
)

// The script is the whole of the feature: everything else is config plumbing.
// A handler that registers with a mangled magic matches nothing, and nothing
// reports it -- `docker run --platform` just keeps saying exec format error.
func TestEmulationScript(t *testing.T) {
	h, ok := emulation.HandlerFor("arm64")
	if !ok {
		t.Fatal("no arm64 handler")
	}
	got := emulationScript([]emulation.Handler{h})

	// binfmt_misc is not mounted in a fresh WSL2 distro until something asks.
	if !strings.Contains(got, "mount -t binfmt_misc") {
		t.Error("the script never mounts binfmt_misc; a fresh distro has it unmounted")
	}

	// Idempotent. This runs on EVERY engine start, and the handlers usually
	// survive between them -- writing over an existing name fails EEXIST, so
	// the removal is what keeps the second start from erroring.
	if !strings.Contains(got, "echo -1 > /proc/sys/fs/binfmt_misc/"+h.Name) {
		t.Error("an existing registration is not removed first; the second engine " +
			"start of a session would fail with EEXIST")
	}
	rmAt := strings.Index(got, "echo -1 >")
	regAt := strings.Index(got, "/binfmt_misc/register")
	if rmAt < 0 || regAt < 0 || rmAt > regAt {
		t.Error("the removal must come before the registration")
	}

	// With the F flag the kernel opens the interpreter at registration time,
	// so a missing one fails there with an ENOENT that names nothing.
	if !strings.Contains(got, "test -x "+h.Interpreter) {
		t.Error("the interpreter is not checked before registering")
	}

	// The registration must reach /proc byte for byte. It is nothing but
	// backslash escapes; a shell that interpreted one would produce a handler
	// that registers cleanly and matches nothing.
	line := strings.TrimRight(h.Registration, "\n")
	if !strings.Contains(got, "'"+line+"'") {
		t.Errorf("the registration is not passed single-quoted and intact.\n got: %s", got)
	}
	if strings.Contains(line, "'") {
		t.Error("the registration contains a single quote, so single-quoting it in " +
			"the script is no longer safe")
	}
	if !strings.Contains(got, `printf '%s'`) {
		t.Error("the registration must be written with printf, not echo: echo " +
			"interprets backslash escapes in some shells and would corrupt the magic")
	}
}

// Two platforms, one exec. Three round trips to register two handlers would be
// most of a second on a path #398 is already about.
func TestEmulationScriptRegistersEveryHandlerInOnePass(t *testing.T) {
	var all []emulation.Handler
	for _, a := range emulation.Supported() {
		if h, ok := emulation.HandlerFor(a); ok {
			all = append(all, h)
		}
	}
	if len(all) < 2 {
		t.Skip("need two handlers to prove this")
	}
	got := emulationScript(all)
	for _, h := range all {
		if !strings.Contains(got, h.Interpreter) {
			t.Errorf("handler %s missing from the script", h.Arch)
		}
	}
	if n := strings.Count(got, "/binfmt_misc/register"); n != len(all) {
		t.Errorf("%d register writes for %d handlers", n, len(all))
	}
}

// countingWSL records whether anything was exec'd. Embedding the interface
// and leaving it nil is the same trick binfmt_test.go uses: any call other
// than Exec panics loudly rather than passing quietly.
type countingWSL struct {
	wsl.WSL
	execs   int
	scripts []string
}

func (c *countingWSL) Exec(_ context.Context, _, _ string, args ...string) (string, error) {
	c.execs++
	if len(args) > 0 {
		c.scripts = append(c.scripts, args[len(args)-1])
	}
	return "", nil
}

// Emulation off must cost NOTHING. It is the default, so this is the path
// almost every engine start takes, and each wsl round trip is ~165 ms on a
// warm distro -- on a start that #398 is already about.
func TestApplyEmulationDoesNothingWhenUnset(t *testing.T) {
	for _, v := range []string{"", "   ", ","} {
		f := &countingWSL{}
		p := &Provisioner{WSL: f}
		p.applyEmulation(context.Background(), Options{Distro: "d", EmulationPlatforms: v})
		if f.execs != 0 {
			t.Errorf("applyEmulation(%q) ran %d command(s); the default must be free",
				v, f.execs)
		}
	}
}

// And when it IS set, exactly one exec carries the whole set.
func TestApplyEmulationUsesOneExec(t *testing.T) {
	foreign := "arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "amd64"
	}
	f := &countingWSL{}
	p := &Provisioner{WSL: f}
	p.applyEmulation(context.Background(), Options{Distro: "d", EmulationPlatforms: foreign})
	if f.execs != 1 {
		t.Fatalf("ran %d commands, want 1", f.execs)
	}
	if !strings.Contains(f.scripts[0], "/binfmt_misc/register") {
		t.Errorf("the exec does not register anything:\n%s", f.scripts[0])
	}
}

// A hand-edited settings file must not stop the engine starting. `skrog
// config set` validates this key, so a bad value here means someone wrote the
// file directly -- and refusing to boot the engine over an optional feature
// would be a worse answer than booting without it.
func TestApplyEmulationSurvivesAnUnusableSetting(t *testing.T) {
	f := &countingWSL{}
	p := &Provisioner{WSL: f}
	p.applyEmulation(context.Background(), Options{Distro: "d", EmulationPlatforms: "linux/riscv64"})
	if f.execs != 0 {
		t.Errorf("an unusable setting still ran %d command(s)", f.execs)
	}
}

// Deregistration covers every architecture Skrog can register, not just the
// ones currently configured. Turning the key off and restarting must clean up
// what the previous setting left in the kernel -- and that kernel is shared
// with every other distro on the machine.
func TestRemoveEmulationCoversEveryHandler(t *testing.T) {
	f := &countingWSL{}
	p := &Provisioner{WSL: f}
	p.RemoveEmulation(context.Background(), Options{Distro: "d"})
	if f.execs != 1 {
		t.Fatalf("ran %d commands, want 1", f.execs)
	}
	for _, arch := range emulation.Supported() {
		h, _ := emulation.HandlerFor(arch)
		if !strings.Contains(f.scripts[0], h.Name) {
			t.Errorf("the cleanup does not remove %s", h.Name)
		}
	}
}
