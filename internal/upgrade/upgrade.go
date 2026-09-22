// Package upgrade answers "am I current?" across the three things that have
// to stay current together: the app, the engine, and the bundled docker CLI
// (#191).
//
// The convenience is the small part. The reason this exists is an ordering
// dependency nothing else surfaces: **the engines `skrog engine upgrade` can
// reach are pinned in the app binary's own manifest**. So a newer engine can
// require a newer app first, and without being told, someone on an older
// skrog runs `engine upgrade`, is told they are current, and is wrong.
//
// Nothing here auto-updates, polls in the background, or resolves "latest" at
// install time. The check runs only when a user asks for it, and it makes
// exactly one outbound request — to the releases API, for the app version.
// Every other comparison is local, because both manifests are compiled into
// the binary, which is why Offline still produces a useful answer.
package upgrade

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Status is where one stream stands.
type Status string

const (
	// StatusCurrent means nothing newer is reachable from this build.
	StatusCurrent Status = "current"
	// StatusAvailable means a newer version exists and can be had.
	StatusAvailable Status = "available"
	// StatusUnknown means the comparison could not be made — offline, an
	// unreachable API, or a version string that is not a release (a source
	// build reports "dev"). Distinct from current on purpose: "I could not
	// tell" must never be reported as "you are up to date".
	StatusUnknown Status = "unknown"
	// StatusNotInstalled means there is nothing to compare against yet.
	StatusNotInstalled Status = "not-installed"
)

// Stream is one of the three things that can be out of date.
type Stream struct {
	Name    string `json:"name"`
	Current string `json:"current,omitempty"`
	Latest  string `json:"latest,omitempty"`
	Status  Status `json:"status"`
	// How to move this stream forward, when it can move.
	Command string `json:"command,omitempty"`
	// Why the answer is what it is, when that is not obvious.
	Note string `json:"note,omitempty"`
}

// Report is the whole answer.
type Report struct {
	Streams []Stream `json:"streams"`
	// Notes carries what is true of the picture rather than of one stream —
	// above all the app/engine coupling.
	Notes []string `json:"notes,omitempty"`
	// Offline records that the network was not consulted, so a reader can
	// tell "nothing newer" from "did not look".
	Offline   bool   `json:"offline"`
	CheckedAt string `json:"checkedAt"`
}

// Installed is what this machine actually has, as opposed to what this build
// could install.
type Installed struct {
	// EngineVersion is empty when no engine is installed.
	EngineVersion string
	// EngineRef is the installed engine BUILD -- version plus rootfs revision,
	// "29.8.1-3". Empty on an install made before the ref was recorded, and on
	// one made with an explicit --rootfs-url, which is why the version above
	// stays and is used as the fallback.
	EngineRef string
	// CLIVersion is empty when the bundled docker CLI is not installed.
	CLIVersion string
}

// ReleaseLister reports the newest published app release. An interface so the
// check is testable without network, and so an air-gapped install can pass
// nil rather than a stub that pretends.
type ReleaseLister interface {
	LatestApp(ctx context.Context) (string, error)
}

// Checker assembles the report. Releases may be nil, which is offline.
type Checker struct {
	// App is this binary's stamped version ("dev" in a source build).
	App string
	// EngineLatest is the newest *published* engine in this build's manifest.
	// Empty when the build ships no published engine, which happens in a dev
	// build before the rootfs release is cut.
	EngineLatest string
	// EngineLatestRef is that engine as a ref, revision included.
	EngineLatestRef string
	// CLILatest is the docker CLI version this build bundles.
	CLILatest string

	Installed Installed
	Releases  ReleaseLister
	Now       func() time.Time
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Check builds the report. It never returns an error for "could not reach the
// releases API": that is a StatusUnknown app stream plus a note, because the
// engine and CLI answers are still worth having and are still correct.
func (c *Checker) Check(ctx context.Context) Report {
	rep := Report{
		Offline:   c.Releases == nil,
		CheckedAt: c.now().UTC().Format(time.RFC3339),
	}

	app := c.appStream(ctx, &rep)
	rep.Streams = append(rep.Streams, app, c.engineStream(), c.cliStream())

	// The coupling, stated only when it can actually bite. Saying it always
	// would train people to skip it.
	if app.Status == StatusAvailable {
		rep.Notes = append(rep.Notes,
			"the engines `skrog engine upgrade` can reach are pinned in this build's "+
				"manifest, so skrog "+app.Latest+" may offer newer engines than the "+
				c.engineLatestOrNone()+" listed here — upgrade skrog first, then re-check")
	}
	return rep
}

func (c *Checker) engineLatestOrNone() string {
	if c.EngineLatest == "" {
		return "engines"
	}
	return c.EngineLatest
}

func (c *Checker) appStream(ctx context.Context, rep *Report) Stream {
	s := Stream{Name: "app", Current: c.App, Status: StatusUnknown}

	if c.Releases == nil {
		s.Note = "not checked (offline)"
		return s
	}
	latest, err := c.Releases.LatestApp(ctx)
	if err != nil {
		// A failed check is not a failed command. Report it and carry on: the
		// two local streams are the ones a user can act on anyway.
		s.Note = "could not reach the releases API: " + err.Error()
		rep.Notes = append(rep.Notes,
			"the app check needs network; `--offline` skips it entirely")
		return s
	}
	s.Latest = latest

	if !IsRelease(c.App) {
		// A source build or a CI artifact parses as 0.0.0, which is below
		// every release. Calling that "an upgrade is available" would be
		// noise on exactly the builds a developer is testing.
		s.Note = "this is not a release build, so there is nothing to compare"
		return s
	}
	if Compare(latest, c.App) > 0 {
		s.Status = StatusAvailable
		// Deliberately not self-replacing: a running .exe cannot cleanly
		// replace itself on Windows, and once there is a signed distribution
		// channel it owns this path.
		s.Command = "download from https://github.com/wslkit/skrog/releases"
		return s
	}
	s.Status = StatusCurrent
	return s
}

func (c *Checker) engineStream() Stream {
	// Refs when both are known, versions otherwise (#481).
	//
	// The rootfs revision exists precisely because the tarball's contents can
	// change while ENGINE_VERSION stays put, so comparing bare versions asks
	// the wrong question: 29.8.1-1 and 29.8.1-3 are both "29.8.1", and only
	// one of them carries the emulator #462 shipped. Someone on the older one
	// asked the command named "upgrade" whether there was anything to do and
	// was told no.
	//
	// Compare already understands the revision suffix, so this is a change of
	// input rather than of comparison.
	cur, latest := c.Installed.EngineVersion, c.EngineLatest
	if c.Installed.EngineRef != "" && c.EngineLatestRef != "" {
		cur, latest = c.Installed.EngineRef, c.EngineLatestRef
	}
	s := Stream{Name: "engine", Current: cur, Latest: latest}

	switch {
	case c.Installed.EngineVersion == "":
		s.Status = StatusNotInstalled
		s.Command = "skrog install"
	case latest == "":
		s.Status = StatusUnknown
		s.Note = "this build's manifest lists no published engine"
	case Compare(latest, cur) > 0:
		s.Status = StatusAvailable
		s.Command = "skrog engine upgrade"
	default:
		s.Status = StatusCurrent
	}
	return s
}

func (c *Checker) cliStream() Stream {
	s := Stream{Name: "cli", Current: c.Installed.CLIVersion, Latest: c.CLILatest}

	switch {
	case c.Installed.CLIVersion == "":
		s.Status = StatusNotInstalled
		s.Note = "Skrog's bundled docker CLI is not installed (Docker Desktop's own CLI works too)"
		s.Command = "skrog cli install"
	case c.CLILatest == "":
		s.Status = StatusUnknown
	case Compare(c.CLILatest, c.Installed.CLIVersion) > 0:
		s.Status = StatusAvailable
		s.Command = "skrog cli install"
	default:
		s.Status = StatusCurrent
	}
	return s
}

// Available reports whether anything can move forward. It drives the exit
// code, so a script can gate on it.
func (r Report) Available() bool {
	for _, s := range r.Streams {
		if s.Status == StatusAvailable {
			return true
		}
	}
	return false
}

// WriteText renders the report for a human.
func (r Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range r.Streams {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, orDash(s.Current), describe(s))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range r.Streams {
		if s.Status == StatusAvailable && s.Command != "" {
			fmt.Fprintf(w, "\n%-8s %s", s.Name+":", s.Command)
		}
	}
	if r.Available() {
		fmt.Fprintln(w)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "\nnote: %s\n", n)
	}
	return nil
}

// describe is the right-hand column: what this stream's status means, in the
// words a reader needs rather than the enum value.
func describe(s Stream) string {
	switch s.Status {
	case StatusAvailable:
		return "-> " + s.Latest + " available"
	case StatusCurrent:
		return "(current)"
	case StatusNotInstalled:
		if s.Note != "" {
			return "(not installed — " + s.Note + ")"
		}
		return "(not installed)"
	default:
		if s.Note != "" {
			return "(unknown — " + s.Note + ")"
		}
		return "(unknown)"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// IsRelease reports whether a version string is a real release, and so worth
// comparing at all.
//
// Two builds are not: a source build reports "dev", and the release workflow
// stamps 0.0.0-ci on pull-request builds and 0.0.0-dryrun on a dry run. Those
// parse as 0.0.0, below every release — so without this check, everyone
// testing a CI artifact would be told an upgrade is available.
func IsRelease(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	return v != "" && v != "dev" && !strings.HasPrefix(v, "0.0.0")
}

// Compare orders the version strings this project actually uses: dotted
// numbers, optionally with a packaging revision ("29.8.0-1") and optionally
// with a leading v. It returns -1, 0 or 1.
//
// Hand-written rather than taken from a semver module because the dependency
// set here is four direct packages and staying that way is a feature; and
// because full semver pre-release ordering would be wrong for "-1", which is
// a revision that sorts *after* the bare version, not before it.
func Compare(a, b string) int {
	as, arev := split(a)
	bs, brev := split(b)

	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := at(as, i), at(bs, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case arev < brev:
		return -1
	case arev > brev:
		return 1
	}
	return 0
}

// split turns "v29.8.0-1" into ([29 8 0], 1). A non-numeric segment counts as
// 0, which makes an unparseable version sort low rather than panic — the same
// direction of failure as IsRelease.
func split(v string) (parts []int, rev int) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		rev = atoi(v[i+1:])
		v = v[:i]
	}
	for _, seg := range strings.Split(v, ".") {
		parts = append(parts, atoi(seg))
	}
	return parts, rev
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func at(xs []int, i int) int {
	if i < len(xs) {
		return xs[i]
	}
	return 0
}

// Action is one thing `skrog upgrade` will do, and the command it is.
type Action struct {
	Stream string   `json:"stream"`
	From   string   `json:"from,omitempty"`
	To     string   `json:"to"`
	Args   []string `json:"args"`
}

// Plan is what `skrog upgrade` would apply, in the order it would apply it.
//
// The app is never in it. A running .exe cannot cleanly replace itself on
// Windows, and once there is a signed distribution channel that channel owns
// the path; self-replacement earns its complexity last, if ever. It is
// reported instead, with where to get it.
//
// The CLI goes before the engine. It is a file copy that costs no downtime and
// depends on nothing else, where the engine upgrade stops and restarts the
// engine and takes minutes. Ordering it first means a failed engine upgrade
// leaves a machine with the CLI already current rather than nothing done, and
// the two are independent, so nothing is half-applied either way.
func (r Report) Plan() []Action {
	byName := map[string]Stream{}
	for _, s := range r.Streams {
		byName[s.Name] = s
	}

	var plan []Action
	add := func(name string, args ...string) {
		s, ok := byName[name]
		if !ok || s.Status != StatusAvailable {
			return
		}
		plan = append(plan, Action{Stream: name, From: s.Current, To: s.Latest, Args: args})
	}
	add("cli", "cli", "install")
	add("engine", "engine", "upgrade")
	return plan
}

// AppStream returns the app's stream, or a zero Stream when the report has
// none. Callers ask about the app specifically because it is the one stream
// `skrog upgrade` treats differently: applying it replaces the running binary
// (#309), so it is opt-in where the engine and CLI are not.
func (r Report) AppStream() Stream {
	for _, s := range r.Streams {
		if s.Name == "app" {
			return s
		}
	}
	return Stream{}
}
