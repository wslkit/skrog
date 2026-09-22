package engineupgrade_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/engineupgrade"
)

// fakeDistro records the shell commands an upgrade runs and answers the
// version probe.
type fakeDistro struct {
	calls      []string
	installErr error
	version    string
	// versionErr makes the dockerd --version probe fail, which is what the
	// "confirm the swap took" step has to survive without reporting a blank.
	versionErr error
}

func (f *fakeDistro) Exec(_ context.Context, _, _ string, args ...string) (string, error) {
	cmd := args[len(args)-1]
	f.calls = append(f.calls, cmd)
	if strings.Contains(cmd, "dockerd") && strings.Contains(cmd, "--version") {
		return f.version, f.versionErr
	}
	if len(args) > 0 && args[0] == "dockerd" {
		return f.version, f.versionErr
	}
	if strings.Contains(cmd, "install -m 0755") {
		return "", f.installErr
	}
	return "", nil
}

type fakeFetcher struct {
	tarball string // copied to dest on Fetch
	err     error
}

func (f *fakeFetcher) FetchRootfs(_ context.Context, _, _, dest string) error {
	if f.err != nil {
		return f.err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(f.tarball)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, b, 0o644)
}

// makeRootfs writes a .tar.gz shaped like a real rootfs export: engine
// binaries under usr/local/bin plus decoys that must be left alone.
func makeRootfs(t *testing.T, dir string, binaries map[string]string) string {
	t.Helper()
	p := filepath.Join(dir, "skrog-rootfs-test.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	write := func(name, content string) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range binaries {
		write("usr/local/bin/"+name, content)
	}
	// Decoys: an Alpine tool in the same directory that is not ours, and files
	// elsewhere. An engine upgrade must not touch the distro's own userland.
	write("usr/local/bin/socat", "alpine's socat")
	write("bin/busybox", "busybox")
	write("etc/docker/daemon.json", "{}")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractBinariesTakesOnlyTheEngine(t *testing.T) {
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{
		"dockerd":     "dockerd-bytes",
		"containerd":  "containerd-bytes",
		"runc":        "runc-bytes",
		"skrog-agent": "agent-bytes",
	})
	dest := filepath.Join(dir, "staging")

	got, err := engineupgrade.ExtractBinaries(src, dest)
	if err != nil {
		t.Fatalf("ExtractBinaries: %v", err)
	}
	want := []string{"containerd", "dockerd", "runc", "skrog-agent"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	// socat sits in the same directory and must not have been taken.
	if _, err := os.Stat(filepath.Join(dest, "socat")); err == nil {
		t.Error("extracted socat: an engine upgrade must not replace the distro's userland")
	}
	b, err := os.ReadFile(filepath.Join(dest, "dockerd"))
	if err != nil || string(b) != "dockerd-bytes" {
		t.Errorf("dockerd content = %q (%v)", b, err)
	}
}

func TestExtractBinariesToleratesLeadingDotSlash(t *testing.T) {
	// `docker export | gzip` and `tar czf .` differ on this, and both are
	// plausible sources for a rootfs tarball.
	dir := t.TempDir()
	p := filepath.Join(dir, "r.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := "x"
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/local/bin/dockerd", Mode: 0o755,
		Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	f.Close()

	got, err := engineupgrade.ExtractBinaries(p, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("ExtractBinaries: %v", err)
	}
	if len(got) != 1 || got[0] != "dockerd" {
		t.Errorf("got %v, want [dockerd]", got)
	}
}

func runner(t *testing.T, d *fakeDistro, f *fakeFetcher) (*engineupgrade.Runner, *[]string) {
	t.Helper()
	var events []string
	r := &engineupgrade.Runner{
		WSL:     d,
		Fetcher: f,
		Stop:    func(context.Context) error { events = append(events, "stop"); return nil },
		Start:   func(context.Context) error { events = append(events, "start"); return nil },
		Healthy: func(context.Context) bool { events = append(events, "healthy?"); return true },
		Restore: func(_ context.Context, prev string) error {
			events = append(events, "restore:"+prev)
			return nil
		},
	}
	return r, &events
}

func opts(t *testing.T, dir string) engineupgrade.Options {
	return engineupgrade.Options{
		StateDir: dir,
		Distro:   "skrog-engine",
		From:     "29.7.2-3",
		Target: engineupgrade.Engine{
			Ref:    "29.7.2-4",
			URL:    "https://example.invalid/skrog-rootfs-29.7.2-4.tar.gz",
			SHA256: "abc",
		},
	}
}

func TestUpgradeSwapsAndVerifies(t *testing.T) {
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "new", "runc": "new"})
	d := &fakeDistro{version: "Docker version 29.7.2, build abcdef"}
	r, events := runner(t, d, &fakeFetcher{tarball: src})

	rep, err := r.Run(context.Background(), opts(t, dir))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(*events, ","); got != "stop,start,healthy?" {
		t.Errorf("lifecycle = %s, want stop,start,healthy?", got)
	}
	if len(rep.Replaced) != 2 {
		t.Errorf("Replaced = %v", rep.Replaced)
	}
	if rep.EngineVersion != "29.7.2" {
		t.Errorf("EngineVersion = %q, want 29.7.2 (parsed from dockerd --version)", rep.EngineVersion)
	}
	if rep.RolledBack {
		t.Error("RolledBack set on a successful upgrade")
	}
	// The copy must use install(1), which renames into place.
	var sawInstall bool
	for _, c := range d.calls {
		if strings.Contains(c, "install -m 0755") {
			sawInstall = true
		}
	}
	if !sawInstall {
		t.Errorf("no install(1) copy: %v", d.calls)
	}
}

func TestFailedStartRollsBackAndSaysSo(t *testing.T) {
	// The exit criterion from #65: a broken new engine leaves a working one.
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "broken"})
	d := &fakeDistro{version: "Docker version 29.7.2, build x"}
	var events []string
	r := &engineupgrade.Runner{
		WSL:     d,
		Fetcher: &fakeFetcher{tarball: src},
		Stop:    func(context.Context) error { events = append(events, "stop"); return nil },
		Start:   func(context.Context) error { events = append(events, "start"); return errors.New("dockerd exited") },
		Healthy: func(context.Context) bool { return false },
		Restore: func(_ context.Context, prev string) error {
			events = append(events, "restore:"+prev)
			return nil
		},
	}

	rep, err := r.Run(context.Background(), opts(t, dir))
	if err == nil {
		t.Fatal("a failed start was reported as success")
	}
	if !rep.RolledBack {
		t.Error("RolledBack not set after a failed start")
	}
	if !strings.Contains(err.Error(), "rolled back to 29.7.2-3") {
		t.Errorf("the error should say the engine is back: %v", err)
	}
	if got := strings.Join(events, ","); got != "stop,start,restore:29.7.2-3" {
		t.Errorf("lifecycle = %s", got)
	}
}

func TestUnhealthyEngineRollsBack(t *testing.T) {
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "quiet"})
	d := &fakeDistro{version: "Docker version 29.7.2, build x"}
	var restored string
	r := &engineupgrade.Runner{
		WSL:     d,
		Fetcher: &fakeFetcher{tarball: src},
		Stop:    func(context.Context) error { return nil },
		Start:   func(context.Context) error { return nil },
		// Starts, but never answers: the case a start-only check would miss.
		Healthy: func(context.Context) bool { return false },
		Restore: func(_ context.Context, prev string) error { restored = prev; return nil },
	}

	_, err := r.Run(context.Background(), opts(t, dir))
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("err = %v, want the unhealthy report", err)
	}
	if restored != "29.7.2-3" {
		t.Errorf("restored = %q", restored)
	}
}

func TestFailedRollbackIsSaidPlainly(t *testing.T) {
	// The worst case: the upgrade failed AND the restore failed. The message
	// has to say the engine may be down, not just "upgrade failed".
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "x"})
	r := &engineupgrade.Runner{
		WSL:     &fakeDistro{},
		Fetcher: &fakeFetcher{tarball: src},
		Stop:    func(context.Context) error { return nil },
		Start:   func(context.Context) error { return errors.New("boom") },
		Healthy: func(context.Context) bool { return false },
		Restore: func(context.Context, string) error { return errors.New("restore boom") },
	}

	_, err := r.Run(context.Background(), opts(t, dir))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"boom", "restore", "may be down", "skrog engine rollback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}

func TestUpgradeToTheSameVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	d := &fakeDistro{}
	r, _ := runner(t, d, &fakeFetcher{})
	o := opts(t, dir)
	o.Target.Ref = o.From

	_, err := r.Run(context.Background(), o)
	var same *engineupgrade.ErrSameVersion
	if !errors.As(err, &same) {
		t.Fatalf("err = %v, want *ErrSameVersion", err)
	}
	if len(d.calls) != 0 {
		t.Errorf("a refusal still touched the distro: %v", d.calls)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	d := &fakeDistro{}
	r, events := runner(t, d, &fakeFetcher{err: errors.New("must not fetch")})
	o := opts(t, dir)
	o.DryRun = true

	rep, err := r.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*events) != 0 || len(d.calls) != 0 {
		t.Errorf("--dry-run acted: events=%v calls=%v", *events, d.calls)
	}
	if len(rep.Steps) != 6 {
		t.Errorf("Steps = %v, want the plan including the rollback note", rep.Steps)
	}
}

func TestAnEmptyTarballIsRefusedBeforeStopping(t *testing.T) {
	// A tarball with no engine binaries would otherwise stop the engine and
	// then copy nothing, leaving it down for no reason.
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{})
	d := &fakeDistro{}
	r, events := runner(t, d, &fakeFetcher{tarball: src})

	if _, err := r.Run(context.Background(), opts(t, dir)); err == nil {
		t.Fatal("expected a refusal for a tarball with no engine binaries")
	}
	if len(*events) != 0 {
		t.Errorf("the engine was touched: %v", *events)
	}
}

func TestBriefKeepsFailuresReadable(t *testing.T) {
	// StartEngine reports the tail of the engine log, which is the right thing
	// to keep in a log file and the wrong thing to put in a one-line failure.
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "broken"})
	noisy := errors.New("dockerd did not create /var/run/docker.sock within 1m0s. Last log lines:\n" +
		strings.Repeat("time=\"...\" level=info msg=\"noise\"\n", 40))
	r := &engineupgrade.Runner{
		WSL:     &fakeDistro{},
		Fetcher: &fakeFetcher{tarball: src},
		Stop:    func(context.Context) error { return nil },
		Start:   func(context.Context) error { return noisy },
		Healthy: func(context.Context) bool { return false },
		Restore: func(context.Context, string) error { return nil },
	}

	_, err := r.Run(context.Background(), opts(t, dir))
	if err == nil {
		t.Fatal("expected a failure")
	}
	msg := err.Error()
	if strings.Count(msg, "\n") != 0 {
		t.Errorf("the failure spans lines:\n%s", msg)
	}
	if strings.Contains(msg, "noise") {
		t.Errorf("the engine log leaked into the message:\n%s", msg)
	}
	if strings.Contains(msg, "Last log lines:") {
		t.Errorf("a label introducing nothing survived:\n%s", msg)
	}
	// `skrog logs --source dockerd`, not `--engine`: there has never been an
	// --engine flag (cmd/skrog/logs.go takes --source supervisor|dockerd|audit),
	// so this message sent a user whose upgrade had just rolled back to a usage
	// error, at the worst possible moment. docs/engine-upgrade.md quoted it
	// faithfully and this test pinned it, which is how a wrong command outlived
	// three readings of the file (#244).
	for _, want := range []string{"docker.sock", "skrog logs --source dockerd", "rolled back to 29.7.2-3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}

func TestStagingIsNotLeftBehind(t *testing.T) {
	// The extracted binaries are ~250 MB and redundant once copied in; the
	// verified tarball beside them is what a rollback re-extracts from.
	dir := t.TempDir()
	src := makeRootfs(t, dir, map[string]string{"dockerd": "new", "runc": "new"})
	d := &fakeDistro{version: "Docker version 29.7.2, build x"}
	r, _ := runner(t, d, &fakeFetcher{tarball: src})

	if _, err := r.Run(context.Background(), opts(t, dir)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	staging := filepath.Join(dir, "engine-staging", "29.7.2-4")
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("%s survived the upgrade (err=%v)", staging, err)
	}
	// The tarball must still be there: it is the rollback source.
	if _, err := os.Stat(filepath.Join(dir, "rootfs", "skrog-rootfs-29.7.2-4.tar.gz")); err != nil {
		t.Errorf("the cached rootfs was removed: %v", err)
	}
}

// The zip side has had this test since #248; the tar side never did, and a
// rootfs tarball is the more plausible carrier of a hostile entry -- it comes
// from a registry, not from our own release page.
//
// Two shapes, both of which used to reach filepath.Join with a name the
// archive chose. They resolve differently, and both outcomes are correct:
//
//   - "usr/local/bin/../../../../dockerd" cleans to "/dockerd", whose
//     directory is not the engine's bin dir, so it is skipped entirely.
//   - "../../../../../../usr/local/bin/containerd" cleans to
//     "/usr/local/bin/containerd" -- collapsing a leading ".." against root
//     is exactly what path.Clean is for -- so it IS taken, and lands at
//     dest/containerd by its base name. Same answer the zip side gives in
//     TestStageIgnoresPathTraversalInZipEntries.
//
// What must never happen either way is a write outside dest.
func TestExtractBinariesIgnoresPathTraversal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	dest := filepath.Join(root, "staging")

	p := filepath.Join(dir, "r.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{
		"usr/local/bin/../../../../dockerd",
		"../../../../../../usr/local/bin/containerd",
		"usr/local/bin/runc",
	} {
		body := "x"
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755,
			Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	f.Close()

	got, err := engineupgrade.ExtractBinaries(p, dest)
	if err != nil {
		t.Fatalf("ExtractBinaries: %v", err)
	}
	if strings.Join(got, ",") != "containerd,runc" {
		t.Errorf("got %v, want [containerd runc]", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "containerd")); err != nil {
		t.Errorf("the cleaned entry should have landed inside dest: %v", err)
	}
	for _, escaped := range []string{
		filepath.Join(dir, "dockerd"),
		filepath.Join(root, "dockerd"),
		filepath.Join(dir, "containerd"),
	} {
		if _, err := os.Stat(escaped); err == nil {
			t.Errorf("an entry escaped the staging directory: %s", escaped)
		}
	}
}

// makeRootfsAt writes a tarball whose keys are full archive paths, so a test
// can put a file outside usr/local/bin. makeRootfs cannot: every key it takes
// goes into that one directory, which is the assumption #479 broke.
func makeRootfsAt(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(dir, "skrog-rootfs-paths.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// The emulator is engine payload that does not live in usr/local/bin, and
// skipping it meant `skrog engine upgrade` could move an install to 29.8.1-3
// and not deliver the one thing that revision was cut for (#479).
func TestExtractBinariesTakesTheEmulator(t *testing.T) {
	dir := t.TempDir()
	// An amd64 image: qemu-aarch64 and no qemu-x86_64, because each image
	// carries exactly one, for the architecture it is NOT (#462).
	src := makeRootfsAt(t, dir, map[string]string{
		"usr/local/bin/dockerd": "dockerd-bytes",
		"usr/bin/qemu-aarch64":  "emulator-bytes",
		// Decoys in both directions: the pairing of directory and name has to
		// be checked, not just the name. A flat staging area means a decoy
		// that IS accepted silently overwrites the real file.
		"usr/bin/dockerd":           "not the engine's dockerd",
		"usr/local/bin/qemu-x86_64": "not where an interpreter lives",
		"usr/local/bin/socat":       "alpine's own",
	})
	dest := filepath.Join(dir, "staging")

	got, err := engineupgrade.ExtractBinaries(src, dest)
	if err != nil {
		t.Fatalf("ExtractBinaries: %v", err)
	}
	if want := []string{"dockerd", "qemu-aarch64"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "qemu-aarch64")); err != nil || string(b) != "emulator-bytes" {
		t.Errorf("qemu-aarch64 content = %q (%v)", b, err)
	}
	// If /usr/bin/dockerd had been accepted it would have clobbered the real
	// one, and the upgrade would install the wrong bytes as the engine.
	if b, err := os.ReadFile(filepath.Join(dest, "dockerd")); err != nil || string(b) != "dockerd-bytes" {
		t.Errorf("dockerd content = %q (%v); a decoy outside %s was taken", b, err, "usr/local/bin")
	}
	if _, err := os.Stat(filepath.Join(dest, "qemu-x86_64")); err == nil {
		t.Error("staged qemu-x86_64 from usr/local/bin: an interpreter is only an interpreter in /usr/bin")
	}
}

// Staging is one flat directory keyed by name, so a name in both sets would
// make one file silently overwrite the other. Asserted rather than assumed,
// because the failure would be a wrong binary installed, not an error.
func TestEngineFileSetsDoNotOverlap(t *testing.T) {
	for _, b := range engineupgrade.EngineBinaries {
		for _, e := range engineupgrade.EngineEmulators {
			if b == e {
				t.Errorf("%q is in both EngineBinaries and EngineEmulators; "+
					"flat staging would make one overwrite the other", b)
			}
		}
	}
}

// The emulator has to land in /usr/bin, because that is the path the
// registration in internal/emulation/*.reg names and the path the start-up
// `test -x` checks. Installing it beside dockerd would satisfy the extractor
// and still leave the feature broken.
func TestUpgradeInstallsTheEmulatorInUsrBin(t *testing.T) {
	dir := t.TempDir()
	src := makeRootfsAt(t, dir, map[string]string{
		"usr/local/bin/dockerd": "new",
		"usr/bin/qemu-aarch64":  "emulator",
	})
	d := &fakeDistro{version: "Docker version 29.7.2, build abcdef"}
	r, _ := runner(t, d, &fakeFetcher{tarball: src})

	if _, err := r.Run(context.Background(), opts(t, dir)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var copyCmd string
	for _, c := range d.calls {
		if strings.Contains(c, "install -m") {
			copyCmd = c
		}
	}
	if copyCmd == "" {
		t.Fatal("no install command was run")
	}
	if !strings.Contains(copyCmd, `"/usr/bin/qemu-aarch64"`) {
		t.Errorf("emulator not installed into /usr/bin:\n%s", copyCmd)
	}
	if !strings.Contains(copyCmd, `"/usr/local/bin/dockerd"`) {
		t.Errorf("dockerd not installed into /usr/local/bin:\n%s", copyCmd)
	}
	if strings.Contains(copyCmd, `"/usr/local/bin/qemu-aarch64"`) {
		t.Errorf("emulator installed beside the engine binaries, where nothing looks for it:\n%s", copyCmd)
	}
}

// A failed copy has to say which file it died on, and has to say so even when
// the command printed nothing at all (#483).
func TestUpgradeFailureNamesTheFileAndSurvivesSilence(t *testing.T) {
	dir := t.TempDir()
	src := makeRootfsAt(t, dir, map[string]string{"usr/local/bin/dockerd": "new"})
	d := &fakeDistro{
		version:    "Docker version 29.7.2, build abcdef",
		installErr: errors.New("exit status 1"),
	}
	r, _ := runner(t, d, &fakeFetcher{tarball: src})

	_, err := r.Run(context.Background(), opts(t, dir))
	if err == nil {
		t.Fatal("Run succeeded while the copy failed")
	}
	// The old message ended in a bare ": ", which reads as truncated.
	if strings.HasSuffix(err.Error(), ": ") || strings.Contains(err.Error(), ": : ") {
		t.Errorf("error ends in an empty output section: %q", err)
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("error should say the command printed nothing: %q", err)
	}
	// And the command itself must name each file, so the transcript of a real
	// failure identifies the one that broke.
	var copyCmd string
	for _, c := range d.calls {
		if strings.Contains(c, "install -m") {
			copyCmd = c
		}
	}
	if !strings.Contains(copyCmd, "installing dockerd") {
		t.Errorf("copy command does not name the file it is installing:\n%s", copyCmd)
	}
}
