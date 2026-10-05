// Package provision installs and removes the Skrog engine distro: preflight,
// checksum-verified rootfs download, wsl --import, engine start, clean removal.
package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/wsl"
)

// MinWSLVersion is the oldest WSL release Skrog is tested against. Older
// releases may work, but several doctor remedies (mirrored networking, DNS
// tunneling) do not exist there, so it is reported rather than assumed.
const MinWSLVersion = "2.0.0"

// Report is the outcome of Preflight: whether install can proceed, and what the
// user should do if not.
type Report struct {
	// OK is true when installation can proceed.
	OK bool
	// Problems block installation. Each carries an actionable remedy.
	Problems []Problem
	// Warnings do not block installation but limit what will work.
	Warnings []Problem
	// Status is what WSL reported, for `skrog version` and doctor output.
	Status wsl.Status
	// ExistingDistro is set when the target distro is already registered.
	ExistingDistro *wsl.Distro
}

// Problem is a preflight finding plus the fix for it. The remedy is the point:
// PLAN §05 requires preflight to print exact instructions rather than
// attempting elevation or failing with a bare error.
type Problem struct {
	// Summary states what is wrong, in one line.
	Summary string
	// Remedy states what the user should do, concretely.
	Remedy string
}

func (p Problem) String() string { return p.Summary + "\n  fix: " + p.Remedy }

// Preflight checks the host before anything is downloaded or written.
func (p *Provisioner) Preflight(ctx context.Context, opts Options) (Report, error) {
	opts = opts.withDefaults()

	var r Report
	status, err := p.wsl().Status(ctx)
	if err != nil {
		return r, fmt.Errorf("preflight: querying WSL: %w", err)
	}
	r.Status = status

	if !status.Installed {
		// Deliberately not attempting to enable WSL: it needs elevation and a
		// reboot, and doing it silently on someone's machine is not our call.
		r.Problems = append(r.Problems, Problem{
			Summary: "WSL2 is not installed or not enabled",
			Remedy: "run `wsl --install --no-distribution` in an elevated prompt, reboot, " +
				"then run `skrog install` again. If that fails, enable the " +
				"'Virtual Machine Platform' and 'Windows Subsystem for Linux' Windows " +
				"features and confirm virtualization is on in firmware.",
		})
		return r, nil
	}

	if status.Version == "" {
		// `wsl --version` postdates the Store release; its absence means the
		// inbox WSL, which lacks the networking modes doctor relies on.
		r.Warnings = append(r.Warnings, Problem{
			Summary: "cannot determine the WSL version (`wsl --version` unsupported)",
			Remedy: "install the Store version of WSL (`wsl --update`) for mirrored " +
				"networking and DNS tunneling, which several VPN fixes need.",
		})
	} else if older, cmpErr := versionOlder(status.Version, MinWSLVersion); cmpErr == nil && older {
		r.Warnings = append(r.Warnings, Problem{
			Summary: fmt.Sprintf("WSL %s is older than the tested minimum %s",
				status.Version, MinWSLVersion),
			Remedy: "run `wsl --update` to get a supported release.",
		})
	}

	if status.DefaultVersion == 1 {
		// Only the default is wrong; Skrog imports with --version 2 anyway, so
		// this is a warning rather than a blocker.
		r.Warnings = append(r.Warnings, Problem{
			Summary: "the default WSL version is 1",
			Remedy: "Skrog imports its own distro as WSL 2 regardless, but " +
				"`wsl --set-default-version 2` avoids surprises with your other distros.",
		})
	}

	distros, err := p.wsl().List(ctx)
	if err != nil {
		return r, fmt.Errorf("preflight: listing distros: %w", err)
	}
	for i := range distros {
		if strings.EqualFold(distros[i].Name, opts.Distro) {
			d := distros[i]
			r.ExistingDistro = &d
			// Naming `engine upgrade` first is deliberate: someone typing
			// `install` on a machine that already has one is usually after a
			// newer engine, and the other two answers throw their data away
			// or leave them running two engines by accident.
			r.Problems = append(r.Problems, Problem{
				Summary: fmt.Sprintf("distro %q is already registered", opts.Distro),
				Remedy: fmt.Sprintf("`skrog engine upgrade` moves that engine to another\n"+
					"         version, keeping your images, containers and volumes.\n"+
					"         To start over instead, run `skrog uninstall` first. To run a\n"+
					"         second engine alongside this one, install under another name\n"+
					"         with `--distro`.\n"+
					"         Its data lives in that distro, so %q is never overwritten\n"+
					"         implicitly.", opts.Distro),
			})
		}
	}

	r.OK = len(r.Problems) == 0
	return r, nil
}

// WSLTooOld reports whether a WSL version string is older than the tested
// minimum. Unparseable or empty input is treated as not-too-old, so callers
// (doctor, #61) skip the check rather than guess.
func WSLTooOld(version string) bool {
	older, err := versionOlder(version, MinWSLVersion)
	return err == nil && older
}

// MinVirtiofsWSLVersion is the first WSL release that honours `virtiofs=true`
// in ~/.wslconfig. Older releases ignore the key silently. It reached a
// stable WSL in 3.0 (#523); 2.9 was pre-release only.
const MinVirtiofsWSLVersion = "2.9.0"

// WSLSupportsVirtiofs reports whether a WSL version honours the virtiofs key.
// Unparseable or empty input is false, so a caller offering the switch stays
// quiet rather than offering something that might be ignored.
func WSLSupportsVirtiofs(version string) bool {
	older, err := versionOlder(version, MinVirtiofsWSLVersion)
	return err == nil && !older
}

// versionOlder compares dotted numeric versions. Returns an error for input it
// cannot parse, so callers can skip the check instead of guessing.
func versionOlder(got, min string) (bool, error) {
	g, err := parseVersion(got)
	if err != nil {
		return false, err
	}
	m, err := parseVersion(min)
	if err != nil {
		return false, err
	}
	for i := 0; i < len(g) && i < len(m); i++ {
		if g[i] != m[i] {
			return g[i] < m[i], nil
		}
	}
	return len(g) < len(m), nil
}

func parseVersion(v string) ([]int, error) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("unparseable version %q", v)
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
			return nil, fmt.Errorf("unparseable version %q", v)
		}
		out = append(out, n)
	}
	return out, nil
}
