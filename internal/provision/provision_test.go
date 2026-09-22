package provision_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/emulation"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wsl"
)

// fakeWSL is a scriptable stand-in for wsl.exe.
type fakeWSL struct {
	mu sync.Mutex

	status  wsl.Status
	distros []wsl.Distro

	imported     [][3]string // distro, dir, rootfs
	started      [][]string
	terminated   []string
	unregistered []string

	// socketAlwaysUp models an engine that is already running before we start.
	socketAlwaysUp bool
	execs          [][]string
	// socketUpAfter delays the socket by N checks after dockerd is launched,
	// simulating a daemon that takes a moment to come up.
	socketUpAfter int
	socketCalls   int

	// engineVersionFile is what /etc/skrog/engine-version contains; empty
	// means the file is absent.
	// panicOnExecContaining makes one exec panic, to prove a step cannot take
	// the process down with it.
	panicOnExecContaining string

	// delayExecContaining makes one exec slow. Without it a goroutine started
	// by StartEngine finishes before StartEngine does no matter what, and a
	// test for "this was joined" passes whether the join is there or not.
	delayExecContaining string
	execDelay           time.Duration

	// order interleaves Exec and Start calls, which the separate slices cannot:
	// #398 made the pre-launch steps concurrent, and what has to hold is that
	// every one of them finishes BEFORE dockerd is launched.
	order []string

	engineVersionFile string
	agentVersion      string // skrog-agent -version output

	importErr error
	statusErr error
	listErr   error
}

func (f *fakeWSL) Status(context.Context) (wsl.Status, error) {
	return f.status, f.statusErr
}

func (f *fakeWSL) Export(_ context.Context, distro, tarPath string) error { return nil }

func (f *fakeWSL) Import(_ context.Context, distro, dir, rootfs string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.importErr != nil {
		return f.importErr
	}
	f.imported = append(f.imported, [3]string{distro, dir, rootfs})
	f.setState(distro, "Stopped")
	return nil
}

// setState upserts a distro's state; callers hold f.mu.
func (f *fakeWSL) setState(distro, state string) {
	for i := range f.distros {
		if f.distros[i].Name == distro {
			f.distros[i].State = state
			return
		}
	}
	f.distros = append(f.distros, wsl.Distro{Name: distro, State: state, Version: 2})
}

func (f *fakeWSL) Unregister(_ context.Context, distro string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, distro)
	out := f.distros[:0]
	for _, d := range f.distros {
		if d.Name != distro {
			out = append(out, d)
		}
	}
	f.distros = out
	return nil
}

func (f *fakeWSL) Terminate(_ context.Context, distro string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, distro)
	f.setState(distro, "Stopped")
	return nil
}

func (f *fakeWSL) List(context.Context) ([]wsl.Distro, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]wsl.Distro, len(f.distros))
	copy(out, f.distros)
	return out, nil
}

func (f *fakeWSL) Exec(_ context.Context, _, _ string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, args)
	f.order = append(f.order, "exec: "+strings.Join(args, " "))
	if f.delayExecContaining != "" && strings.Contains(strings.Join(args, " "), f.delayExecContaining) {
		// Released while sleeping: a real wsl round trip does not hold a
		// lock, and holding one here would serialize the very overlap under
		// test.
		f.mu.Unlock()
		time.Sleep(f.execDelay)
		f.mu.Lock()
	}
	if f.panicOnExecContaining != "" && strings.Contains(strings.Join(args, " "), f.panicOnExecContaining) {
		panic("fakeWSL: deliberate panic in " + f.panicOnExecContaining)
	}
	if len(args) >= 3 && args[0] == "sh" && strings.Contains(args[2], "_ping") {
		const ok = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nOK"
		if f.socketAlwaysUp {
			return ok, nil
		}
		// No answering daemon until something launched dockerd — the same
		// order a real machine enforces, so tests cannot skip the start path.
		if len(f.started) == 0 {
			return "", errors.New("exit status 1")
		}
		f.socketCalls++
		if f.socketCalls > f.socketUpAfter {
			return ok, nil
		}
		return "", errors.New("exit status 1")
	}
	if len(args) >= 3 && args[0] == "sh" && strings.Contains(args[2], "skrog-agent -version") {
		return f.agentVersion, nil
	}
	if len(args) >= 3 && args[0] == "sh" && strings.Contains(args[2], "agent-secret") {
		return "deadbeefsecret", nil
	}
	if len(args) > 0 && args[0] == "tail" {
		return "dockerd log line 1\ndockerd log line 2", nil
	}
	if len(args) >= 2 && args[0] == "cat" && args[1] == provision.EngineVersionFile {
		if f.engineVersionFile == "" {
			return "", errors.New("cat: no such file")
		}
		return f.engineVersionFile, nil
	}
	return "", nil
}

func (f *fakeWSL) Start(_ context.Context, distro, _ string, args ...string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, args)
	f.order = append(f.order, "start: "+strings.Join(args, " "))
	// wsl.exe boots a stopped distro to run anything in it — model that, since
	// the health probe's stopped-distro gate (#82) depends on it.
	f.setState(distro, "Running")
	return func() {}, nil
}

var _ wsl.WSL = (*fakeWSL)(nil)

func healthyWSL() *fakeWSL {
	return &fakeWSL{status: wsl.Status{Installed: true, DefaultVersion: 2, Version: "2.7.8.0"}}
}

// bootedWSL is healthyWSL plus an already-registered, already-running default
// distro — the state a supervisor finds on an installed machine. Kept apart
// from healthyWSL because preflight refuses to INSTALL over a registered
// distro.
func bootedWSL() *fakeWSL {
	w := healthyWSL()
	w.setState(provision.DefaultDistro, "Running")
	return w
}

// startedMatching counts fake-started commands whose joined argv contains s.
func startedMatching(w *fakeWSL, s string) int {
	n := 0
	for _, args := range w.started {
		if strings.Contains(strings.Join(args, " "), s) {
			n++
		}
	}
	return n
}

// rootfsServer serves a payload and returns its URL and SHA-256.
func rootfsServer(t *testing.T, payload []byte) (url, sum string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	h := sha256.Sum256(payload)
	return srv.URL + "/skrog-rootfs-29.7.2.tar.gz", hex.EncodeToString(h[:])
}

func testOptions(t *testing.T, url, sum string) provision.Options {
	t.Helper()
	dir := t.TempDir()
	return provision.Options{
		StateDir:      filepath.Join(dir, "state"),
		DataDir:       filepath.Join(dir, "data"),
		RootfsURL:     url,
		RootfsSHA256:  sum,
		EngineVersion: "29.7.2",
		StartTimeout:  2 * time.Second,
	}
}

func TestInstallHappyPath(t *testing.T) {
	url, sum := rootfsServer(t, []byte("pretend rootfs tarball"))
	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)

	m, err := p.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(w.imported) != 1 {
		t.Fatalf("imported %d distros, want 1", len(w.imported))
	}
	if got := w.imported[0][0]; got != provision.DefaultDistro {
		t.Errorf("distro = %q, want %q", got, provision.DefaultDistro)
	}
	if got := w.imported[0][1]; got != opts.DataDir {
		t.Errorf("data dir = %q, want %q", got, opts.DataDir)
	}
	// The rootfs must have been downloaded to disk before import.
	if _, err := os.Stat(w.imported[0][2]); err != nil {
		t.Errorf("rootfs not on disk at import time: %v", err)
	}
	if n := startedMatching(w, "dockerd"); n != 1 {
		t.Errorf("dockerd started %d times, want 1", n)
	}
	// The vsock agent (#40) rides along with every engine start; its command
	// carries its own "not in this rootfs" and "already running" guards.
	if n := startedMatching(w, "skrog-agent"); n != 1 {
		t.Errorf("skrog-agent started %d times, want 1", n)
	}

	if m.EngineVersion != "29.7.2" || m.WSLVersion != "2.7.8.0" {
		t.Errorf("manifest = %+v", m)
	}

	// And it must be readable back, which is what `skrog version` relies on.
	back, err := p.ReadManifest(opts)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if back.Distro != m.Distro || back.RootfsSHA256 != sum {
		t.Errorf("manifest round-trip mismatch: %+v vs %+v", back, m)
	}
}

func TestInstallRefusesBadChecksum(t *testing.T) {
	// The rootfs becomes root inside the engine VM, so this must be a hard stop
	// and must not leave a partial file that a later run could trust.
	url, _ := rootfsServer(t, []byte("pretend rootfs tarball"))
	wrong := strings.Repeat("0", 64)

	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, wrong)

	_, err := p.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install succeeded with a bad checksum")
	}
	var mismatch *provision.ErrChecksumMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %T (%v), want *ErrChecksumMismatch", err, err)
	}
	if len(w.imported) != 0 {
		t.Error("distro was imported despite the checksum mismatch")
	}

	// Nothing left behind at all: a partial file could be mistaken for a good
	// download, and a completed one would be actively dangerous.
	entries, _ := os.ReadDir(filepath.Join(opts.StateDir, "rootfs"))
	for _, e := range entries {
		t.Errorf("file left behind after checksum failure: %q", e.Name())
	}
}

func TestInstallRequiresChecksum(t *testing.T) {
	url, _ := rootfsServer(t, []byte("x"))
	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, url, "")

	if _, err := p.Install(context.Background(), opts); err == nil {
		t.Fatal("Install succeeded with no expected checksum")
	}
}

func TestInstallRequiresRootfsURL(t *testing.T) {
	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, "", "")
	if _, err := p.Install(context.Background(), opts); err == nil {
		t.Fatal("Install succeeded with no rootfs URL")
	}
}

func TestInstallRefusesExistingDistro(t *testing.T) {
	// That distro holds the user's images and volumes; reimporting would
	// destroy them, so this is a refusal rather than a silent overwrite.
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	w.distros = []wsl.Distro{{Name: provision.DefaultDistro, State: "Stopped", Version: 2}}

	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	_, err := p.Install(context.Background(), testOptions(t, url, sum))
	if err == nil {
		t.Fatal("Install succeeded over an existing distro")
	}
	var pfe *provision.PreflightError
	if !errors.As(err, &pfe) {
		t.Fatalf("error = %T (%v), want *PreflightError", err, err)
	}
	if !strings.Contains(err.Error(), "uninstall") {
		t.Errorf("error should suggest uninstall, got: %v", err)
	}
	if len(w.imported) != 0 {
		t.Error("import happened despite preflight failure")
	}
}

func TestInstallReusesVerifiedCachedRootfs(t *testing.T) {
	payload := []byte("cached rootfs")
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write(payload)
	}))
	defer srv.Close()
	h := sha256.Sum256(payload)
	sum := hex.EncodeToString(h[:])
	url := srv.URL + "/rootfs.tar.gz"

	opts := testOptions(t, url, sum)
	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Second attempt with a fresh WSL (no distro registered) must not re-download.
	p2 := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	if _, err := p2.Install(context.Background(), opts); err != nil {
		t.Fatalf("second install: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("server hit %d times, want 1 (cached copy should be reused)", hits)
	}
}

func TestInstallReplacesCorruptCachedRootfs(t *testing.T) {
	payload := []byte("good rootfs")
	url, sum := rootfsServer(t, payload)
	opts := testOptions(t, url, sum)

	// Plant a file with the right name but the wrong contents.
	cache := filepath.Join(opts.StateDir, "rootfs", filepath.Base(url))
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("cached rootfs = %q, want it replaced with %q", got, payload)
	}
}

func TestInstallWritesManifestBeforeEngineStart(t *testing.T) {
	// If the engine fails to start, uninstall still needs to know what to
	// clean up, so the manifest must already be on disk.
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	w.socketUpAfter = 1000 // never comes up within the timeout

	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)
	opts.StartTimeout = 500 * time.Millisecond

	m, err := p.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install succeeded despite the engine never starting")
	}
	if m == nil {
		t.Fatal("Install returned no manifest; uninstall would not know what to remove")
	}
	if _, err := p.ReadManifest(opts); err != nil {
		t.Errorf("manifest not persisted: %v", err)
	}
	// The failure must name the daemon log, or it is undiagnosable.
	if !strings.Contains(err.Error(), "dockerd log line") {
		t.Errorf("error should include the dockerd log, got: %v", err)
	}
}

func TestStartEngineWaitsForSocket(t *testing.T) {
	w := healthyWSL()
	w.socketUpAfter = 3 // succeeds on the 4th check
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: 5 * time.Second}
	if err := p.StartEngine(context.Background(), opts); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	if n := startedMatching(w, "dockerd"); n != 1 {
		t.Errorf("dockerd started %d times, want 1", n)
	}
}

func TestStartEngineSkipsWhenAlreadyRunning(t *testing.T) {
	w := bootedWSL()
	w.socketAlwaysUp = true
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: time.Second}
	if err := p.StartEngine(context.Background(), opts); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	if n := startedMatching(w, "dockerd"); n != 0 {
		t.Errorf("dockerd was launched even though the socket was already up")
	}
	// But the agent must still be ensured: a restarted supervisor finding a
	// healthy engine cannot assume the vsock path is up.
	if n := startedMatching(w, "skrog-agent"); n != 1 {
		t.Errorf("skrog-agent ensured %d times, want 1", n)
	}
}

func TestStartEngineRespectsContextCancel(t *testing.T) {
	w := healthyWSL()
	w.socketUpAfter = 1000
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: 30 * time.Second}
	start := time.Now()
	if err := p.StartEngine(ctx, opts); err == nil {
		t.Fatal("StartEngine succeeded, want cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to honor cancellation", elapsed)
	}
}

func TestUninstallRemovesWhatInstallCreated(t *testing.T) {
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)

	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := p.Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if len(w.unregistered) != 1 || w.unregistered[0] != provision.DefaultDistro {
		t.Errorf("unregistered = %v, want [%s]", w.unregistered, provision.DefaultDistro)
	}
	if _, err := os.Stat(opts.DataDir); !os.IsNotExist(err) {
		t.Errorf("data dir still present: %v", err)
	}
	if _, err := p.ReadManifest(opts); err == nil {
		t.Error("manifest still present after uninstall")
	}
}

func TestUninstallOnCleanMachineIsNotAnError(t *testing.T) {
	// A half-finished or already-removed install must still come clean.
	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, "", "")
	if err := p.Uninstall(context.Background(), opts); err != nil {
		t.Errorf("Uninstall on a clean machine returned %v, want nil", err)
	}
}

func TestUninstallPrefersManifestOverOptions(t *testing.T) {
	// The manifest records where the install actually went, which matters when
	// the user installed with --distro or --data-dir and later runs a bare
	// `skrog uninstall`.
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := testOptions(t, url, sum)
	opts.Distro = "skrog-custom"
	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Uninstall called without the custom name, as the CLI would by default.
	bare := provision.Options{StateDir: opts.StateDir}
	if err := p.Uninstall(context.Background(), bare); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(w.unregistered) != 1 || w.unregistered[0] != "skrog-custom" {
		t.Errorf("unregistered = %v, want [skrog-custom]", w.unregistered)
	}
}

func TestFetchRootfsSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, srv.URL+"/missing.tar.gz", strings.Repeat("a", 64))

	_, err := p.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install succeeded against a 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention the HTTP status, got: %v", err)
	}
}

// customFetcher lets a test supply rootfs bytes without a server.
type customFetcher struct{ payload []byte }

func (c customFetcher) Fetch(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(c.payload))), nil
}

func TestFetcherSeam(t *testing.T) {
	payload := []byte("rootfs via custom fetcher")
	h := sha256.Sum256(payload)
	p := &provision.Provisioner{
		WSL:     healthyWSL(),
		Fetcher: customFetcher{payload: payload},
		Logger:  quietLogger(),
	}
	opts := testOptions(t, "https://example.invalid/rootfs.tar.gz", hex.EncodeToString(h[:]))

	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
}

func TestInstallFromLocalRootfs(t *testing.T) {
	// The path ErrNotPublished tells developers to use: install a rootfs built
	// locally, before any release exists. Verification still applies.
	payload := []byte("locally built rootfs")
	dir := t.TempDir()
	local := filepath.Join(dir, "skrog-rootfs-29.7.2.tar.gz")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(payload)

	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	for _, spelling := range []string{
		local,                                // bare Windows path
		"file:///" + filepath.ToSlash(local), // file:///C:/...
		"file://" + filepath.ToSlash(local),  // file://C:/...
	} {
		t.Run(spelling, func(t *testing.T) {
			w2 := healthyWSL()
			p2 := &provision.Provisioner{WSL: w2, Logger: quietLogger()}
			opts := testOptions(t, spelling, hex.EncodeToString(h[:]))
			if _, err := p2.Install(context.Background(), opts); err != nil {
				t.Fatalf("Install from %q: %v", spelling, err)
			}
			if len(w2.imported) != 1 {
				t.Errorf("imported %d, want 1", len(w2.imported))
			}
		})
	}
	_ = p
}

func TestInstallLocalRootfsStillVerifiesChecksum(t *testing.T) {
	// A local file is not more trusted than a downloaded one.
	dir := t.TempDir()
	local := filepath.Join(dir, "rootfs.tar.gz")
	if err := os.WriteFile(local, []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, local, strings.Repeat("b", 64))

	var mismatch *provision.ErrChecksumMismatch
	if _, err := p.Install(context.Background(), opts); !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want ErrChecksumMismatch", err)
	}
}

func TestInstallMissingLocalRootfsIsClear(t *testing.T) {
	p := &provision.Provisioner{WSL: healthyWSL(), Logger: quietLogger()}
	opts := testOptions(t, filepath.Join(t.TempDir(), "absent.tar.gz"), strings.Repeat("c", 64))

	_, err := p.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install succeeded with a missing local rootfs")
	}
	if !strings.Contains(err.Error(), "local rootfs") {
		t.Errorf("error should say the local rootfs could not be opened, got: %v", err)
	}
}

func TestInstallReadsEngineVersionFromRootfs(t *testing.T) {
	// With --rootfs-url and no --engine-version there is no flag to trust, so
	// the rootfs's own /etc/skrog/engine-version is the source of truth.
	// Without this, `skrog version` reports "engine unknown" after a
	// development install - seen for real before this was added.
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	w.engineVersionFile = "29.7.2\n"

	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)
	opts.EngineVersion = "" // as an override install leaves it

	m, err := p.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.EngineVersion != "29.7.2" {
		t.Errorf("EngineVersion = %q, want 29.7.2 from the rootfs", m.EngineVersion)
	}
}

func TestInstallPrefersExplicitEngineVersion(t *testing.T) {
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL()
	w.engineVersionFile = "1.2.3\n"

	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum) // testOptions sets 29.7.2

	m, err := p.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.EngineVersion != "29.7.2" {
		t.Errorf("EngineVersion = %q, want the explicit 29.7.2", m.EngineVersion)
	}
}

func TestInstallToleratesRootfsWithoutVersionMarker(t *testing.T) {
	// An old or third-party rootfs need not carry the marker; that is not a
	// reason to fail an install.
	url, sum := rootfsServer(t, []byte("rootfs"))
	w := healthyWSL() // engineVersionFile empty -> cat fails
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)
	opts.EngineVersion = ""

	m, err := p.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.EngineVersion != "" {
		t.Errorf("EngineVersion = %q, want empty", m.EngineVersion)
	}
}

func TestEngineRunningRejectsStaleSocket(t *testing.T) {
	// #82: a crashed dockerd leaves its socket file behind; only an answering
	// daemon counts as running. The fake ping refuses until dockerd started.
	w := bootedWSL() // distro booted, nothing launched, ping refuses
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	if p.EngineRunning(context.Background(), provision.Options{}) {
		t.Error("EngineRunning true with a distro up but no daemon answering")
	}
}

func TestEngineRunningNeverBootsAStoppedDistro(t *testing.T) {
	// #82: wsl exec boots a stopped distro. After an idle-stop, every health
	// probe (tick, status, tray) must answer from the host-side distro list
	// alone — zero execs — or the idle feature's RAM reclaim is defeated.
	w := bootedWSL()
	w.socketAlwaysUp = true // even a would-be-healthy engine must not be probed
	w.setState(provision.DefaultDistro, "Stopped")
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	if p.EngineRunning(context.Background(), provision.Options{}) {
		t.Error("EngineRunning true for a stopped distro")
	}
	w.mu.Lock()
	n := len(w.execs)
	w.mu.Unlock()
	if n != 0 {
		t.Errorf("health probe ran %d exec(s) against a stopped distro; that boots it", n)
	}
}

func TestValidateDistroNameRejectsShellDangerousNames(t *testing.T) {
	url, sum := rootfsServer(t, []byte("x"))
	bad := []string{"../evil", "a b", "name;rm", "back`tick`", "$(x)", "..", ".", "a/b"}
	for _, name := range bad {
		w := healthyWSL()
		p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
		opts := testOptions(t, url, sum)
		opts.Distro = name
		if _, err := p.Install(context.Background(), opts); err == nil {
			t.Errorf("Install accepted dangerous distro name %q", name)
		}
		if len(w.imported) != 0 {
			t.Errorf("name %q reached import", name)
		}
	}
}

func TestWriteManifestIsAtomic(t *testing.T) {
	// No .tmp litter after a successful write, and the file round-trips —
	// the tmp+rename path (#93) must leave only the final file.
	url, sum := rootfsServer(t, []byte("pretend rootfs"))
	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
	opts := testOptions(t, url, sum)
	if _, err := p.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, "manifest.json.tmp")); !os.IsNotExist(err) {
		t.Error("manifest .tmp left behind after write")
	}
	if _, err := p.ReadManifest(opts); err != nil {
		t.Errorf("manifest not readable after atomic write: %v", err)
	}
}

func TestEnsureAgentSecretGatedOnAgentVersion(t *testing.T) {
	// #81: with a /2 agent the host mirrors the secret; with a /1 agent (older
	// rootfs) it must NOT, so the dialer keeps the v1 handshake that agent
	// speaks instead of refusing a downgrade.
	newFake := func(version string) *fakeWSL {
		w := bootedWSL()
		w.agentVersion = version
		return w
	}
	for _, tc := range []struct {
		version    string
		wantSecret bool
	}{
		{"skrog-agent/2", true},
		{"skrog-agent/1", false},
		{"", false},
	} {
		w := newFake(tc.version)
		p := &provision.Provisioner{WSL: w, Logger: quietLogger()}
		dir := t.TempDir()
		p.EnsureAgentSecretForTest(context.Background(), provision.Options{Distro: provision.DefaultDistro, StateDir: dir})
		_, err := os.Stat(provision.AgentSecretPath(dir))
		got := err == nil
		if got != tc.wantSecret {
			t.Errorf("agent %q: host secret present=%v, want %v", tc.version, got, tc.wantSecret)
		}
	}
}

// Every pre-launch step has to finish before dockerd starts.
//
// (An earlier draft of #398 ran these concurrently and this comment said so.
// They are SERIAL -- measured, see the note at the call site -- and a comment
// that outlived the change it described is how the next reader gets a wrong
// idea from a passing test.)
//
// Each has its own reason, written at its call site — network.env is sourced
// by the dockerd command line, the CDI spec and daemon.json are read by the
// daemon at startup, binfmt_misc has to be there for the first container. A
// refactor that let any of them drift past the launch would break all four
// quietly, on a path no unit test otherwise crosses.
func TestPreLaunchStepsAllFinishBeforeDockerdStarts(t *testing.T) {
	w := healthyWSL()
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{
		StateDir:           t.TempDir(),
		StartTimeout:       5 * time.Second,
		EmulationPlatforms: foreignPlatform(t),
		GPUEnabled:         true,
	}
	if err := p.StartEngine(context.Background(), opts); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}

	w.mu.Lock()
	order := append([]string(nil), w.order...)
	w.mu.Unlock()

	launch := -1
	for i, entry := range order {
		if strings.HasPrefix(entry, "start: ") && strings.Contains(entry, "dockerd") {
			launch = i
			break
		}
	}
	if launch < 0 {
		t.Fatalf("dockerd was never launched; calls were:\n%s", strings.Join(order, "\n"))
	}

	// Each step is identified by something only it writes.
	steps := map[string]string{
		"network":         "/etc/skrog/network.env",
		"emulation":       "binfmt",
		"engine-defaults": "daemon.json",
	}
	for name, marker := range steps {
		seen := -1
		for i, entry := range order {
			if strings.HasPrefix(entry, "exec: ") && strings.Contains(entry, marker) {
				seen = i
				break
			}
		}
		if seen < 0 {
			t.Errorf("the %s step never ran; calls were:\n%s", name, strings.Join(order, "\n"))
			continue
		}
		if seen > launch {
			t.Errorf("the %s step ran AFTER dockerd launched (index %d > %d); the daemon never saw it",
				name, seen, launch)
		}
	}
}

// The agent now starts alongside the wait for dockerd (#398), and the property
// that makes that safe is the JOIN: it must be fully started before
// StartEngine returns.
//
// Without it the supervisor would return to a caller that immediately dials
// the engine, with the vsock agent possibly not up -- which is not a crash,
// just a silent fall back to the socat relay at ~165 ms per connection instead
// of ~0.6 ms. That is exactly the kind of regression the agentBinaries comment
// warns is invisible.
func TestTheAgentIsStartedBeforeStartEngineReturns(t *testing.T) {
	w := healthyWSL()
	w.socketUpAfter = 0 // dockerd is up at once, so only the join can hold the return
	w.delayExecContaining = "skrog-agent -version"
	w.execDelay = 750 * time.Millisecond
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: 5 * time.Second}
	if err := p.StartEngine(context.Background(), opts); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	// Read immediately, with no settling time: the point is that the join
	// already happened, not that the goroutine gets there eventually.
	if n := startedMatching(w, "skrog-agent"); n != 1 {
		t.Errorf("skrog-agent started %d times by the time StartEngine returned, want 1", n)
	}
}

// ...and the same on the failure path, where there is no socket to wait for.
// A goroutine still running when StartEngine returns an error is one still
// exec'ing into a distro the caller is about to give up on.
func TestTheAgentIsJoinedEvenWhenTheEngineNeverComesUp(t *testing.T) {
	w := healthyWSL()
	w.socketUpAfter = 1000 // never, within the timeout below
	w.delayExecContaining = "skrog-agent -version"
	w.execDelay = 900 * time.Millisecond // outlives the StartTimeout below
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: 600 * time.Millisecond}
	if err := p.StartEngine(context.Background(), opts); err == nil {
		t.Fatal("StartEngine succeeded, want a timeout")
	}
	if n := startedMatching(w, "skrog-agent"); n != 1 {
		t.Errorf("skrog-agent started %d times, want 1 — the agent goroutine was not joined", n)
	}
}

// A panic starting the agent must not take the supervisor with it.
//
// The agent is never fatal by design (agentStartCmd says so at length), and
// moving it to a goroutine would have quietly made it fatal: a panic there
// cannot unwind into StartEngine, it kills the process. The supervisor IS the
// process, and a machine with no supervisor has no bridge at all.
func TestAPanickingAgentStartDoesNotKillTheEngineStart(t *testing.T) {
	w := healthyWSL()
	w.panicOnExecContaining = "agent-secret"
	p := &provision.Provisioner{WSL: w, Logger: quietLogger()}

	opts := provision.Options{StateDir: t.TempDir(), StartTimeout: 5 * time.Second}
	if err := p.StartEngine(context.Background(), opts); err != nil {
		t.Fatalf("StartEngine: %v", err)
	}
	if n := startedMatching(w, "dockerd"); n != 1 {
		t.Errorf("dockerd started %d times, want 1 — the engine must come up without the agent", n)
	}
}

// foreignPlatform names an architecture this host is NOT, so applyEmulation
// has something to register.
//
// Hardcoding "linux/arm64" fails on the arm64 runner, where it is the host's
// own architecture and Parse returns no handlers at all -- so the step under
// test does nothing and the test reads that as the step having been skipped.
// This is the third host-dependent test in this package's history; deriving
// the value from the product's own table is the fix that keeps working.
func foreignPlatform(t *testing.T) string {
	t.Helper()
	for _, arch := range emulation.Supported() {
		if arch == runtime.GOARCH {
			continue
		}
		if _, ok := emulation.HandlerFor(arch); ok {
			return "linux/" + arch
		}
	}
	t.Skip("no foreign architecture is supported on this host")
	return ""
}
