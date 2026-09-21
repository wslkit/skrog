package emulation_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/emulation"
)

// The embedded registration lines are what the kernel parses, and a broken one
// registers cleanly and matches nothing. These assert the shape the kernel
// requires and the one field that decides which binaries a handler catches.
func TestRegistrationLinesAreWellFormed(t *testing.T) {
	for _, arch := range emulation.Supported() {
		h, ok := emulation.HandlerFor(arch)
		if !ok {
			t.Fatalf("Supported() lists %q with no handler", arch)
		}
		t.Run(arch, func(t *testing.T) {
			line := strings.TrimRight(h.Registration, "\n")

			if strings.Contains(h.Registration, "\r") {
				t.Error("the line carries a CR. The kernel takes the last ':' field as " +
					"the flags, so it would land inside them and the write fails EINVAL. " +
					"*.reg must be LF; see .gitattributes")
			}

			// :name:type:offset:magic:mask:interpreter:flags
			parts := strings.Split(line, ":")
			if len(parts) != 8 {
				t.Fatalf("want 8 colon-separated fields, got %d: %q", len(parts), line)
			}
			if parts[1] != h.Name {
				t.Errorf("name field %q != Handler.Name %q", parts[1], h.Name)
			}
			if parts[2] != "M" {
				t.Errorf("type field %q, want M (magic match)", parts[2])
			}
			if parts[6] != h.Interpreter {
				t.Errorf("interpreter field %q != Handler.Interpreter %q", parts[6], h.Interpreter)
			}

			// F is the whole trick: it loads the interpreter into the kernel at
			// registration time, so it still resolves inside a container whose
			// mount namespace has no /usr/bin/qemu-*. Without it the
			// registration succeeds and every emulated exec fails with ENOENT.
			if !strings.Contains(parts[7], "F") {
				t.Errorf("flags %q lack F (fix-binary); emulation would fail inside containers", parts[7])
			}

			// e_machine sits at bytes 18-19 of an ELF header, little-endian.
			// This is the field that decides what the handler catches, and
			// getting it wrong is how a handler matches everything or nothing.
			wantMachine := map[string]string{
				"amd64": `\x3e\x00`,
				"arm64": `\xb7\x00`,
			}[arch]
			if !strings.HasSuffix(parts[4], wantMachine) {
				t.Errorf("magic %q does not end in the %s e_machine %q",
					parts[4], arch, wantMachine)
			}
		})
	}
}

// Nothing may ship a handler for the architecture it runs on.
func TestTheHostArchIsNeverEmulated(t *testing.T) {
	if _, err := emulation.Normalize(runtime.GOARCH); err == nil {
		t.Fatalf("Normalize(%q) accepted this machine's own architecture", runtime.GOARCH)
	} else {
		var native *emulation.ErrNativeArch
		if !errors.As(err, &native) {
			t.Errorf("want ErrNativeArch so callers can say something useful, got %T", err)
		}
	}
}

func TestNormalize(t *testing.T) {
	// The foreign architecture, whichever host this runs on -- so the table is
	// meaningful on the arm64 CI runner too rather than vacuously skipped.
	foreign, foreignLong, foreignAlt := "arm64", "linux/arm64", "aarch64"
	if runtime.GOARCH == "arm64" {
		foreign, foreignLong, foreignAlt = "amd64", "linux/amd64", "x86_64"
	}

	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
		why     string
	}{
		{in: foreign, want: foreign, why: "the bare GOARCH the codebase speaks"},
		{in: foreignLong, want: foreign, why: "the docker --platform spelling someone arrives with"},
		{in: foreignAlt, want: foreign, why: "what uname -m and Alpine call it"},
		{in: "  " + foreignLong + "  ", want: foreign, why: "trimmed"},
		{in: strings.ToUpper(foreign), want: foreign, why: "case folded"},
		{in: "", wantErr: true, why: "empty is not a platform"},
		{in: "windows/amd64", wantErr: true,
			why: "a non-linux OS means the user expects something this cannot give them"},
		{in: "linux/riscv64", wantErr: true, why: "no emulator ships for it"},
		{in: "nonsense", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := emulation.Normalize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Normalize(%q) = %q, want an error\n  %s", tc.in, got, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q): %v\n  %s", tc.in, err, tc.why)
			}
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q\n  %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// Empty is how emulation is OFF, which is the default. It must not be an
// error, or every install without the setting logs one.
func TestParseEmptyIsOffAndNotAnError(t *testing.T) {
	for _, in := range []string{"", "   ", ",", " , ,"} {
		got, err := emulation.Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Errorf("Parse(%q) = %v, want nothing", in, got)
		}
	}
}

func TestParseDeduplicatesAndSorts(t *testing.T) {
	foreign := "arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "amd64"
	}
	got, err := emulation.Parse(foreign + ", linux/" + foreign + " ," + foreign)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Arch != foreign {
		t.Errorf("Parse = %v, want exactly one %s handler", got, foreign)
	}
}

// One bad entry fails the whole setting rather than being dropped. A list
// where two of three architectures quietly did nothing is worse than a
// refusal: the user believes they asked for something they did not get.
func TestParseRejectsTheWholeListOnOneBadEntry(t *testing.T) {
	foreign := "arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "amd64"
	}
	if _, err := emulation.Parse(foreign + ",linux/riscv64"); err == nil {
		t.Error("a list containing an unsupported platform was accepted")
	}
}
