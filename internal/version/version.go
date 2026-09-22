package version

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Report is the full component picture `skrog version` prints.
type Report struct {
	// App is Skrog's own version, stamped at build time.
	App string `json:"app"`
	// Engine describes the installed engine, from the install manifest.
	Engine EngineInfo `json:"engine"`
	// WSL is the host's WSL release, empty when it cannot be determined.
	WSL string `json:"wsl,omitempty"`
	// Docker lists every docker.exe on PATH, in resolution order.
	Docker []Binary `json:"docker"`
	// Context is the active docker context; empty when DOCKER_HOST overrides.
	Context string `json:"context"`
	// ContextSource says how Context was selected, which is half the answer
	// when someone's shell disagrees with their config.
	ContextSource string `json:"contextSource"`
	// APIVersion is the negotiated engine API version, when the engine
	// answered. Empty means it was not reachable, not that it is unknown.
	APIVersion string `json:"apiVersion,omitempty"`
	// Warnings flags conditions that make the setup behave unexpectedly.
	Warnings []string `json:"warnings,omitempty"`
}

// EngineInfo is what the install manifest recorded.
type EngineInfo struct {
	Installed bool `json:"installed"`
	// Backend is which engine this install serves. Always "distro" since the
	// session backend was removed (#451); reported anyway, because it is part
	// of the JSON contract and a reader that switches on it must keep
	// parsing.
	Backend string `json:"backend,omitempty"`
	Version string `json:"version,omitempty"`
	Distro  string `json:"distro,omitempty"`
	Rootfs  string `json:"rootfsSha256,omitempty"`
	// Ref is the engine BUILD -- "29.8.1-3" -- version plus rootfs revision.
	//
	// A new field rather than a change to rootfsSha256 or version: docs/cli-json.md
	// pins those, and a reader switching on either must keep working (#484).
	// It answers the question the SHA cannot -- two revisions can carry the
	// same dockerd and differ in what else is in the image.
	Ref string `json:"engineRef,omitempty"`
	// WSLAtInstall is the WSL version present when Skrog was installed;
	// a difference from WSL means the host was updated since.
	WSLAtInstall string `json:"wslAtInstall,omitempty"`
}

// analyze fills in Warnings from the collected facts. Kept separate so the
// rules are testable without touching a filesystem.
func (r *Report) analyze() {
	first := r.firstDocker()

	// PATH shadowing: the skrog context is selected, but a foreign docker.exe
	// runs first. Commands still work — that binary talks to whatever its
	// context says — but the user is not driving Skrog, which looks like a
	// Skrog fault (PLAN §05, v0.3 doctor check).
	//
	// This states the fact and stops. It used to append "put Skrog's bin
	// directory earlier, or use its full path", which is impossible when the
	// shadowing entry is on the machine PATH -- Windows resolves the whole
	// machine PATH before the whole user PATH, and Skrog writes only the user
	// half (#282). Saying the right thing requires knowing which PATH the entry
	// is on, which is a registry read, and analyze() is deliberately pure so the
	// rules stay testable without touching the machine. So the remedy lives
	// where that scope is already gathered: `skrog doctor` (#287).
	if r.Context == "skrog" && first != nil && first.Origin != OriginSkrog {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"the skrog context is active but %s (%s) resolves first on PATH; "+
				"run `skrog doctor` for what will move it",
			first.Path, OriginLabel(first.Origin)))
	}

	if len(r.Docker) == 0 {
		r.Warnings = append(r.Warnings,
			"no docker.exe found on PATH; install Skrog's bundled CLI or add it to PATH")
	}

	if r.Engine.Installed && r.Engine.WSLAtInstall != "" && r.WSL != "" &&
		r.Engine.WSLAtInstall != r.WSL {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"WSL was %s when Skrog was installed and is %s now; "+
				"run `skrog doctor` if the engine misbehaves",
			r.Engine.WSLAtInstall, r.WSL))
	}

	if !r.Engine.Installed {
		r.Warnings = append(r.Warnings, "no engine installed; run `skrog install`")
	}
}

func (r *Report) firstDocker() *Binary {
	for i := range r.Docker {
		if r.Docker[i].First {
			return &r.Docker[i]
		}
	}
	return nil
}

// WriteText renders the report for a terminal.
func (r *Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "skrog\t%s\n", r.App)
	if r.Engine.Installed {
		fmt.Fprintf(tw, "engine\t%s\t(distro %s)\n", orUnknown(r.Engine.Version), r.Engine.Distro)
		if r.Engine.Rootfs != "" {
			// The ref first, because it is the half a human can act on: it says
			// which revision is installed, and revisions are what an upgrade
			// moves between. The short SHA stays, because it is what a bug
			// report, --rootfs-sha256 and the manifest all speak, and it is the
			// only identifier an image installed from outside the manifest has
			// (#484).
			fmt.Fprintf(tw, "rootfs\t%s\n", RootfsLine(r.Engine.Ref, r.Engine.Rootfs))
		}
	} else {
		fmt.Fprintf(tw, "engine\tnot installed\n")
	}
	fmt.Fprintf(tw, "wsl\t%s\n", orUnknown(r.WSL))

	if r.Context != "" {
		fmt.Fprintf(tw, "context\t%s\t(%s)\n", r.Context, r.ContextSource)
	} else {
		fmt.Fprintf(tw, "context\t-\t(%s)\n", r.ContextSource)
	}
	if r.APIVersion != "" {
		fmt.Fprintf(tw, "api\t%s\n", r.APIVersion)
	}

	// Flush before the docker rows so they align among themselves and not with
	// everything above. A full path is far wider than a version, and one shared
	// column would push every annotation above it -- "(implicit default)" and
	// "(distro skrog-engine)" -- out to the width of the longest path.
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(r.Docker) == 0 {
		fmt.Fprintf(tw, "docker\tnone found on PATH\n")
	}
	for _, b := range r.Docker {
		marker := " "
		if b.First {
			marker = "*"
		}
		// Path in the column every row above uses for a version, origin after
		// it in parentheses. The origin used to sit in that column, so the word
		// "unknown" landed exactly where five versions had just been printed
		// and read as "skrog cannot tell what version your docker is" (#281).
		// It never meant that: it means this docker.exe matches no install
		// location we recognise, which for a hand-downloaded binary is correct
		// and not a problem.
		fmt.Fprintf(tw, "docker %s\t%s\t(%s)\n", marker, b.Path, OriginLabel(b.Origin))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(r.Docker) > 1 {
		fmt.Fprintln(w, "\n(* is the one that runs when you type `docker`)")
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
	return nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

// RootfsLine renders the installed image as "29.8.1-3  (f1be49d99b9a...)".
//
// Shared by `skrog version` and `skrog doctor`, which printed the bare SHA and
// nothing else — a value that identifies the bytes exactly and answers none of
// the questions anyone asks of it (#484).
//
// Without a ref it degrades to the SHA alone rather than inventing one. That
// case is real: an install from an explicit --rootfs-url has no revision to
// name, and saying so by omission beats printing the engine version twice.
func RootfsLine(ref, sha string) string {
	short := shortSHA(sha)
	if ref == "" {
		return short
	}
	return ref + "  (" + short + ")"
}

// OriginLabel renders an Origin for humans in the PARENTHETICAL position --
// "(skrog)", "(unrecognised install location)". OriginUnknown becomes a phrase
// rather than a bare word: "unknown" alone reads as a missing fact, when what
// it records is that the binary sits somewhere no installer we know puts one
// (#281). The --json field keeps the raw value -- that is a pinned contract.
//
// Exported because it was unexported, so `skrog doctor` grew its own bare
// rendering and kept showing the "unknown" #281 was filed about (#287).
// dockercli.ShadowAdvice has a separate renderer for the SUBJECT position of a
// sentence, where this phrasing would not read; each names the other.
func OriginLabel(o Origin) string {
	if o == OriginUnknown {
		return "unrecognised install location"
	}
	return string(o)
}
