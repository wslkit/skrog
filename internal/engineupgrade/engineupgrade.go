// Package engineupgrade replaces the engine binaries in an installed distro,
// reversibly, without touching the data beside them (#65).
//
// An engine security patch should not wait for an app release, and it should
// not cost you your images. Both fall out of one decision: an upgrade swaps
// the *binaries* (dockerd, containerd, runc, buildkitd and friends) out of a
// checksum-verified rootfs tarball, and leaves the filesystem they live on
// alone. /var/lib/docker -- every image, container and volume -- is never
// exported, re-imported or migrated, because it never moves.
//
// The alternative, importing the new rootfs side by side and swapping distros,
// reads better on paper and loses the data: a fresh rootfs has an empty
// /var/lib/docker, so the swap would need a full export/import of the engine's
// entire data set (tens of GB) or a separate data disk, which is a layout
// change to the install itself. That is worth doing deliberately, not as a
// side effect of an upgrade command.
//
// Every upgrade is reversible because the *source* is reversible: rollback is
// an upgrade whose target is the version recorded before the last one. The
// tarballs stay in the state directory's rootfs cache, verified, so rolling
// back re-extracts rather than re-downloads -- and if the cache was cleared,
// the download is the same checksum-mandatory path as an install.
//
// Failure is handled the same way: if the new engine does not come back, the
// previous binaries go back in and the engine is started again before the
// error is reported. An upgrade that breaks the engine leaves a working
// engine.
package engineupgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// EngineBinaries are the files an upgrade replaces, as they appear in the
// rootfs tarball. Deliberately an explicit list rather than "everything in
// usr/local/bin": the tarball also carries Alpine's own tools, and an upgrade
// that quietly replaced those would be a distro upgrade wearing an engine
// upgrade's clothes.
var EngineBinaries = []string{
	"dockerd",
	"docker-proxy",
	"docker-init",
	"containerd",
	"containerd-shim-runc-v2",
	"ctr",
	"runc",
	"buildkitd",
	"buildctl",
	// Either name: a tarball cut before the Skrog rename carries the old one
	// (see agentBinaries in internal/provision). The extractor skips whichever
	// is absent.
	"skrog-agent",
	"hawser-agent",
	// Present from rootfs 29.7.2-4: moby registers its NVIDIA GPU driver at
	// daemon start only when this exists (#139), so an upgrade that skipped it
	// would not deliver the feature the rootfs revision was cut for. Absent
	// from older tarballs, which the extractor simply skips.
	"nvidia-cdi-hook",
}

// distroBinDir is where the rootfs puts them, and where they must land.
const distroBinDir = "/usr/local/bin"

// EngineEmulators are the foreign-architecture interpreters (#462). They are
// engine payload in every sense that matters -- `docker run --platform` works
// or does not work depending on whether they are there -- but they live in
// /usr/bin rather than distroBinDir, and the extractor took one directory.
//
// So `skrog engine upgrade` moved an install to 29.8.1-3, the revision cut to
// carry them, and did not carry them: the handler registration then failed on
// `test -x /usr/bin/qemu-aarch64` and the feature was unreachable for everyone
// who had not installed fresh (#479). Same reasoning as nvidia-cdi-hook above,
// which is in the list for exactly this reason -- an upgrade that skips what a
// revision was cut for has not delivered that revision.
//
// Each rootfs carries exactly one, for the architecture it is NOT, so the
// extractor takes whichever is present and skips the other, as with the agent.
//
// The matching /usr/lib/binfmt.d/*.conf is deliberately NOT carried. Skrog
// composes the registration itself from internal/emulation/*.reg and writes it
// to /proc directly, and this distro runs without systemd, so nothing in the
// engine ever reads that file. Copying it would be cargo cult.
var EngineEmulators = []string{
	"qemu-aarch64",
	"qemu-x86_64",
}

// distroEmulatorDir is where the rootfs puts the interpreters, and where the
// registration in internal/emulation/*.reg names them.
const distroEmulatorDir = "/usr/bin"

// Distro is the slice of wsl.WSL this package needs.
type Distro interface {
	Exec(ctx context.Context, distro, user string, args ...string) (string, error)
}

// Fetcher downloads a rootfs tarball to dest and verifies its SHA-256, the
// same checksum-mandatory path an install uses.
type Fetcher interface {
	FetchRootfs(ctx context.Context, url, wantSHA, dest string) error
}

// Engine describes an upgrade target: a version label and the rootfs carrying
// it. Ref is what gets recorded and reported -- the revisioned label
// ("29.7.2-4") when there is one, because the rootfs revision is the unit of
// upgrade, not the dockerd version.
type Engine struct {
	Ref    string
	URL    string
	SHA256 string
}

// Options is one upgrade request.
type Options struct {
	StateDir string
	Distro   string
	// Target is where to go.
	Target Engine
	// From is the engine ref currently installed, recorded so a rollback knows
	// where to return to. Empty is allowed (an install that predates this
	// bookkeeping); rollback then has nothing to offer, which it says.
	From string
	// DryRun reports the plan and changes nothing.
	DryRun bool
}

// Report is the outcome, shaped for the human and --json alike.
type Report struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Replaced lists the binaries actually swapped.
	Replaced []string `json:"replaced,omitempty"`
	// EngineVersion is what the new dockerd reports about itself -- the check
	// that the swap took, rather than an assumption that it did.
	EngineVersion string `json:"engineVersion,omitempty"`
	// RolledBack is true when the new engine failed to come back and the
	// previous binaries were restored.
	RolledBack bool     `json:"rolledBack"`
	DryRun     bool     `json:"dryRun"`
	Steps      []string `json:"steps,omitempty"`
}

// Runner performs upgrades.
type Runner struct {
	WSL     Distro
	Fetcher Fetcher
	// Stop and Start bracket the swap; both are required.
	Stop  func(ctx context.Context) error
	Start func(ctx context.Context) error
	// Healthy reports whether the engine answers after a start. Required: it
	// is what turns "the files were copied" into "the engine works".
	Healthy func(ctx context.Context) bool
	// Restore is called with the previous engine when a rollback is needed;
	// nil means an upgrade cannot self-heal and reports the failure as-is.
	Restore func(ctx context.Context, previous string) error
}

// ErrSameVersion reports an upgrade to the version already installed.
type ErrSameVersion struct{ Ref string }

func (e *ErrSameVersion) Error() string {
	return fmt.Sprintf("the engine is already %s", e.Ref)
}

// ErrNoPrevious reports a rollback with nothing recorded to roll back to.
type ErrNoPrevious struct{}

func (*ErrNoPrevious) Error() string {
	return "no previous engine is recorded, so there is nothing to roll back to " +
		"(a rollback point is recorded by `skrog engine upgrade`)"
}

// Run performs the upgrade (or reports the plan under DryRun).
func (r *Runner) Run(ctx context.Context, opts Options) (Report, error) {
	rep := Report{From: opts.From, To: opts.Target.Ref, DryRun: opts.DryRun}
	if opts.Distro == "" || opts.StateDir == "" {
		return rep, errors.New("engineupgrade: distro and state dir are required")
	}
	if opts.Target.Ref == "" {
		return rep, errors.New("engineupgrade: no target engine")
	}
	if opts.From != "" && opts.From == opts.Target.Ref {
		return rep, &ErrSameVersion{Ref: opts.From}
	}

	tarball := filepath.Join(opts.StateDir, "rootfs", path.Base(opts.Target.URL))
	if opts.DryRun {
		rep.Steps = []string{
			"fetch and verify " + opts.Target.URL,
			fmt.Sprintf("extract %d engine binaries and the emulator, if the image carries one",
				len(EngineBinaries)),
			"stop the engine",
			"replace them in " + opts.Distro + ":" + distroBinDir +
				" and " + distroEmulatorDir,
			"start the engine and confirm it answers",
		}
		if opts.From != "" {
			rep.Steps = append(rep.Steps,
				"on failure: restore "+opts.From+" and start the engine again")
		}
		return rep, nil
	}

	// 1. The rootfs is the source of truth for what an engine version is, and
	//    it is never used unverified -- same path as an install.
	if err := r.Fetcher.FetchRootfs(ctx, opts.Target.URL, opts.Target.SHA256, tarball); err != nil {
		return rep, err
	}
	rep.Steps = append(rep.Steps, "fetched and verified the rootfs")

	// 2. Extract only the engine binaries, to a staging dir on the host --
	// removed on the way out. It is ~250 MB of binaries that are redundant the
	// moment they are copied in, and the verified tarball beside it is the
	// durable source a rollback re-extracts from.
	staging := filepath.Join(opts.StateDir, "engine-staging", opts.Target.Ref)
	defer os.RemoveAll(staging)
	extracted, err := ExtractBinaries(tarball, staging)
	if err != nil {
		return rep, err
	}
	if len(extracted) == 0 {
		return rep, fmt.Errorf("engineupgrade: %s carries none of the engine binaries", tarball)
	}
	rep.Steps = append(rep.Steps, fmt.Sprintf("extracted %d binaries", len(extracted)))

	// 3. Stop, swap, start. The engine must be down for the swap: replacing a
	//    running dockerd's file is allowed on Linux but leaves the old code in
	//    memory, which is the worst of both worlds -- a version report that
	//    disagrees with what is running.
	if err := r.Stop(ctx); err != nil {
		return rep, fmt.Errorf("engineupgrade: stopping the engine: %w", err)
	}
	if err := r.install(ctx, opts, staging, extracted); err != nil {
		// The copy failed mid-way; put the engine back the way it was.
		return rep, r.selfHeal(ctx, opts, &rep, err)
	}
	rep.Replaced = extracted
	rep.Steps = append(rep.Steps, "replaced the engine binaries")

	if err := r.Start(ctx); err != nil {
		return rep, r.selfHeal(ctx, opts, &rep,
			fmt.Errorf("the new engine did not start: %s", brief(err)))
	}
	if r.Healthy != nil && !r.Healthy(ctx) {
		return rep, r.selfHeal(ctx, opts, &rep, errors.New("the new engine started but does not answer"))
	}

	// 4. Confirm the swap took, rather than trusting it.
	//
	// The previous version of this step did not confirm anything: it dropped
	// the probe's error and compared the result against nothing, so a failed
	// probe produced the step string "engine is running " with a trailing
	// space, an upgrade that reported success, and a manifest left describing
	// the old engine under the new ref (#241). An error swallowed under a
	// comment claiming verification is the same shape as the agent-start bug.
	v, verr := r.engineVersion(ctx, opts)
	switch {
	case verr != nil:
		// Not fatal: the binaries are in place and the engine answered the
		// readiness check above, so the upgrade did happen. What is unknown is
		// whether it is the version we asked for — say that, rather than
		// printing a blank.
		rep.Steps = append(rep.Steps, "engine is running, but its version could not be read: "+verr.Error())
	case opts.Target.Ref != "" && !strings.Contains(v, versionOf(opts.Target.Ref)):
		rep.EngineVersion = v
		rep.Steps = append(rep.Steps,
			"engine reports "+v+", which does not look like the requested "+opts.Target.Ref)
	default:
		rep.EngineVersion = v
		rep.Steps = append(rep.Steps, "engine is running "+v)
	}
	return rep, nil
}

// versionOf strips a ref down to the version it names, so a reported
// "29.8.0" can be recognised in a ref spelled "29.8.0" or "v29.8.0".
func versionOf(ref string) string { return strings.TrimPrefix(ref, "v") }

// selfHeal restores the previous engine after a failed upgrade and returns the
// error to report -- the original failure, with the outcome of the restore
// appended, because "the upgrade failed" and "and you are now without an
// engine" are very different messages.
func (r *Runner) selfHeal(ctx context.Context, opts Options, rep *Report, cause error) error {
	if opts.From == "" || r.Restore == nil {
		return fmt.Errorf("engineupgrade: %w (no rollback point recorded; "+
			"`skrog engine install --to <ref>` or a snapshot restore is the way back)", cause)
	}
	if err := r.Restore(ctx, opts.From); err != nil {
		return fmt.Errorf("engineupgrade: %w -- AND restoring %s failed: %v; "+
			"the engine may be down (`skrog status`, then `skrog engine rollback`)",
			cause, opts.From, err)
	}
	rep.RolledBack = true
	rep.Steps = append(rep.Steps, "restored "+opts.From+" after the failure")
	return fmt.Errorf("engineupgrade: %w; rolled back to %s, which is running", cause, opts.From)
}

// install copies the staged binaries into the distro. The copy is done inside
// the distro from the automounted host path, one command, so a partial copy is
// as unlikely as it can be made without a transaction.
func (r *Runner) install(ctx context.Context, opts Options, staging string, names []string) error {
	mnt, err := hostPathInDistro(staging)
	if err != nil {
		return err
	}
	// install(1) writes to a temp name and renames, so a binary is never
	// half-written even if the copy is interrupted.
	var b strings.Builder
	b.WriteString("set -e; ")
	for _, n := range names {
		// Each file is named before it is copied, so a failure says WHICH one
		// it died on (#483). `set -e` stops at the first error, so the last
		// name in the output is the one that failed. Exec returns combined
		// output, so this is captured; and on the path that works nobody sees
		// it, because nobody sees a successful upgrade's transcript.
		fmt.Fprintf(&b, "echo %q; ", "installing "+n+" -> "+destDirFor(n))
		fmt.Fprintf(&b, "install -m 0755 %q %q; ", mnt+"/"+n, destDirFor(n)+"/"+n)
	}
	out, err := r.WSL.Exec(ctx, opts.Distro, "root", "sh", "-c", b.String())
	if err != nil {
		return fmt.Errorf("copying binaries into %s: %w: %s", opts.Distro, err, describeOutput(out))
	}
	return nil
}

// describeOutput renders a command's output for an error message.
//
// Silence is a fact worth stating. The failure that prompted #483 ended in
// `exit status 1: ` with nothing after the colon, which reads like a truncated
// message and sent the reader looking for the missing half. There was no
// missing half: the command said nothing, which is itself the clue, because it
// means the shell never got far enough to complain.
func describeOutput(out string) string {
	if s := strings.TrimSpace(out); s != "" {
		return s
	}
	return "(the command produced no output, so it likely never ran)"
}

// engineVersion asks the newly installed dockerd what it is, which is the only
// evidence that the swap did what it claimed.
func (r *Runner) engineVersion(ctx context.Context, opts Options) (string, error) {
	out, err := r.WSL.Exec(ctx, opts.Distro, "root", "dockerd", "--version")
	if err != nil {
		return "", err
	}
	// "Docker version 29.7.2, build ..." -> "29.7.2"
	f := strings.Fields(out)
	for i, w := range f {
		if w == "version" && i+1 < len(f) {
			return strings.TrimSuffix(f[i+1], ","), nil
		}
	}
	return strings.TrimSpace(out), nil
}

// matchEngineBinary returns the entry of EngineBinaries equal to base, or
// false. The returned string is the constant, not the archive's.
func matchEngineBinary(base string) (string, bool) {
	for _, n := range EngineBinaries {
		if n == base {
			return n, true
		}
	}
	return "", false
}

// matchEngineFile resolves an archive entry to the name an upgrade knows it
// by, given the directory it came from. Two directories now, not one (#479),
// and the pairing is checked: an entry called `dockerd` under /usr/bin is not
// the engine, and an interpreter under /usr/local/bin is not one either.
//
// As with matchEngineBinary, the returned name is the constant rather than the
// archive's string, so the path that reaches filepath.Join is provably ours.
func matchEngineFile(dir, base string) (string, bool) {
	switch dir {
	case strings.TrimPrefix(distroBinDir, "/"):
		return matchEngineBinary(base)
	case strings.TrimPrefix(distroEmulatorDir, "/"):
		for _, n := range EngineEmulators {
			if n == base {
				return n, true
			}
		}
	}
	return "", false
}

// destDirFor says where a staged file has to land. Staging is flat and keyed
// by name, which is safe only because the two sets do not overlap -- asserted
// by TestEngineFileSetsDoNotOverlap rather than assumed.
func destDirFor(name string) string {
	for _, n := range EngineEmulators {
		if n == name {
			return distroEmulatorDir
		}
	}
	return distroBinDir
}

// ExtractBinaries pulls the engine binaries out of a rootfs tarball into dest,
// returning the names it found, sorted. Everything else in the archive is
// skipped: this is an engine upgrade, not a distro upgrade.
func ExtractBinaries(tarball, dest string) ([]string, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}

	f, err := os.Open(tarball)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", tarball, err)
	}
	defer gz.Close()

	var found []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", tarball, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// Entries are "usr/local/bin/dockerd" (or "./usr/local/bin/dockerd"),
		// and since #479 also "usr/bin/qemu-aarch64".
		clean := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		// As in internal/upgrade: resolve to the constant from EngineBinaries
		// or EngineEmulators rather than reusing the tar header's string, so
		// the path we write is provably ours. path.Clean, the exact-directory
		// check inside matchEngineFile and path.Base already prevented an
		// escape; this stops the tainted name reaching filepath.Join at all
		// (CodeQL go/zipslip).
		name, ok := matchEngineFile(path.Dir(clean), path.Base(clean))
		if !ok {
			continue
		}
		if err := writeFile(filepath.Join(dest, name), tr); err != nil {
			return nil, err
		}
		found = append(found, name)
	}
	sort.Strings(found)
	return found, nil
}

func writeFile(dest string, r io.Reader) error {
	// Written to a temp name and renamed so an interrupted extraction cannot
	// leave a truncated binary that a later run would copy into the engine.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".stage-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// hostPathInDistro turns a Windows path into the /mnt/<drive> path the distro
// sees. Kept here rather than reaching for internal/winpath because that
// package translates bind specs for the docker API; this is one plain path.
func hostPathInDistro(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if len(abs) < 3 || abs[1] != ':' {
		return "", fmt.Errorf("engineupgrade: %q is not an absolute Windows path", p)
	}
	drive := strings.ToLower(abs[:1])
	rest := strings.ReplaceAll(abs[2:], `\`, "/")
	return "/mnt/" + drive + rest, nil
}

// brief reduces a start failure to something a person reads. StartEngine
// reports the tail of the engine's own log, which is the right thing to keep
// somewhere and the wrong thing to paste into a one-line failure: the useful
// part is the first line, and `skrog logs --source dockerd` has the rest.
func brief(err error) string {
	if err == nil {
		return ""
	}
	line := err.Error()
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	// StartEngine's message ends with "Last log lines:" and then the log, so
	// cutting at the newline leaves a label introducing nothing.
	line = strings.TrimSpace(strings.TrimSuffix(line, "Last log lines:"))
	line = strings.TrimRight(line, ".")
	const max = 160
	if len(line) > max {
		line = line[:max] + "…"
	}
	return line + " (see `skrog logs --source dockerd`)"
}
