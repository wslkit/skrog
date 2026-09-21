// Package emulation registers QEMU user-mode interpreters so the engine can
// run containers built for another CPU architecture (#462).
//
// # What this is for, and what it is not
//
// It is for `docker run --platform`. On a Windows-on-ARM machine most of
// Docker Hub is amd64-only, and without an interpreter those images fail with
// `exec format error` after a successful pull — an engine that can fetch an
// image and not start it.
//
// It is NOT for `docker buildx build --platform`, which already works with
// nothing installed: the `docker-container` driver builds inside an official
// BuildKit image, and those bundle the emulators. docs/docker-cli.md has the
// recipe and the measurement. Anyone reaching for this package to fix a build
// is solving the wrong problem.
//
// # Why it is opt-in
//
// binfmt_misc is a property of the KERNEL, and on WSL2 one kernel is shared by
// every distro in the utility VM. A handler Skrog registers changes how the
// user's Ubuntu executes foreign binaries too, and replaces any that
// tonistiigi/binfmt or a distro's qemu-user-static had put there.
//
// docs/docker-cli.md argued from that to "Skrog does not register handlers",
// on the same consent grounds as ~/.wslconfig, and concluded it "stays a thing
// you opt into". This package is that opt-in, not a reversal: nothing happens
// until someone sets emulation.platforms, and the reach is stated where they
// set it.
package emulation

import (
	_ "embed"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// The registration lines, byte for byte as the kernel wants them.
//
// Embedded rather than built from constants in Go so that there is ONE source
// of truth: guest/rootfs/emulation-test.sh reads these same files to prove in
// CI that a foreign container runs. A Go-side copy of the magic and mask would
// drift from the tested one silently, and the failure mode is a handler that
// registers cleanly and matches nothing.
//
// They are pinned to LF in .gitattributes. The kernel splits the line on ':'
// and takes the last field as the flags, so a trailing \r from a Windows
// checkout lands inside the flags and the write fails EINVAL, naming nothing.
var (
	//go:embed binfmt-amd64.reg
	regAmd64 string
	//go:embed binfmt-arm64.reg
	regArm64 string
)

// Handler is the binfmt_misc entry for one architecture.
type Handler struct {
	// Arch is the GOARCH this handler executes, e.g. "arm64".
	Arch string
	// Name is the entry's name under /proc/sys/fs/binfmt_misc.
	Name string
	// Interpreter is the qemu binary the rootfs ships for it.
	Interpreter string
	// Registration is the line written to .../binfmt_misc/register.
	Registration string
}

// handlers is every architecture Skrog can emulate, keyed by GOARCH.
//
// Two, deliberately. amd64 and arm64 are what real images are published for;
// the architectures below them are a list of things that exist rather than a
// list of things anyone runs on a Windows laptop. Adding one is a rootfs
// change (the emulator has to ship) plus an entry here — the interface does
// not move.
var handlers = map[string]Handler{
	"amd64": {
		Arch:         "amd64",
		Name:         "qemu-x86_64",
		Interpreter:  "/usr/bin/qemu-x86_64",
		Registration: regAmd64,
	},
	"arm64": {
		Arch:         "arm64",
		Name:         "qemu-aarch64",
		Interpreter:  "/usr/bin/qemu-aarch64",
		Registration: regArm64,
	},
}

// Supported lists the GOARCH values that can be emulated, sorted.
func Supported() []string {
	out := make([]string, 0, len(handlers))
	for a := range handlers {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// HandlerFor returns the handler for a GOARCH.
func HandlerFor(arch string) (Handler, bool) {
	h, ok := handlers[arch]
	return h, ok
}

// ErrNativeArch is returned for a request to emulate the host's own
// architecture.
//
// Its own error because it is the mistake most likely to be made and the most
// damaging to get wrong. A handler matching the host's own ELF type would
// intercept binaries the CPU executes directly and route every one of them
// through an emulator — a machine that still works, and is several times
// slower, for a reason nothing reports.
type ErrNativeArch struct{ Arch string }

func (e *ErrNativeArch) Error() string {
	return fmt.Sprintf("%s is this machine's own architecture; it needs no emulation.\n"+
		"  Registering an interpreter for it would route native binaries through\n"+
		"  QEMU and slow everything down for no benefit.", e.Arch)
}

// ErrUnsupportedArch is returned for an architecture Skrog ships no emulator
// for.
type ErrUnsupportedArch struct {
	Arch      string
	Supported []string
}

func (e *ErrUnsupportedArch) Error() string {
	return fmt.Sprintf("no emulator for %q; the rootfs ships %s",
		e.Arch, strings.Join(e.Supported, " and "))
}

// Normalize turns one user-written platform into a GOARCH.
//
// Accepts "linux/arm64" and "arm64" both. The docker spelling is what someone
// arrives with, because it is what they type at `--platform`; the bare GOARCH
// is what the rest of the codebase speaks. Refusing either would be pedantry
// about a value with one obvious meaning.
//
// A non-linux OS is refused rather than ignored: `windows/amd64` in this
// setting means someone expects Windows containers, and silently treating it
// as linux/amd64 would confirm a belief that is wrong in a bigger way.
func Normalize(platform string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(platform))
	if p == "" {
		return "", fmt.Errorf("empty platform")
	}
	if os, arch, ok := strings.Cut(p, "/"); ok {
		if os != "linux" {
			return "", fmt.Errorf("platform %q: only linux is supported; the engine runs Linux containers", platform)
		}
		p = arch
	}
	// Tolerate the spellings the other ecosystems use, since someone copying
	// from `uname -m` or an Alpine package name is not making a mistake.
	switch p {
	case "x86_64", "x86-64":
		p = "amd64"
	case "aarch64":
		p = "arm64"
	}
	if p == runtime.GOARCH {
		return "", &ErrNativeArch{Arch: p}
	}
	if _, ok := handlers[p]; !ok {
		return "", &ErrUnsupportedArch{Arch: p, Supported: Supported()}
	}
	return p, nil
}

// Parse turns a comma-separated setting into the handlers to register.
//
// Empty in, empty out and no error: "" is how emulation is off, which is the
// default and must not be an error condition.
//
// Duplicates collapse. The result is sorted, so the registration order does
// not depend on how the user happened to type the list — which matters because
// it ends up in log lines that get compared across runs.
func Parse(setting string) ([]Handler, error) {
	var archs []string
	seen := map[string]bool{}
	for _, field := range strings.Split(setting, ",") {
		if strings.TrimSpace(field) == "" {
			continue
		}
		arch, err := Normalize(field)
		if err != nil {
			return nil, err
		}
		if !seen[arch] {
			seen[arch] = true
			archs = append(archs, arch)
		}
	}
	sort.Strings(archs)

	out := make([]Handler, 0, len(archs))
	for _, a := range archs {
		out = append(out, handlers[a])
	}
	return out, nil
}
