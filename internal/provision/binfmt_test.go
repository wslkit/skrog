package provision

import (
	"context"
	"testing"

	"github.com/wslkit/skrog/internal/wsl"
)

// binfmtWSL answers one Exec with canned output. The interface is embedded and
// left nil on purpose: BinfmtHandlers must only ever call Exec, and anything
// else panics loudly rather than passing quietly.
type binfmtWSL struct {
	wsl.WSL
	out string
	err error
}

func (b binfmtWSL) Exec(context.Context, string, string, ...string) (string, error) {
	return b.out, b.err
}

// binfmt_misc holds more than architecture emulators. The first cut of this
// excluded the names we knew (register, status, WSLInterop) and let everything
// else through, which made doctor announce "the default builder can emulate:
// python3.14" on a machine whose Ubuntu had registered a Python handler.
// Confidently wrong, and only caught by running it against a real distro.
func TestBinfmtHandlersOnlyCountsEmulators(t *testing.T) {
	p := &Provisioner{WSL: binfmtWSL{out: "WSLInterop\npython3.14\nqemu-aarch64\nregister\nstatus\n"}}

	got := p.BinfmtHandlers(context.Background(), Options{Distro: "d"})

	if len(got) != 1 || got[0] != "qemu-aarch64" {
		t.Errorf("BinfmtHandlers = %v; want only qemu-aarch64", got)
	}
}

func TestBinfmtHandlersEmptyTable(t *testing.T) {
	p := &Provisioner{WSL: binfmtWSL{out: "WSLInterop\nregister\nstatus\n"}}
	if got := p.BinfmtHandlers(context.Background(), Options{Distro: "d"}); len(got) != 0 {
		t.Errorf("BinfmtHandlers = %v; want none", got)
	}
}

func TestBinfmtHandlersSorted(t *testing.T) {
	p := &Provisioner{WSL: binfmtWSL{out: "qemu-riscv64\nqemu-aarch64\nqemu-arm\n"}}
	got := p.BinfmtHandlers(context.Background(), Options{Distro: "d"})

	want := []string{"qemu-aarch64", "qemu-arm", "qemu-riscv64"}
	if len(got) != len(want) {
		t.Fatalf("BinfmtHandlers = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("BinfmtHandlers = %v, want %v", got, want)
		}
	}
}

// BuildKit's in-container emulators carry a prefix; they show up when someone
// inspects a builder container rather than the host, but the parser should not
// silently drop them.
func TestBinfmtHandlersAcceptsBuildkitPrefix(t *testing.T) {
	p := &Provisioner{WSL: binfmtWSL{out: "buildkit-qemu-aarch64\nWSLInterop\n"}}
	if got := p.BinfmtHandlers(context.Background(), Options{Distro: "d"}); len(got) != 1 {
		t.Errorf("BinfmtHandlers = %v; want the buildkit-prefixed handler", got)
	}
}

// An unreadable table is "no emulation", not a crash: doctor reports it as the
// native-only case, which is the same thing a user sees.
func TestBinfmtHandlersOnError(t *testing.T) {
	p := &Provisioner{WSL: binfmtWSL{err: context.DeadlineExceeded}}
	if got := p.BinfmtHandlers(context.Background(), Options{Distro: "d"}); got != nil {
		t.Errorf("BinfmtHandlers = %v; want nil on a failed probe", got)
	}
}
