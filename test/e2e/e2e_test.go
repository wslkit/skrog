//go:build e2e

// Package e2e is the v0.1 acceptance suite (#11).
//
// It drives the real skrog.exe against the real published rootfs on a real
// Windows machine with WSL2 — the class of testing that found all ten of the
// defects the unit tests could not (see #38 for the tally). It deliberately
// shells out to the same binaries a user runs rather than importing internal
// packages: the product is the CLI contract, so that is what gets tested.
//
// Run on any Windows machine with WSL2 and a docker CLI:
//
//	cd test/e2e && go test -tags e2e -timeout 30m -v .
//
// The suite installs into an isolated distro and state dir, and removes both;
// a failure can leave the distro behind, in which case
// `skrog uninstall --state-dir <printed dir> --yes` cleans up.
package e2e

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	distro   = "skrog-e2e-suite"
	pipeName = `\\.\pipe\skrog-e2e-suite`
	// dockerHost matches pipeName in the form the docker CLI wants.
	dockerHost = "npipe:////./pipe/skrog-e2e-suite"
	// dockerPipePath is the same pipe as a path, for `docker run -v`.
	dockerPipePath = "//./pipe/skrog-e2e-suite"
)

// state carries everything the ordered stages share.
type state struct {
	workDir  string // parent-scoped scratch: subtest TempDirs die with the subtest
	skrog    string // built skrog.exe
	docker   string // resolved docker CLI
	stateDir string
	dataDir  string
	proxy    *exec.Cmd
	proxyLog *os.File

	// baselines captured before anything runs, compared after teardown.
	wslProcsBefore int
	ddWorkedBefore bool
	// skrogCtxBefore is the machine's own `skrog` docker context endpoint,
	// empty when it has none. The suite installs beside a real install and
	// shares that one global context with it (#217).
	skrogCtxBefore string
}

// TestAcceptance is one ordered scenario, not independent tests: install must
// precede use, use must precede uninstall, and the teardown assertions are
// meaningless unless the earlier stages actually ran. Sub-tests give per-stage
// reporting while a stage failure stops the sequence.
func TestAcceptance(t *testing.T) {
	if os.Getenv("OS") != "Windows_NT" {
		t.Skip("acceptance runs on Windows with WSL2")
	}
	s := &state{workDir: t.TempDir()}

	// Resolved up front so a missing CLI skips the suite rather than failing a
	// later stage: a skip inside a sub-stage does not stop the sequence.
	s.docker = findDocker()
	if s.docker == "" {
		t.Skip("no docker CLI found on PATH or in the usual install locations")
	}

	stages := []struct {
		name string
		fn   func(t *testing.T, s *state)
	}{
		{"Baselines", stageBaselines},
		{"BuildSkrog", stageBuild},
		{"InstallFromPublishedRelease", stageInstall},
		{"EnableAuditLog", stageEnableAudit},
		{"EnableHostCAImport", stageEnableHostCAs},
		{"StartProxy", stageProxy},
		{"StatusNamesTheServedEndpoint", stageStatusEndpoint},
		{"DoctorReportsHealthy", stageDoctor},
		{"RunnerCheckReports", stageRunnerCheck},
		{"DeclarativeExportAndConverge", stageDeclarative},
		{"EngineLockReflectsInstall", stageLock},
		{"AirGapBundlePacksRootfs", stageAirGap},
		{"HelloWorld", stageHelloWorld},
		{"PrewarmPullsPinnedImages", stagePrewarm},
		{"AuditLogRecordsCalls", stageAudit},
		{"AuditTogglesWithoutARestart", stageAuditToggle},
		{"HostCAsImportedIntoEngine", stageHostCAs},
		{"BindMountReadThroughContainer", stageBindMount},
		{"PublishedPortReachableFromWindows", stagePublishedPort},
		{"EnginePipeMountsAsDockerSocket", stageSocketMount},
		{"TestcontainersAgainstTheEngine", stageTestcontainers},
		{"ActRunsAWorkflowLocally", stageAct},
		{"GitLabPipelineRunsLocally", stageGitLabCILocal},
		{"DaggerPipelineRuns", stageDagger},
		{"BuildCacheExportsAndImports", stageBuildCache},
		{"KindClusterServesFromWindows", stageKind},
		{"ExecInRunningContainer", stageExec},
		{"StdinPipeIntoContainer", stageStdinPipe},
		{"LogsFollowStreams", stageLogsFollow},
		{"ComposeStack", stageCompose},
		{"DevContainerUpThroughPipe", stageDevContainer},
		{"DockerCLIBundleInstalls", stageDockerCLIBundle},
		{"GPUPassthroughIfPresent", stageGPU},
		{"PruneReclaimsAndReports", stagePrune},
		{"WSLIntegrateSharesEngine", stageWSLIntegrate},
		{"MigrateFromDesktop", stageMigrate},
		{"EngineConfigValidatesAndApplies", stageEngineConfig},
		{"NetworkProfilesSwitchEngineConfig", stageProfiles},
		{"LifecycleHooksFire", stageHooks},
		{"IdleStopAndOnDemandWake", stageIdle},
		{"InterruptedClientDoesNotWedgeBridge", stageInterrupt},
		{"SupervisorRestartReplacesTheProcess", stageSupervisorRestart},
		{"VsockPathServedEverything", stageVsockServed},
		{"RemoteEngineOverMutualTLS", stageServeMTLS},
		{"EngineSnapshotSaveAndList", stageSnapshot},
		{"UpgradeReportsCurrentAfterInstall", stageUpgradeCheck},
		{"Uninstall", stageUninstall},
		{"NothingLeftBehind", stageClean},
		{"DockerDesktopStillWorks", stageDesktopIntact},
	}

	// Teardown runs even on failure, so a broken run does not strand a distro.
	t.Cleanup(func() { forceCleanup(s) })

	for _, st := range stages {
		if !t.Run(st.name, func(t *testing.T) { st.fn(t, s) }) {
			t.Fatalf("stage %s failed; skipping the rest", st.name)
		}
	}
}

// run executes a command and returns trimmed combined output.
func run(t *testing.T, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	// wsl.exe writes its own messages as UTF-16LE -- distro lists, --version,
	// the "no installed distributions" notice -- so a raw CombinedOutput of
	// them lands in the test log with a NUL between every character:
	//
	//   W^@i^@n^@d^@o^@w^@s^@ ^@S^@u^@b^@s^@y^@s^@t^@e^@m^@...
	//
	// The suite calls wsl.exe directly in several places, and a failure
	// message nobody can read is a failure nobody can act on.
	//
	// WSL_UTF8=1 makes it emit UTF-8 (WSL 0.64.0+). Set for every command
	// rather than only the wsl ones: it is inert everywhere else, and a list
	// of "which commands are really wsl underneath" is the kind of thing that
	// goes stale silently.
	//
	// The product itself does not need this -- internal/wsl/decode.go detects
	// and converts UTF-16 already. This is for the test harness, which
	// bypasses that package by design: it drives the real binaries the way a
	// user does.
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("%s timed out after %s", name, timeout)
	}
}

// runEnv is run() with an explicit environment, for subprocesses that shell
// out to docker and thus need docker's credential helper on PATH (hostEnv adds
// docker's own directory). A real `skrog migrate` inherits a shell where
// Docker Desktop is on PATH; the suite must reproduce that for its subprocess.
func runEnv(t *testing.T, env []string, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("%s timed out after %s", name, timeout)
	}
}

func must(t *testing.T, out string, err error, what string) string {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v\n%s", what, err, out)
	}
	return out
}

// dockerE runs docker against the suite's engine, never the user's default.
func dockerE(t *testing.T, s *state, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(s.docker, args...)
	cmd.Env = s.dockerEnv()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("docker %s timed out after %s",
			strings.Join(args, " "), timeout)
	}
}

func wslProcCount(t *testing.T) int {
	t.Helper()
	out, _ := run(t, 30*time.Second, "tasklist", "/FI", "IMAGENAME eq wsl.exe", "/FO", "CSV", "/NH")
	if strings.Contains(out, "No tasks") {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func stageBaselines(t *testing.T, s *state) {
	if out, err := run(t, 30*time.Second, s.docker, "--version"); err != nil {
		t.Skipf("no docker CLI on PATH (%v: %s); the suite drives the engine through it", err, out)
	}
	s.wslProcsBefore = wslProcCount(t)
	t.Logf("wsl.exe baseline: %d", s.wslProcsBefore)

	// The `skrog` docker context is a single global object, and this suite
	// installs beside whatever is already on the machine. Recording where it
	// points is how the teardown can prove the suite handed it back (#217) —
	// before that fix, running the acceptance suite silently left the
	// developer's own `docker --context skrog` broken.
	s.skrogCtxBefore = skrogContextEndpoint(t, s)
	if s.skrogCtxBefore == "" {
		t.Log("no `skrog` docker context on this machine before the suite")
	} else {
		t.Logf("`skrog` docker context before: %s", s.skrogCtxBefore)
	}

	// Whether Docker Desktop worked BEFORE decides whether the intact-after
	// stage can claim anything. Recorded, not required.
	if out, err := s.runDocker(t, 60*time.Second, "--context", "desktop-linux", "version", "--format", "{{.Server.Version}}"); err == nil && out != "" {
		s.ddWorkedBefore = true
		t.Logf("Docker Desktop serving version %s", out)
	} else {
		t.Log("Docker Desktop not responding before the suite; the intact-check will be skipped")
	}
}

func stageBuild(t *testing.T, s *state) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	s.skrog = filepath.Join(s.workDir, "skrog.exe")
	cmd := exec.Command("go", "build", "-o", s.skrog, "./cmd/skrog")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building skrog.exe: %v\n%s", err, out)
	}
}

func stageInstall(t *testing.T, s *state) {
	s.stateDir = filepath.Join(s.workDir, "state")
	s.dataDir = filepath.Join(s.workDir, "data")

	// No rootfs flags: this installs what a user installs, verifying the
	// embedded manifest against the published release asset.
	out, err := run(t, 10*time.Minute, s.skrog, "install",
		"--distro", distro, "--state-dir", s.stateDir, "--data-dir", s.dataDir, "--headless")
	must(t, out, err, "skrog install")

	if !strings.Contains(out, "installed and running") {
		t.Errorf("install output does not report success:\n%s", out)
	}
}

func stageProxy(t *testing.T, s *state) {
	logPath := filepath.Join(s.stateDir, "proxy-e2e.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s.proxyLog = f

	// The bridge is `skrog supervise`, not `skrog proxy`: it is what real
	// installs run (autostart launches it), and it is where the idle/on-demand
	// behavior the IdleStop stage exercises lives. The suite's own pipe and
	// state dir keep a Skrog or Docker Desktop already on the machine
	// undisturbed — the state dir also scopes the supervisor's single-instance
	// mutex, so a real supervisor can coexist with the suite's.
	s.proxy = exec.Command(s.skrog, "supervise",
		"--distro", distro, "--state-dir", s.stateDir,
		"--pipe", pipeName, "--no-context")
	s.proxy.Stdout = f
	s.proxy.Stderr = f
	if err := s.proxy.Start(); err != nil {
		t.Fatalf("starting supervisor: %v", err)
	}

	// Up when the engine answers, not when the process exists.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if out, err := dockerE(t, s, 15*time.Second, "version", "--format", "{{.Server.Version}}"); err == nil && out != "" {
			t.Logf("engine answering: server %s", out)
			return
		}
		time.Sleep(2 * time.Second)
	}
	log, _ := os.ReadFile(logPath)
	t.Fatalf("engine never answered through the pipe. Proxy log:\n%s", log)
}

// stageStatusEndpoint asserts that `skrog status` names the pipe the running
// supervisor actually bound (#273).
//
// This stage is the negative control for the whole feature, for free. The suite
// runs the supervisor on `--pipe \\.\pipe\skrog-e2e-suite`, which is a name
// pipeproxy.SelectPipeName can never return on its own: asked fresh it answers
// docker_engine or skrog_engine. So an implementation that recomputed the
// endpoint instead of reading the supervisor's record would report one of those
// two here and fail — which is exactly the bug #273 warned about, since Docker
// Desktop can start or stop after Skrog chose.
func stageStatusEndpoint(t *testing.T, s *state) {
	out, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
	must(t, out, err, "skrog status --json")

	var st struct {
		Supervisor string `json:"supervisor"`
		Endpoint   *struct {
			Pipe       string `json:"pipe"`
			DockerHost string `json:"dockerHost"`
			Reason     string `json:"reason"`
		} `json:"endpoint"`
	}
	if jerr := json.Unmarshal([]byte(out), &st); jerr != nil {
		t.Fatalf("status --json unparseable: %v\n%s", jerr, out)
	}
	if st.Supervisor != "running" {
		t.Fatalf("supervisor = %q, want running — the stage before this one started it", st.Supervisor)
	}
	if st.Endpoint == nil {
		t.Fatalf("status reports no endpoint while the supervisor is running:\n%s", out)
	}
	if st.Endpoint.Pipe != pipeName {
		t.Errorf("endpoint.pipe = %q, want %q (the pipe the supervisor was told to bind)",
			st.Endpoint.Pipe, pipeName)
	}
	if st.Endpoint.DockerHost != dockerHost {
		t.Errorf("endpoint.dockerHost = %q, want %q", st.Endpoint.DockerHost, dockerHost)
	}
	if st.Endpoint.Reason == "" {
		t.Error("endpoint.reason is empty; it should say why this pipe was chosen")
	}

	// The human output carries it too: the point of #273 is that the answer
	// must be available without --json and without asking docker.
	text, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir)
	must(t, text, err, "skrog status")
	for _, want := range []string{pipeName, dockerHost} {
		if !strings.Contains(text, want) {
			t.Errorf("`skrog status` does not mention %q:\n%s", want, text)
		}
	}

}

// stageDoctor runs `skrog doctor --json` against the live install and asserts
// the checks that describe a healthy engine all pass. It does not assert the
// overall exit code: host-specific checks (which docker.exe is on PATH, whether
// a credential helper resolves, the machine's default docker context) depend on
// the developer's or CI runner's environment, not on Skrog, so pinning them
// would make the suite flaky. The engine-shaped checks are what doctor owns.
func stageDoctor(t *testing.T, s *state) {
	// doctor exits non-zero when any check fails (e.g. no docker.exe on the CI
	// runner's PATH), but still writes the full JSON report to stdout, so parse
	// the output regardless of the exit error.
	out, _ := run(t, 60*time.Second, s.skrog, "doctor", "--json", "--state-dir", s.stateDir)
	var rep struct {
		Results []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json unparseable: %v\n%s", err, out)
	}

	status := map[string]string{}
	for _, r := range rep.Results {
		status[r.Name] = r.Status
	}

	// With the supervisor up and the engine answering, these must be ok.
	for _, name := range []string{"wsl", "engine", "supervisor"} {
		if status[name] != "ok" {
			t.Errorf("doctor check %q = %q, want ok\nfull report:\n%s", name, status[name], out)
		}
	}
	// Disk should never fail on a machine that just installed the engine.
	if status["disk"] == "fail" {
		t.Errorf("doctor disk check failed unexpectedly\nfull report:\n%s", out)
	}
}

// stageEngineConfig exercises the validated daemon.json surface (#68): set a
// real engine key, confirm it lands in daemon.json and the engine bounces back
// healthy, then clear it. The set goes through `dockerd --validate` and a
// supervisor-cooperative restart, so a green run proves that whole chain end to
// end against the live engine.
func stageEngineConfig(t *testing.T, s *state) {
	set := func(key, value string) (string, error) {
		return run(t, 3*time.Minute, s.skrog, "config", "--state-dir", s.stateDir, "set", key, value)
	}
	get := func(key string) string {
		out, err := run(t, 30*time.Second, s.skrog, "config", "--state-dir", s.stateDir, "get", key)
		must(t, out, err, "config get "+key)
		return strings.TrimSpace(out)
	}
	engineUp := func() bool {
		out, err := dockerE(t, s, 30*time.Second, "version", "--format", "{{.Server.Version}}")
		return err == nil && out != ""
	}

	out, err := set("engine.max-concurrent-downloads", "5")
	must(t, out, err, "config set engine.max-concurrent-downloads 5")
	if got := get("engine.max-concurrent-downloads"); got != "5" {
		t.Fatalf("get after set = %q, want 5", got)
	}
	if !engineUp() {
		t.Fatal("engine did not answer after applying engine config")
	}

	// Clearing with an empty value removes the key and bounces the engine again.
	out, err = set("engine.max-concurrent-downloads", "")
	must(t, out, err, "config set engine.max-concurrent-downloads (clear)")
	if got := get("engine.max-concurrent-downloads"); got != "" {
		t.Fatalf("get after clear = %q, want empty", got)
	}
	if !engineUp() {
		t.Fatal("engine did not answer after clearing engine config")
	}
}

// stageHooks proves the lifecycle hooks (#70): configure a post-start and a
// pre-stop script, bounce the engine through the supervisor, and confirm both
// scripts ran. The hooks fire inside the running supervisor process, so a green
// run exercises the real path a user gets, not a unit stub.
func stageHooks(t *testing.T, s *state) {
	postMarker := filepath.Join(s.workDir, "poststart.marker")
	preMarker := filepath.Join(s.workDir, "prestop.marker")
	os.Remove(postMarker)
	os.Remove(preMarker)

	writeScript := func(name, marker string) string {
		p := filepath.Join(s.workDir, name)
		// A .cmd so config-set's existence check and the supervisor's extension
		// dispatch both apply; it just drops a marker file.
		body := "@echo off\r\necho fired> \"" + marker + "\"\r\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	postScript := writeScript("post-start.cmd", postMarker)
	preScript := writeScript("pre-stop.cmd", preMarker)

	setHook := func(key, val string) {
		out, err := run(t, 30*time.Second, s.skrog, "config", "--state-dir", s.stateDir, "set", key, val)
		must(t, out, err, "config set "+key)
	}
	setHook("hook.post-start", postScript)
	setHook("hook.pre-stop", preScript)
	defer setHook("hook.post-start", "")
	defer setHook("hook.pre-stop", "")

	// Bounce through the supervisor so it observes stop (pre-stop) and start
	// (post-start) transitions and fires both hooks.
	out, err := run(t, 3*time.Minute, s.skrog, "restart", "--state-dir", s.stateDir)
	must(t, out, err, "skrog restart")

	waitFile := func(path, which string) {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		log, _ := os.ReadFile(filepath.Join(s.stateDir, "proxy-e2e.log"))
		tail := string(log)
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		t.Fatalf("%s hook never ran (no marker %s)\nsupervisor log tail:\n%s", which, path, tail)
	}
	waitFile(preMarker, "pre-stop")
	waitFile(postMarker, "post-start")
}

// stageDeclarative proves the infrastructure-as-code loop (#69): export the
// live install as a skrog.yaml, then feed it back to `install --config` and
// confirm it converges idempotently (skips provisioning, engine stays healthy)
// rather than erroring on the already-installed distro.
func stageDeclarative(t *testing.T, s *state) {
	out, err := run(t, 60*time.Second, s.skrog, "config", "--state-dir", s.stateDir, "export")
	must(t, out, err, "config export")
	if !strings.Contains(out, "distro: "+distro) {
		t.Fatalf("exported YAML missing the distro:\n%s", out)
	}
	if !strings.Contains(out, "engine-version:") {
		t.Fatalf("exported YAML missing the engine version:\n%s", out)
	}

	yamlPath := filepath.Join(s.workDir, "skrog.yaml")
	if err := os.WriteFile(yamlPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-apply the exported file: already installed, so this must converge, not
	// reinstall. --no-autostart keeps it from touching the registry.
	out, err = run(t, 3*time.Minute, s.skrog, "install", "--config", yamlPath,
		"--state-dir", s.stateDir, "--no-autostart")
	must(t, out, err, "install --config (idempotent converge)")

	if v, err := dockerE(t, s, 30*time.Second, "version", "--format", "{{.Server.Version}}"); err != nil || v == "" {
		t.Fatalf("engine not healthy after declarative converge: %v (%s)", err, v)
	}
}

// stageLock proves `skrog lock` (#74) emits a lock that pins the same engine
// the machine is actually running: the reproducible-install guarantee is only
// real if the lock reflects reality, so it is cross-checked against
// `skrog version --json`.
func stageLock(t *testing.T, s *state) {
	lockPath := filepath.Join(s.workDir, "skrog.lock")
	out, err := run(t, 60*time.Second, s.skrog, "lock", "-o", lockPath)
	must(t, out, err, "skrog lock")

	b, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lk struct {
		EngineVersion string `json:"engineVersion"`
		Rootfs        struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(b, &lk); err != nil {
		t.Fatalf("lock is not valid JSON: %v\n%s", err, b)
	}
	if !strings.Contains(lk.Rootfs.URL, "rootfs") || len(lk.Rootfs.SHA256) != 64 {
		t.Fatalf("lock rootfs looks wrong: %+v", lk.Rootfs)
	}

	vout, err := run(t, 30*time.Second, s.skrog, "version", "--state-dir", s.stateDir, "--json")
	must(t, vout, err, "skrog version --json")
	var v struct {
		Engine struct {
			Version      string `json:"version"`
			RootfsSHA256 string `json:"rootfsSha256"`
		} `json:"engine"`
	}
	if err := json.Unmarshal([]byte(vout), &v); err != nil {
		t.Fatalf("version --json unparseable: %v\n%s", err, vout)
	}
	if lk.EngineVersion != v.Engine.Version {
		t.Errorf("lock engine %q != installed engine %q", lk.EngineVersion, v.Engine.Version)
	}
	if lk.Rootfs.SHA256 != v.Engine.RootfsSHA256 {
		t.Errorf("lock rootfs sha %q != installed rootfs sha %q", lk.Rootfs.SHA256, v.Engine.RootfsSHA256)
	}
}

// stageProfiles proves the headline of network profiles (#73): a profile
// captures engine config, and switching to it actually applies that config to
// the live engine — the thing every other tool makes you hand-toggle. It also
// checks that `skrog status` names the active profile.
func stageProfiles(t *testing.T, s *state) {
	cfg := func(args ...string) (string, error) {
		return run(t, 3*time.Minute, s.skrog, append([]string{"config", "--state-dir", s.stateDir}, args...)...)
	}
	prof := func(args ...string) (string, error) {
		return run(t, 3*time.Minute, s.skrog, append([]string{"profile", "--state-dir", s.stateDir}, args...)...)
	}
	getDL := func() string {
		out, err := cfg("get", "engine.max-concurrent-downloads")
		must(t, out, err, "config get engine.max-concurrent-downloads")
		return strings.TrimSpace(out)
	}

	// Capture a distinctive engine setting into a "vpn" profile.
	out, err := cfg("set", "engine.max-concurrent-downloads", "7")
	must(t, out, err, "set engine.max-concurrent-downloads 7")
	out, err = prof("create", "vpn")
	must(t, out, err, "profile create vpn")

	// Change the live config, then switch back via the profile and confirm the
	// profile's value was actually re-applied to the engine.
	out, err = cfg("set", "engine.max-concurrent-downloads", "3")
	must(t, out, err, "set engine.max-concurrent-downloads 3")
	if got := getDL(); got != "3" {
		t.Fatalf("precondition: expected 3, got %q", got)
	}

	out, err = prof("switch", "vpn")
	must(t, out, err, "profile switch vpn")
	if got := getDL(); got != "7" {
		t.Fatalf("switch did not re-apply the profile's engine config: got %q, want 7", got)
	}

	// status names the active profile.
	sout, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
	must(t, sout, err, "status --json")
	var st struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal([]byte(sout), &st); err != nil {
		t.Fatalf("status --json unparseable: %v\n%s", err, sout)
	}
	if st.Profile != "vpn" {
		t.Errorf("status profile = %q, want vpn", st.Profile)
	}

	if out, err := prof("list"); err != nil || !strings.Contains(out, "vpn") {
		t.Fatalf("profile list missing vpn: %v\n%s", err, out)
	}

	// Clean up: remove the profile and the setting so later stages start clean.
	prof("delete", "vpn")
	cfg("set", "engine.max-concurrent-downloads", "")
}

// stageAirGap proves `skrog bundle` (#75) packs a self-contained, verified
// archive: the rootfs plus a skrog.lock. It reuses the rootfs already cached in
// the state dir, so it does not re-download. The full offline install path is
// verified separately (it reuses the same checksum-verified local-rootfs import
// a networked install uses); here we assert the bundle a connected machine
// produces is complete and self-describing.
func stageAirGap(t *testing.T, s *state) {
	bundlePath := filepath.Join(s.workDir, "bundle.zip")
	out, err := run(t, 5*time.Minute, s.skrog, "bundle", "-o", bundlePath, "--state-dir", s.stateDir)
	must(t, out, err, "skrog bundle")

	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		t.Fatalf("bundle is not a valid zip: %v", err)
	}
	defer zr.Close()

	var lockData []byte
	var rootfsSize uint64
	for _, f := range zr.File {
		switch {
		case f.Name == "skrog.lock":
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			lockData = readAll(t, rc)
			rc.Close()
		case strings.HasSuffix(f.Name, ".tar.gz"):
			rootfsSize = f.UncompressedSize64
		}
	}

	if lockData == nil {
		t.Fatal("bundle is missing skrog.lock")
	}
	var lk struct {
		EngineVersion string `json:"engineVersion"`
		Rootfs        struct {
			SHA256 string `json:"sha256"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(lockData, &lk); err != nil {
		t.Fatalf("bundle lock is not valid JSON: %v\n%s", err, lockData)
	}
	if lk.EngineVersion == "" || len(lk.Rootfs.SHA256) != 64 {
		t.Fatalf("bundle lock looks wrong: %+v", lk)
	}
	if rootfsSize < 1<<20 {
		t.Fatalf("bundle rootfs is implausibly small (%d bytes)", rootfsSize)
	}
}

func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }) []byte {
	t.Helper()
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// stageEnableAudit turns on the audit log before the supervisor starts, so the
// bridge records the container-affecting calls the later stages make. The
// handler is chosen at supervise start, which is why this runs before StartProxy.
func stageEnableAudit(t *testing.T, s *state) {
	out, err := run(t, 30*time.Second, s.skrog, "config", "--state-dir", s.stateDir, "set", "audit", "on")
	must(t, out, err, "config set audit on")
}

// stageAudit verifies the audit log (#121) captured HelloWorld's activity: a
// fresh engine pulls hello-world, then creates and starts a container — each a
// JSON-lines record the bridge wrote.
func stageAudit(t *testing.T, s *state) {
	var out string
	deadline := time.Now().Add(15 * time.Second)
	for {
		o, err := run(t, 30*time.Second, s.skrog, "audit", "--state-dir", s.stateDir, "tail")
		must(t, o, err, "audit tail")
		out = o
		if strings.Contains(out, "container-create") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit log never recorded a container-create:\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}

	for _, want := range []string{"image-pull", "container-create", "container-start"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log missing a %q record:\n%s", want, out)
		}
	}
	// Every line is a well-formed JSON event with an action.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev struct {
			Action string `json:"action"`
			Time   string `json:"time"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" || ev.Time == "" {
			t.Fatalf("audit line is not a valid event: %q (%v)", line, err)
		}
	}

	// `audit trace` (#152): run a docker command under the trace and check the
	// summary attributes its container to it. hello-world is already pulled, so
	// the expected records are create/start, not a pull. The child inherits the
	// suite's DOCKER_HOST so it reaches the suite's engine.
	out, err := runEnv(t, s.dockerEnv(), 3*time.Minute, s.skrog,
		"audit", "--state-dir", s.stateDir, "--json", "trace", "--", s.docker, "run", "--rm", "hello-world")
	must(t, out, err, "audit trace")
	// The traced command prints to the same stdout; the JSON summary is the
	// trailing object.
	i := strings.LastIndex(out, "\n{")
	if i < 0 {
		t.Fatalf("no JSON summary in trace output:\n%s", out)
	}
	var sum struct {
		ExitCode int            `json:"exitCode"`
		Events   int            `json:"events"`
		Actions  map[string]int `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out[i+1:]), &sum); err != nil {
		t.Fatalf("trace summary is not JSON: %v\n%s", err, out[i+1:])
	}
	if sum.ExitCode != 0 || sum.Events == 0 || sum.Actions["container-create"] < 1 {
		t.Fatalf("trace summary did not attribute the container run: %+v", sum)
	}
	t.Logf("audit trace attributed %d events (%d container-create) to the traced docker run", sum.Events, sum.Actions["container-create"])
}

// stageSnapshot exercises `skrog snapshot` (#122): a real `wsl --export` of the
// engine distro with a recorded checksum, listed and deleted. The full restore
// (unregister + re-import) is destructive and verified separately on a throwaway
// distro; here we prove save/list/delete against the live engine near the end of
// the run, where the export's brief engine bounce disturbs nothing (Uninstall is
// next). Flags come before the subcommand, as the CLI expects.
func stageSnapshot(t *testing.T, s *state) {
	out, err := run(t, 3*time.Minute, s.skrog, "snapshot", "--state-dir", s.stateDir, "--force", "save", "e2e")
	must(t, out, err, "snapshot save")

	out, err = run(t, 30*time.Second, s.skrog, "snapshot", "--state-dir", s.stateDir, "list")
	must(t, out, err, "snapshot list")
	if !strings.Contains(out, "e2e") {
		t.Fatalf("snapshot list missing the saved snapshot:\n%s", out)
	}

	// The archive and its checksum sidecar are on disk.
	if _, err := os.Stat(filepath.Join(s.stateDir, "snapshots", "e2e.tar")); err != nil {
		t.Errorf("snapshot archive not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "snapshots", "e2e.json")); err != nil {
		t.Errorf("snapshot metadata not written: %v", err)
	}

	// `skrog reset --to` (#142): the runner's clean slate. Resetting to the
	// snapshot just taken is non-destructive in effect and exercises the whole
	// path — verify, unregister, import, engine back — with a real timing that
	// the log records for the fast-restore follow-up.
	out, err = run(t, 5*time.Minute, s.skrog, "reset", "--state-dir", s.stateDir, "--json", "--to", "e2e")
	must(t, out, err, "skrog reset --to e2e")
	// Supervisor log lines may precede the JSON on the combined stream.
	if i := strings.Index(out, "{"); i >= 0 {
		out = out[i:]
	}
	var rs struct {
		Snapshot string `json:"snapshot"`
		Millis   int64  `json:"ms"`
	}
	if err := json.Unmarshal([]byte(out), &rs); err != nil || rs.Snapshot != "e2e" {
		t.Fatalf("reset --json unexpected (%v):\n%s", err, out)
	}
	if v, err := dockerE(t, s, 2*time.Minute, "version", "--format", "{{.Server.Version}}"); err != nil || v == "" {
		t.Fatalf("engine did not answer after reset: %v\n%s", err, v)
	}
	t.Logf("reset to snapshot in %d ms; engine answering", rs.Millis)

	out, err = run(t, 30*time.Second, s.skrog, "snapshot", "--state-dir", s.stateDir, "delete", "e2e")
	must(t, out, err, "snapshot delete")
	out, _ = run(t, 30*time.Second, s.skrog, "snapshot", "--state-dir", s.stateDir, "list")
	if strings.Contains(out, "e2e") {
		t.Errorf("snapshot still listed after delete:\n%s", out)
	}
}

// stageEnableHostCAs turns on host-CA import before the supervisor starts, so
// the engine trusts the host's roots (#62). Set here (like the audit log)
// because the network config is read at supervise start. Harmless to the rest
// of the run: it only adds trusted roots.
func stageEnableHostCAs(t *testing.T, s *state) {
	out, err := run(t, 30*time.Second, s.skrog, "config", "--state-dir", s.stateDir, "set", "network.import-host-cas", "on")
	must(t, out, err, "config set network.import-host-cas on")
}

// stageHostCAs verifies the host CA import landed in the engine's trust store.
func stageHostCAs(t *testing.T, s *state) {
	// One file per imported cert lands under the CA source dir (Alpine splits them).
	out, err := run(t, 60*time.Second, "wsl.exe", "-d", distro, "-u", "root", "sh", "-c",
		"find /usr/local/share/ca-certificates -name 'skrog-host-*.crt' | wc -l")
	if err != nil {
		t.Fatalf("checking imported CAs: %v\n%s", err, out)
	}
	out = strings.TrimSpace(strings.ReplaceAll(out, "\x00", ""))
	if out == "0" || out == "" {
		t.Fatalf("no host CA certificates imported into the engine (got %q)", out)
	}
	t.Logf("engine now trusts %s imported host CA certificate(s)", out)
}

// stageDevContainer proves the Dev Containers CLI works against the Skrog
// engine with no shim (#67): `devcontainer up` builds and starts a container —
// including a Windows-path workspace bind mount the bridge rewrites — and
// `devcontainer exec` runs a command inside it. Skipped when the CLI is not
// installed (it needs Node + @devcontainers/cli), so CI without them is fine.
func stageDevContainer(t *testing.T, s *state) {
	dc, err := exec.LookPath("devcontainer")
	if err != nil {
		t.Skip("devcontainer CLI not on PATH; install @devcontainers/cli to exercise this")
	}
	ws := filepath.Join(s.workDir, "dcws")
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "devcontainer.json"),
		[]byte(`{"image":"alpine:3.20","runArgs":["--label","skrog-dc-e2e"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// devcontainer shells out to docker; point both at the suite's engine.
	env := append(os.Environ(),
		"DOCKER_HOST="+dockerHost,
		"PATH="+filepath.Dir(s.docker)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	defer func() { // best-effort teardown of the dev container
		ids, _ := dockerE(t, s, 30*time.Second, "ps", "-aq", "--filter", "label=skrog-dc-e2e")
		for _, id := range strings.Fields(ids) {
			dockerE(t, s, 30*time.Second, "rm", "-f", id)
		}
	}()

	out, err := runEnv(t, env, 5*time.Minute, dc, "up", "--workspace-folder", ws)
	must(t, out, err, "devcontainer up")
	if !strings.Contains(out, `"outcome":"success"`) {
		t.Fatalf("devcontainer up did not succeed:\n%s", out)
	}

	out, err = runEnv(t, env, 2*time.Minute, dc, "exec", "--workspace-folder", ws,
		"sh", "-c", "echo dc-exec-ok && cat /etc/alpine-release")
	must(t, out, err, "devcontainer exec")
	if !strings.Contains(out, "dc-exec-ok") {
		t.Fatalf("devcontainer exec output unexpected:\n%s", out)
	}
	t.Logf("Dev Containers CLI ran through the Skrog pipe")
}

// stageServeMTLS proves the remote-engine door (#123): mint the mutual-TLS
// material, run `skrog serve --tcp` on loopback, and reach the engine with a
// stock docker client over TCP+TLS — then confirm a client WITHOUT the signed
// certificate is refused, which is the whole guarantee.
func stageServeMTLS(t *testing.T, s *state) {
	const addr = "127.0.0.1:52376"

	out, err := run(t, 60*time.Second, s.skrog, "serve", "cert",
		"--state-dir", s.stateDir, "--host", "127.0.0.1")
	must(t, out, err, "skrog serve cert")

	tlsDir := filepath.Join(s.stateDir, "tls")
	ca := filepath.Join(tlsDir, "ca.pem")
	cert := filepath.Join(tlsDir, "client.pem")
	key := filepath.Join(tlsDir, "client-key.pem")
	for _, f := range []string{ca, cert, key, filepath.Join(tlsDir, "server.pem")} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected TLS material %s: %v", f, err)
		}
	}

	// Run the server in the background; it serves until killed.
	srv := exec.Command(s.skrog, "serve", "--state-dir", s.stateDir, "--tcp", addr)
	if err := srv.Start(); err != nil {
		t.Fatalf("starting skrog serve: %v", err)
	}
	defer func() {
		if srv.Process != nil {
			srv.Process.Kill()
		}
		srv.Wait()
	}()

	// Poll the TLS endpoint until the listener is up (or give up).
	host := "tcp://" + addr
	var verOut string
	deadline := time.Now().Add(30 * time.Second)
	for {
		verOut, err = run(t, 30*time.Second, s.docker, "-H", host,
			"--tlsverify", "--tlscacert", ca, "--tlscert", cert, "--tlskey", key,
			"version", "--format", "{{.Server.Version}}")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("docker over mutual TLS never succeeded: %v\n%s", err, verOut)
		}
		time.Sleep(time.Second)
	}
	if verOut == "" {
		t.Fatalf("engine reported no version over TLS:\n%s", verOut)
	}
	t.Logf("reached the engine over mutual TLS; server version %s", verOut)

	// The mutual half: a client that presents no certificate must be refused.
	out, err = run(t, 20*time.Second, s.docker, "-H", host,
		"--tlsverify", "--tlscacert", ca,
		"version", "--format", "{{.Server.Version}}")
	if err == nil {
		t.Fatalf("engine answered a client with NO certificate — mutual TLS is not enforced:\n%s", out)
	}
	t.Logf("client without a signed certificate correctly refused")

	// The client side (#138): register the loopback server as a remote, prove
	// the round trip through the docker context it creates, and clean up.
	// `remote use` is deliberately not exercised: it would switch the machine's
	// current docker context, which the suite must leave alone.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Log("no docker on PATH for the remote client round trip; skipping that part")
		return
	}
	out, err = run(t, 60*time.Second, s.skrog, "remote", "--state-dir", s.stateDir,
		"--host", host, "--certs", tlsDir, "add", "e2e-loop")
	must(t, out, err, "skrog remote add")
	defer run(t, 30*time.Second, s.skrog, "remote", "--state-dir", s.stateDir, "remove", "e2e-loop")

	out, err = run(t, 30*time.Second, s.skrog, "remote", "--state-dir", s.stateDir, "--json", "list")
	must(t, out, err, "skrog remote list --json")
	if !strings.Contains(out, `"e2e-loop"`) {
		t.Fatalf("remote list --json does not show the remote:\n%s", out)
	}

	out, err = run(t, 60*time.Second, s.skrog, "remote", "--state-dir", s.stateDir, "test", "e2e-loop")
	must(t, out, err, "skrog remote test")
	if !strings.Contains(out, verOut) {
		t.Errorf("remote test did not report server version %q:\n%s", verOut, out)
	}
	t.Logf("remote client round trip through context skrog-e2e-loop OK")
}

// stageDockerCLIBundle proves `skrog cli install` (#66) fetches and installs
// the pinned upstream docker CLI + compose + buildx, checksum-verified, and that
// each one runs. It keeps the embedded pins honest: a rotted URL or a drifted
// checksum fails here. PATH is left alone (--no-path) and the plugins land in a
// throwaway DOCKER_CONFIG so the machine's real environment is untouched.
func stageDockerCLIBundle(t *testing.T, s *state) {
	cfg := filepath.Join(s.workDir, "clibundle-dockercfg")
	env := append(os.Environ(), "DOCKER_CONFIG="+cfg)

	out, err := runEnv(t, env, 5*time.Minute, s.skrog, "cli", "install",
		"--no-path", "--state-dir", s.stateDir)
	must(t, out, err, "skrog cli install")

	docker := filepath.Join(s.stateDir, "bin", "docker.exe")
	if _, err := os.Stat(docker); err != nil {
		t.Fatalf("bundled docker.exe not installed: %v", err)
	}

	// Each tool must run. These are client-only (no engine needed), so they
	// pass regardless of engine state.
	for _, tc := range []struct {
		what string
		args []string
		want string
	}{
		{"docker --version", []string{"--version"}, "Docker version"},
		{"docker compose version", []string{"compose", "version"}, "Docker Compose"},
		{"docker buildx version", []string{"buildx", "version"}, "buildx"},
	} {
		out, err := runEnv(t, env, time.Minute, docker, tc.args...)
		must(t, out, err, tc.what)
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s output %q does not contain %q", tc.what, out, tc.want)
		}
	}
	// The credential helper is placed on the bin dir (no `list` here — that would
	// read the machine's real stored credentials).
	if _, err := os.Stat(filepath.Join(s.stateDir, "bin", "docker-credential-wincred.exe")); err != nil {
		t.Errorf("credential helper not installed: %v", err)
	}
	t.Logf("docker CLI bundle installed and every tool runs")
}

// stageGPU proves NVIDIA GPU passthrough (#83) end to end when the machine has
// an NVIDIA GPU, and skips cleanly when it does not (CI, AMD/Intel). It enables
// GPU, checks the CDI device registered, and runs nvidia-smi in a container via
// `--device nvidia.com/gpu=all` — the whole point of the feature.
func stageGPU(t *testing.T, s *state) {
	out, err := run(t, 60*time.Second, "wsl.exe", "-d", distro, "-u", "root", "sh", "-c",
		"[ -e /dev/dxg ] && [ -e /usr/lib/wsl/lib/libcuda.so.1 ] && echo gpu-ok")
	if err != nil || !strings.Contains(out, "gpu-ok") {
		t.Skip("no NVIDIA GPU visible to WSL; skipping GPU passthrough")
	}

	out, err = run(t, 60*time.Second, s.skrog, "enable-gpu", "--state-dir", s.stateDir, "--distro", distro)
	must(t, out, err, "skrog enable-gpu")
	defer run(t, 30*time.Second, s.skrog, "enable-gpu", "--off", "--state-dir", s.stateDir, "--distro", distro)

	// The CDI device must be registered with the engine.
	info, err := dockerE(t, s, 30*time.Second, "info")
	must(t, info, err, "docker info")
	if !strings.Contains(info, "nvidia.com/gpu") {
		t.Fatalf("engine did not register the nvidia CDI device:\n%s", info)
	}

	// A container must see the GPU through the hookless CDI spec. ubuntu (glibc)
	// runs the WSL-injected nvidia-smi.
	out, err = dockerE(t, s, 5*time.Minute, "run", "--rm", "--device", "nvidia.com/gpu=all",
		"ubuntu:22.04", "nvidia-smi", "-L")
	must(t, out, err, "docker run --device nvidia.com/gpu=all nvidia-smi")
	if !strings.Contains(out, "GPU 0") {
		t.Fatalf("nvidia-smi listed no GPU in the container:\n%s", out)
	}
	t.Logf("GPU reachable in a container: %s", strings.TrimSpace(out))

	// `--gpus all` (#139) needs nvidia-cdi-hook present when dockerd started;
	// rootfs 29.7.2-4+ ships it. Assert it only where the rootfs has it, so the
	// suite stays honest on an older published rootfs.
	hook, _ := run(t, 30*time.Second, "wsl.exe", "-d", distro, "-u", "root", "sh", "-c",
		"command -v nvidia-cdi-hook >/dev/null 2>&1 && echo hook-present")
	if !strings.Contains(hook, "hook-present") {
		t.Log("rootfs has no nvidia-cdi-hook; --gpus all not asserted (use --device nvidia.com/gpu=all)")
		return
	}
	out, err = dockerE(t, s, 3*time.Minute, "run", "--rm", "--gpus", "all", "ubuntu:22.04", "nvidia-smi", "-L")
	must(t, out, err, "docker run --gpus all nvidia-smi")
	if !strings.Contains(out, "GPU 0") {
		t.Fatalf("--gpus all did not reach the GPU:\n%s", out)
	}
	t.Logf("--gpus all routed to the CDI spec: %s", strings.TrimSpace(out))
}

// stagePrewarm proves `skrog prewarm` (#149) pulls a pinned list through
// whatever docker targets — here the suite's engine via DOCKER_HOST — and
// reports per-image results. hello-world is already present; alpine:latest is
// new, and pre-pulling it here is exactly the warm-up the later stages enjoy.
func stagePrewarm(t *testing.T, s *state) {
	list := filepath.Join(s.workDir, "images.txt")
	if err := os.WriteFile(list, []byte("# pinned by the suite\nhello-world\nalpine:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runEnv(t, s.dockerEnv(), 5*time.Minute, s.skrog, "prewarm", "--json", list)
	must(t, out, err, "skrog prewarm")
	var res struct {
		Pulled int `json:"pulled"`
		Failed int `json:"failed"`
		Images []struct {
			Ref string `json:"ref"`
			OK  bool   `json:"ok"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("prewarm --json is not JSON: %v\n%s", err, out)
	}
	if res.Pulled != 2 || res.Failed != 0 || len(res.Images) != 2 {
		t.Fatalf("prewarm result unexpected: %+v", res)
	}
	t.Logf("prewarm pulled %d pinned images through the suite's engine", res.Pulled)
}

// stageRunnerCheck proves `skrog runner check` (#150) evaluates the machine
// and reports findings in the documented shape, with the exit code agreeing
// with the verdict. The suite machine is normally not a runner (no auto-logon,
// and the suite installs with --no-autostart), so the expected verdict is "not
// ready" — but the assertions hold either way, so a real runner passes too.
func stageRunnerCheck(t *testing.T, s *state) {
	out, err := run(t, 60*time.Second, s.skrog, "runner", "--state-dir", s.stateDir, "--json", "check")
	// A non-zero exit is the expected verdict here, not a failure of the command.
	var res struct {
		Ready    bool `json:"ready"`
		Findings []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"findings"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("runner check --json is not JSON (%v): %v\n%s", jerr, err, out)
	}
	names := map[string]bool{}
	for _, f := range res.Findings {
		names[f.Name] = true
		if f.Status != "ok" && f.Status != "warn" && f.Status != "fail" {
			t.Errorf("finding %s has status %q", f.Name, f.Status)
		}
	}
	for _, want := range []string{"autologon", "autostart", "supervisor", "engine"} {
		if !names[want] {
			t.Errorf("runner check is missing the %q finding: %+v", want, res.Findings)
		}
	}
	if res.Ready != (err == nil) {
		t.Errorf("exit code disagrees with the verdict: ready=%v err=%v", res.Ready, err)
	}
	t.Logf("runner check: ready=%v, %d findings", res.Ready, len(res.Findings))
}

// stagePrune proves `skrog prune` (#145) runs against the suite's engine and
// reports per-step results. Earlier stages leave stopped containers and
// dangling layers behind, so there is usually something to reclaim — but the
// assertion is on shape and success, not a byte count. Tagged images survive
// (no --all), so later stages keep hello-world and alpine.
func stagePrune(t *testing.T, s *state) {
	out, err := runEnv(t, s.dockerEnv(), 3*time.Minute, s.skrog, "prune", "--json", "--build-cache")
	must(t, out, err, "skrog prune")
	var res struct {
		ReclaimedBytes int64 `json:"reclaimedBytes"`
		Failed         int   `json:"failed"`
		Steps          []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("prune --json is not JSON: %v\n%s", err, out)
	}
	if res.Failed != 0 || len(res.Steps) != 3 {
		t.Fatalf("prune result unexpected: %+v", res)
	}
	t.Logf("prune reclaimed %d bytes over %d steps", res.ReclaimedBytes, len(res.Steps))
}

func stageHelloWorld(t *testing.T, s *state) {
	out, err := dockerE(t, s, 5*time.Minute, "run", "--rm", "hello-world")
	must(t, out, err, "docker run hello-world")
	if !strings.Contains(out, "Hello from Docker!") {
		t.Errorf("unexpected hello-world output:\n%s", out)
	}
}

func stageBindMount(t *testing.T, s *state) {
	// The #7 chain end to end: a Windows path, rewritten by the proxy,
	// automounted by WSL, read by a container. Every link has failed at least
	// once in isolation; this is the only test that exercises them together.
	dir := t.TempDir()
	const proof = "bind-mount-proof-e2e"
	if err := os.WriteFile(filepath.Join(dir, "proof.txt"), []byte(proof), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := dockerE(t, s, 3*time.Minute, "run", "--rm",
		"-v", dir+":/data:ro", "alpine:latest", "cat", "/data/proof.txt")
	must(t, out, err, "bind mount read")
	if !strings.Contains(out, proof) {
		t.Errorf("container read %q, want %q — path translation or automount broke", out, proof)
	}
}

// stagePublishedPort proves `docker run -p` is reachable from Windows. It is
// the check that would have caught the userland-proxy trap: with docker-proxy
// relaying, a connection from the Windows side arrives in the container with a
// 127.0.0.1 source under mirrored networking and the reply never returns, so
// `curl localhost:<port>` hangs while everything else looks healthy.
func stagePublishedPort(t *testing.T, s *state) {
	const port = "58080"
	if out, err := dockerE(t, s, 3*time.Minute, "run", "-d", "--name", "e2e-port",
		"-p", port+":80", "nginx:alpine"); err != nil {
		t.Fatalf("starting nginx: %v\n%s", err, out)
	}
	defer dockerE(t, s, time.Minute, "rm", "-f", "e2e-port")

	// nginx needs a moment; poll rather than sleep a fixed amount.
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1:" + port + "/")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if !strings.Contains(string(body), "nginx") {
					t.Errorf("published port answered, but not with nginx: %q", body)
				}
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("published port %s never answered from Windows: %v "+
		"(engine.userland-proxy must be false for -p to work under mirrored networking)", port, lastErr)
}

// stageSocketMount proves the engine's own pipe can be mounted as
// /var/run/docker.sock, which is how Testcontainers' Ryuk, docker-in-docker
// helpers and CI images that talk to "the local engine" are wired (#144). The
// proxy maps the Windows pipe to the engine's socket inside the distro; without
// that, the daemon sees a UNC path it cannot bind-mount.
func stageSocketMount(t *testing.T, s *state) {
	out, err := dockerE(t, s, 3*time.Minute, "run", "--rm",
		"-v", dockerPipePath+":/var/run/docker.sock",
		"docker:cli", "version", "--format", "{{.Server.Version}}")
	must(t, out, err, "docker CLI in a container through the mounted engine socket")
	if !strings.Contains(out, "29.") {
		t.Errorf("containerized docker CLI reported server %q, want an engine version", out)
	}
}

// stageTestcontainers runs the Testcontainers acceptance module (a separate Go
// module so its dependency tree stays out of skrog's) against the suite's
// engine (#144). Testcontainers is the single best proxy for "does this engine
// behave like Docker Desktop": it maps a published port and polls it from the
// host, and its Ryuk reaper mounts the engine's own docker socket.
func stageTestcontainers(t *testing.T, s *state) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH; cannot run the Testcontainers module")
	}
	// The suite runs with test/e2e as its working directory.
	dir, err := filepath.Abs("testcontainers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Skipf("no Testcontainers module at %s", dir)
	}
	cmd := exec.Command(goBin, "test", "-tags", "e2e", "-count=1", "-timeout", "10m", ".")
	cmd.Dir = dir
	cmd.Env = append(s.dockerEnv(), "DOCKER_HOST="+dockerHost)
	out, err := cmd.CombinedOutput()
	must(t, string(out), err, "go test (testcontainers module)")
	t.Logf("Testcontainers passed against the engine:\n%s", strings.TrimSpace(string(out)))
}

// stageAct runs a two-job GitHub Actions workflow with `act` against the engine
// (#144): a container job with a service container, and a job that uses the
// docker CLI inside the runner image. act talks to DOCKER_HOST directly, so it
// exercises container creation, networking between containers, and volume
// mounts of the workspace — the three things a local CI runner needs.
func stageAct(t *testing.T, s *state) {
	actBin, err := exec.LookPath("act")
	if err != nil {
		t.Skip("act not on PATH; install nektos/act to exercise this")
	}
	ws := filepath.Join(s.workDir, "actws")
	wf := filepath.Join(ws, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	const workflow = `name: build
on: [push]
jobs:
  container-job:
    runs-on: ubuntu-latest
    container: alpine:3.20
    services:
      redis:
        image: redis:7-alpine
    steps:
      - run: apk add --no-cache redis
      - run: redis-cli -h redis ping
`
	if err := os.WriteFile(filepath.Join(wf, "build.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(s.dockerEnv(), "DOCKER_HOST="+dockerHost)
	out, err := runEnv(t, env, 10*time.Minute, actBin, "-W", wf, "-j", "container-job",
		"-P", "ubuntu-latest=catthehacker/ubuntu:act-22.04")
	must(t, out, err, "act -j container-job")
	if !strings.Contains(out, "PONG") {
		t.Errorf("the service container never answered; act output:\n%s", out)
	}
}

// stageGitLabCILocal runs a GitLab pipeline through gitlab-ci-local against the
// engine (#144). `gitlab-runner exec` was removed in 17.0, so this is the only
// way to prove a GitLab-shaped pipeline locally without a GitLab instance.
func stageGitLabCILocal(t *testing.T, s *state) {
	bin, err := exec.LookPath("gitlab-ci-local")
	if err != nil {
		t.Skip("gitlab-ci-local not on PATH; npm i -g gitlab-ci-local to exercise this")
	}
	// It rsyncs the working tree into the job's build dir, through bash — on
	// Windows that means rsync must be on PATH or every job fails at setup.
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("gitlab-ci-local needs rsync on PATH (MSYS2/Cygwin provides one)")
	}
	ws := filepath.Join(s.workDir, "glws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const pipeline = `stages: [test]

unit:
  stage: test
  image: alpine:3.20
  script:
    - echo gitlab-ci-local-ok
`
	if err := os.WriteFile(filepath.Join(ws, ".gitlab-ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(s.dockerEnv(), "DOCKER_HOST="+dockerHost)
	out, err := runEnv(t, env, 10*time.Minute, bin, "--cwd", ws, "unit")
	must(t, out, err, "gitlab-ci-local unit")
	if !strings.Contains(out, "gitlab-ci-local-ok") {
		t.Errorf("the job did not run its script:\n%s", out)
	}
}

// stageDagger proves Dagger works against the engine (#151). Dagger
// provisions its own long-lived engine container over DOCKER_HOST, which is a
// different shape from act and gitlab-ci-local: nothing bind-mounts the
// socket, but a container has to survive between invocations.
func stageDagger(t *testing.T, s *state) {
	bin, err := exec.LookPath("dagger")
	if err != nil {
		t.Skip("dagger not on PATH; install the Dagger CLI to exercise this")
	}
	env := append(s.dockerEnv(), "DOCKER_HOST="+dockerHost, "DAGGER_NO_NAG=1")
	out, err := runEnv(t, env, 10*time.Minute, bin, "core", "container",
		"from", "--address=alpine:3.20",
		"with-exec", "--args=echo,dagger-on-skrog-ok", "stdout")
	must(t, out, err, "dagger core container")
	if !strings.Contains(out, "dagger-on-skrog-ok") {
		t.Errorf("the Dagger pipeline produced no output:\n%s", out)
	}
	// Its engine container should be on this engine, not somewhere else.
	ps, _ := dockerE(t, s, time.Minute, "ps", "--format", "{{.Image}}")
	if !strings.Contains(ps, "dagger") {
		t.Errorf("no dagger engine container on the suite's engine; ps:\n%s", ps)
	}
	defer func() {
		ids, _ := dockerE(t, s, time.Minute, "ps", "-aq", "--filter", "name=dagger-engine")
		for _, id := range strings.Fields(ids) {
			dockerE(t, s, 2*time.Minute, "rm", "-f", id)
		}
	}()
}

// stageBuildCache proves a BuildKit cache written through this engine can be
// imported again after the builder's own cache is wiped (#151) -- the property
// that lets a laptop and a runner share layers, and the reason the rootfs pins
// BuildKit rather than taking whatever is newest.
func stageBuildCache(t *testing.T, s *state) {
	if out, err := dockerE(t, s, time.Minute, "buildx", "version"); err != nil {
		t.Skipf("no buildx plugin with this docker CLI: %s", out)
	}
	ctx := filepath.Join(s.workDir, "bakectx")
	if err := os.MkdirAll(ctx, 0o755); err != nil {
		t.Fatal(err)
	}
	// A layer slow enough that a cache hit is unmistakable, and unique to this
	// run so a stale cache cannot fake it.
	marker := fmt.Sprintf("cache-proof-%d", time.Now().UnixNano())
	dockerfile := "FROM alpine:3.20\nRUN sleep 5 && echo " + marker + " > /marker\n"
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(s.workDir, "buildcache")

	const builder = "skrog-e2e-cache"
	dockerE(t, s, time.Minute, "buildx", "rm", builder) // best effort
	if out, err := dockerE(t, s, 5*time.Minute, "buildx", "create", "--name", builder,
		"--driver", "docker-container"); err != nil {
		t.Skipf("cannot create a container builder on this engine: %s", out)
	}
	defer dockerE(t, s, 2*time.Minute, "buildx", "rm", builder)

	out, err := dockerE(t, s, 15*time.Minute, "buildx", "build", "--builder", builder,
		"--cache-to", "type=local,dest="+cache+",mode=max", ctx)
	must(t, out, err, "buildx build exporting a local cache")
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("no cache was exported to %s: %v", cache, err)
	}

	// Wipe the builder's own cache so the rebuild can only come from the
	// exported directory.
	if out, err := dockerE(t, s, 5*time.Minute, "buildx", "prune", "-af", "--builder", builder); err != nil {
		t.Fatalf("pruning the builder cache: %v\n%s", err, out)
	}
	start := time.Now()
	out, err = dockerE(t, s, 15*time.Minute, "buildx", "build", "--builder", builder,
		"--cache-from", "type=local,src="+cache, ctx)
	must(t, out, err, "buildx build importing the local cache")
	if !strings.Contains(out, "CACHED") {
		t.Errorf("rebuild did not report a cache hit after importing %s:\n%s", cache, out)
	}
	t.Logf("cache-imported rebuild took %s", time.Since(start).Round(time.Second))
}

// stageKind proves a Kubernetes cluster built out of containers works on this
// engine, and that a workload in it is reachable from Windows (#153). Skrog
// ships no Kubernetes; running kind is the whole claim, so this is the test
// that keeps it true.
//
// The cluster is configured with apiServerAddress 0.0.0.0 deliberately: WSL2
// mirrored networking only projects wildcard-bound listeners onto the Windows
// host, so kind's 127.0.0.1 default builds a cluster kubectl cannot reach.
func stageKind(t *testing.T, s *state) {
	kind, err := exec.LookPath("kind")
	if err != nil {
		t.Skip("kind not on PATH; install kind to exercise this")
	}
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not on PATH; install kubectl to exercise this")
	}

	const cluster = "skrog-e2e"
	const nodePort = "30089"
	dir := filepath.Join(s.workDir, "kind")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "kind.yaml")
	config := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "0.0.0.0"
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: ` + nodePort + `
        hostPort: ` + nodePort + `
        listenAddress: "0.0.0.0"
        protocol: TCP
`
	if err := os.WriteFile(cfg, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	env := append(s.dockerEnv(), "DOCKER_HOST="+dockerHost, "KUBECONFIG="+kubeconfig)

	// Pulling the node image is the slow part; a first run can take minutes.
	out, err := runEnv(t, env, 15*time.Minute, kind, "create", "cluster",
		"--name", cluster, "--config", cfg, "--wait", "180s")
	if err != nil {
		runEnv(t, env, 5*time.Minute, kind, "delete", "cluster", "--name", cluster)
		t.Fatalf("kind create cluster: %v\n%s", err, out)
	}
	defer runEnv(t, env, 5*time.Minute, kind, "delete", "cluster", "--name", cluster)

	// kind's kubeconfig says https://0.0.0.0:6443 and that is what works from
	// Windows; rewriting it to 127.0.0.1 fails certificate verification.
	out, err = runEnv(t, env, 2*time.Minute, kubectl, "get", "nodes", "--no-headers")
	must(t, out, err, "kubectl get nodes")
	if !strings.Contains(out, "Ready") {
		t.Fatalf("no Ready node:\n%s", out)
	}

	svc := filepath.Join(dir, "web.yaml")
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: web
          image: nginx:alpine
---
apiVersion: v1
kind: Service
metadata: {name: web}
spec:
  type: NodePort
  selector: {app: web}
  ports:
    - port: 80
      targetPort: 80
      nodePort: ` + nodePort + `
`
	if err := os.WriteFile(svc, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runEnv(t, env, 2*time.Minute, kubectl, "apply", "-f", svc)
	must(t, out, err, "kubectl apply")
	out, err = runEnv(t, env, 5*time.Minute, kubectl, "rollout", "status", "deployment/web", "--timeout=240s")
	must(t, out, err, "kubectl rollout status")

	// The point of the whole stage: a pod answering on the Windows side.
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := client.Get("http://127.0.0.1:" + nodePort + "/")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "nginx") {
				t.Logf("NodePort %s served nginx from the cluster to Windows", nodePort)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("NodePort %s never served from Windows (last error: %v)", nodePort, err)
		}
		time.Sleep(3 * time.Second)
	}
}

func stageExec(t *testing.T, s *state) {
	if out, err := dockerE(t, s, 2*time.Minute, "run", "-d", "--name", "e2e-exec",
		"alpine:latest", "sleep", "300"); err != nil {
		t.Fatalf("starting container: %v\n%s", err, out)
	}
	defer dockerE(t, s, time.Minute, "rm", "-f", "e2e-exec")

	out, err := dockerE(t, s, time.Minute, "exec", "e2e-exec", "sh", "-c", "echo exec-$(hostname)")
	must(t, out, err, "docker exec")
	if !strings.HasPrefix(out, "exec-") {
		t.Errorf("exec output = %q", out)
	}
	// A real interactive TTY (exec -it, Ctrl-C on logs -f) cannot be driven
	// from a test binary without a ConPTY harness; that stays the documented
	// manual check from #11.
}

func stageStdinPipe(t *testing.T, s *state) {
	// #57: piping into an interactive container must return, not hang. The CLI
	// signals stdin-EOF by half-closing its named-pipe connection, which only
	// works because the bridge serves the pipe in message mode; a byte-mode
	// pipe swallows the CloseWrite and the container blocks on stdin forever.
	// The bounded timeout in dockerStdin turns a regression into a failure
	// rather than a hung suite.
	const payload = "line-one\nline-two\nline-three\n"

	got, err := dockerStdin(t, s, payload, 45*time.Second, "run", "-i", "--rm", "alpine:latest", "cat")
	must(t, got, err, "docker run -i cat (stdin round-trip)")
	if got != strings.TrimRight(payload, "\n") {
		t.Errorf("run -i cat returned %q, want the piped payload", got)
	}

	// exec -i into a running container is the same half-close path.
	if out, err := dockerE(t, s, 2*time.Minute, "run", "-d", "--name", "e2e-stdin",
		"alpine:latest", "sleep", "300"); err != nil {
		t.Fatalf("starting container: %v\n%s", err, out)
	}
	defer dockerE(t, s, time.Minute, "rm", "-f", "e2e-stdin")

	got, err = dockerStdin(t, s, payload, 45*time.Second, "exec", "-i", "e2e-stdin", "wc", "-l")
	must(t, got, err, "docker exec -i wc -l (stdin round-trip)")
	if strings.TrimSpace(got) != "3" {
		t.Errorf("exec -i wc -l counted %q lines, want 3 (stdin EOF did not propagate)", got)
	}
}

// dockerStdin runs docker against the suite's engine with a string on stdin,
// bounded by timeout so a stdin-EOF regression fails instead of hanging.
func dockerStdin(t *testing.T, s *state, stdin string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(s.docker, args...)
	cmd.Env = s.dockerEnv()
	cmd.Stdin = strings.NewReader(stdin)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("docker %s timed out after %s (stdin EOF likely not propagating)",
			strings.Join(args, " "), timeout)
	}
}

func stageLogsFollow(t *testing.T, s *state) {
	if out, err := dockerE(t, s, 2*time.Minute, "run", "-d", "--name", "e2e-logs",
		"alpine:latest", "sh", "-c", "i=0; while true; do echo line-$i; i=$((i+1)); sleep 1; done"); err != nil {
		t.Fatalf("starting logger: %v\n%s", err, out)
	}
	defer dockerE(t, s, time.Minute, "rm", "-f", "e2e-logs")

	// Follow for a bounded window; receiving multiple distinct lines proves
	// incremental streaming rather than buffer-until-close.
	cmd := exec.Command(s.docker, "logs", "-f", "e2e-logs")
	cmd.Env = s.dockerEnv()
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	buf := make([]byte, 4096)
	collected := ""
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && strings.Count(collected, "line-") < 3 {
		n, err := outPipe.Read(buf)
		if n > 0 {
			collected += string(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if got := strings.Count(collected, "line-"); got < 3 {
		t.Errorf("logs -f streamed %d lines in 20s, want >= 3:\n%s", got, collected)
	}
}

func stageCompose(t *testing.T, s *state) {
	if _, err := run(t, 30*time.Second, s.docker, "compose", "version"); err != nil {
		t.Skip("docker compose plugin not available on this machine")
	}

	dir := t.TempDir()
	// Healthcheck-gated depends_on and a Windows-path bind: the two compose
	// behaviours PLAN §05 names, in the smallest stack that has both.
	compose := `
services:
  db:
    image: alpine:latest
    command: sh -c "touch /tmp/ready && sleep 300"
    healthcheck:
      test: ["CMD", "test", "-f", "/tmp/ready"]
      interval: 2s
      retries: 15
  app:
    image: alpine:latest
    depends_on:
      db:
        condition: service_healthy
    volumes:
      - ./shared:/shared
    command: sh -c "cat /shared/input.txt && echo compose-ran > /shared/output.txt"
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared", "input.txt"), []byte("compose-input"), 0o644); err != nil {
		t.Fatal(err)
	}

	composeCmd := func(timeout time.Duration, args ...string) (string, error) {
		cmd := exec.Command(s.docker, append([]string{"compose"}, args...)...)
		cmd.Dir = dir
		cmd.Env = s.dockerEnv()
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
			return strings.TrimSpace(string(out)), err
		case <-time.After(timeout):
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			<-done
			return strings.TrimSpace(string(out)), fmt.Errorf("compose timed out after %s", timeout)
		}
	}

	defer composeCmd(3*time.Minute, "down", "--timeout", "5")

	out, err := composeCmd(5*time.Minute, "up", "--abort-on-container-exit", "--exit-code-from", "app")
	must(t, out, err, "compose up")

	got, err := os.ReadFile(filepath.Join(dir, "shared", "output.txt"))
	if err != nil {
		t.Fatalf("app never wrote through the bind mount: %v\ncompose output:\n%s", err, out)
	}
	if !strings.Contains(string(got), "compose-ran") {
		t.Errorf("output.txt = %q", got)
	}
}

func stageIdle(t *testing.T, s *state) {
	// The #41 story end to end: configure a short idle timeout, watch the
	// supervisor stop the quiet engine (status says idle, exit 0 — "stopped
	// by design" is not an error), then have one docker command wake it.
	out, err := run(t, 30*time.Second, s.skrog, "config",
		"--state-dir", s.stateDir, "set", "idle-timeout", "15s")
	must(t, out, err, "skrog config set idle-timeout")
	defer func() {
		out, err := run(t, 30*time.Second, s.skrog, "config",
			"--state-dir", s.stateDir, "set", "idle-timeout", "off")
		must(t, out, err, "skrog config set idle-timeout off")
	}()

	statusJSON := func() (engine string, exit int) {
		t.Helper()
		out, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
		if err != nil {
			return "", 1
		}
		var st struct {
			Engine string `json:"engine"`
		}
		if jerr := json.Unmarshal([]byte(out), &st); jerr != nil {
			t.Fatalf("status --json unparseable: %v\n%s", jerr, out)
		}
		return st.Engine, 0
	}

	// Idle within: 15s quiet + a few 3s ticks + the stop itself. Nothing here
	// touches the engine while polling — a docker call each iteration would
	// itself reset the quiet window ("open client connections") and never let
	// the engine idle; status reads host-side files only.
	deadline := time.Now().Add(90 * time.Second)
	for {
		engine, _ := statusJSON()
		if engine == "idle" {
			break
		}
		if time.Now().After(deadline) {
			// The supervisor logs why each idle stop was deferred, including —
			// when a container vetoes — the names busyProbe saw.
			logBytes, _ := os.ReadFile(filepath.Join(s.stateDir, "proxy-e2e.log"))
			tail := string(logBytes)
			if len(tail) > 4000 {
				tail = tail[len(tail)-4000:]
			}
			ps, _ := dockerE(t, s, 30*time.Second, "ps", "-a", "--format", "{{.Names}} {{.Status}}")
			t.Fatalf("engine did not idle-stop within 90s; status reports %q\ncontainers (ps -a):\n%s\nsupervisor log tail:\n%s", engine, ps, tail)
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("engine idle-stopped; status reports it as such")

	// `skrog status` must treat idle as healthy: exit 0.
	if out, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir); err != nil {
		t.Errorf("status exited non-zero on an idle engine (stopped by design is not broken):\n%s", out)
	}

	// The whole point of idling is reclaiming the VM's RAM, and #82 found the
	// health probe itself booting the stopped distro on every tick and status
	// poll. Hammer status a few times, give a couple of supervisor ticks a
	// chance to misbehave, then require the distro to still be Stopped.
	for i := 0; i < 3; i++ {
		run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
	}
	time.Sleep(7 * time.Second) // > two supervisor ticks
	list, _ := run(t, 30*time.Second, "wsl.exe", "--list", "--verbose")
	for _, line := range strings.Split(strings.ReplaceAll(list, "\x00", ""), "\n") {
		if strings.Contains(line, distro) && strings.Contains(line, "Running") {
			t.Errorf("distro %q is running again while idle-stopped — a health probe booted it (#82): %s", distro, strings.TrimSpace(line))
		}
	}

	// One docker command is the wake-up: it pays the cold start and works.
	began := time.Now()
	out, err = dockerE(t, s, 2*time.Minute, "version", "--format", "{{.Server.Version}}")
	must(t, out, err, "docker version against an idle engine")
	t.Logf("cold start: docker version answered %q in %s", out, time.Since(began).Round(100*time.Millisecond))

	if engine, _ := statusJSON(); engine != "running" {
		t.Errorf("engine is %q after the on-demand wake, want running", engine)
	}
}

func stageWSLIntegrate(t *testing.T, s *state) {
	// #42 end to end, against a real second distro: the engine socket is
	// shared VM-wide at /mnt/wsl/<distro>/docker.sock, wsl-integrate wires a
	// target distro's DOCKER_HOST to it, --remove unwires cleanly.
	list, _ := run(t, 30*time.Second, "wsl.exe", "--list", "--quiet")
	if !strings.Contains(strings.ReplaceAll(list, "\x00", ""), "Ubuntu") {
		t.Skip("no Ubuntu distro to integrate against")
	}
	// Never clobber a real integration: if the user's Ubuntu already carries
	// the profile script, this stage's --remove would delete theirs.
	if _, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu",
		"--", "test", "-f", "/etc/profile.d/skrog.sh"); err == nil {
		t.Skip("Ubuntu already has a skrog integration; not touching it")
	}

	// The engine's socket must be visible from the OTHER distro — that is
	// the whole point of the /mnt/wsl bind.
	sock := "/mnt/wsl/" + distro + "/docker.sock"
	if out, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu",
		"--", "test", "-S", sock); err != nil {
		t.Fatalf("shared socket %s not visible from Ubuntu: %v\n%s", sock, err, out)
	}
	// And it must actually answer, as the ORDINARY user (not root): the point
	// of the share is docker-without-sudo, which needs the 0666 the
	// provisioner sets. A dead inode also passes test -S, so this is the real
	// check. Skip only when curl is genuinely absent (exit 127), not when it
	// runs and fails.
	if _, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu", "--", "sh", "-c", "command -v curl"); err != nil {
		t.Log("curl absent in Ubuntu; socket presence checked but not exercised")
	} else {
		out, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu", "--",
			"curl", "-s", "--max-time", "10", "--unix-socket", sock, "http://localhost/_ping")
		if err != nil || out != "OK" {
			t.Errorf("engine ping from Ubuntu as the ordinary user = %q (%v); want OK — the shared socket must be user-accessible, not root-only", out, err)
		}
	}

	out, err := run(t, time.Minute, s.skrog, "wsl-integrate", "--state-dir", s.stateDir, "Ubuntu")
	must(t, out, err, "skrog wsl-integrate Ubuntu")
	defer run(t, time.Minute, s.skrog, "wsl-integrate", "--state-dir", s.stateDir, "--remove", "Ubuntu")

	if out, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu",
		"--", "cat", "/etc/profile.d/skrog.sh"); err != nil || !strings.Contains(out, sock) {
		t.Errorf("profile script wrong or missing (%v):\n%s", err, out)
	}

	out, err = run(t, time.Minute, s.skrog, "wsl-integrate", "--state-dir", s.stateDir, "--remove", "Ubuntu")
	must(t, out, err, "skrog wsl-integrate --remove")
	if _, err := run(t, 30*time.Second, "wsl.exe", "-d", "Ubuntu",
		"--", "test", "-f", "/etc/profile.d/skrog.sh"); err == nil {
		t.Error("profile script still present after --remove")
	}
}

func stageMigrate(t *testing.T, s *state) {
	// #43 end to end against real Docker Desktop: seed a distinctive image and
	// a volume with known contents in Desktop, migrate ONLY those into the
	// Skrog engine (--only keeps this from copying the developer's whole
	// Desktop), and prove they arrived intact while Desktop is untouched.
	if !s.ddWorkedBefore {
		t.Skip("Docker Desktop was not serving before the suite; nothing to migrate from")
	}
	const (
		probeImg = "alpine:3.19"
		probeVol = "skrog-e2e-migrate-probe"
		marker   = "skrog-e2e-migrate-marker"
	)
	// Seed Desktop. Cleaned up regardless of outcome.
	if out, err := s.runDocker(t, 3*time.Minute, "--context", "desktop-linux", "pull", probeImg); err != nil {
		t.Skipf("cannot pull %s into Desktop (%v): %s", probeImg, err, out)
	}
	s.runDocker(t, 30*time.Second, "--context", "desktop-linux", "volume", "rm", "-f", probeVol)
	vout, verr := s.runDocker(t, 30*time.Second, "--context", "desktop-linux", "volume", "create", probeVol)
	must(t, vout, verr, "create source volume")
	defer s.runDocker(t, 30*time.Second, "--context", "desktop-linux", "volume", "rm", "-f", probeVol)
	sout, serr := s.runDocker(t, time.Minute, "--context", "desktop-linux", "run", "--rm",
		"-v", probeVol+":/d", probeImg, "sh", "-c", "echo "+marker+" > /d/marker.txt")
	must(t, sout, serr, "seed source volume")

	// Migrate just the probe items into the suite's engine. hostEnv puts
	// docker's dir on PATH so the credential helper resolves for the pull of
	// the tar-helper image — the environment a real migrate already has.
	out, err := runEnv(t, s.hostEnv(), 5*time.Minute, s.skrog, "migrate", "--from-desktop",
		"--only", probeImg, "--only", probeVol, "--state-dir", s.stateDir,
		"--docker", s.docker, "--docker-host", dockerHost)
	must(t, out, err, "skrog migrate")

	// Image arrived.
	if got, err := dockerE(t, s, time.Minute, "images", probeImg, "--format", "{{.Repository}}:{{.Tag}}"); err != nil || got == "" {
		t.Errorf("migrated image not on the Skrog engine: %q (%v)", got, err)
	}
	// Volume arrived with its contents intact — the real proof, not just presence.
	got, err := dockerE(t, s, time.Minute, "run", "--rm", "-v", probeVol+":/d", probeImg, "cat", "/d/marker.txt")
	if err != nil || got != marker {
		t.Errorf("migrated volume content = %q (%v), want %q", got, err, marker)
	}

	// Desktop is untouched: the source volume and its data still there.
	if src, err := s.runDocker(t, time.Minute, "--context", "desktop-linux", "run", "--rm",
		"-v", probeVol+":/d", probeImg, "cat", "/d/marker.txt"); err != nil || src != marker {
		t.Errorf("source volume changed by migration: %q (%v); it must be read-only", src, err)
	}

	// Re-running is a no-op (everything already present), and idempotent.
	out, err = run(t, 2*time.Minute, s.skrog, "migrate", "--from-desktop",
		"--only", probeImg, "--only", probeVol, "--state-dir", s.stateDir,
		"--docker", s.docker, "--docker-host", dockerHost)
	must(t, out, err, "skrog migrate (second run)")
	if !strings.Contains(out, "Nothing to migrate") {
		t.Errorf("second migrate should be a no-op, got:\n%s", out)
	}
}

func stageInterrupt(t *testing.T, s *state) {
	// The #35 shape: kill a client mid-request, then prove the bridge still
	// answers AND that nothing leaked. Under the v0.1 socat relay this stage
	// could only assert responsiveness (the leak was bounded by an idle
	// timeout, not fixed); with the vsock agent (#40) both ends of the
	// transport are owned, a killed client's connection tears down
	// explicitly, and the wsl.exe count must return to its pre-interrupt
	// value — the regression #35 asked for.
	before := wslProcCount(t)

	long := exec.Command(s.docker, "run", "--rm", "alpine:latest", "sleep", "30")
	long.Env = s.dockerEnv()
	if err := long.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	long.Process.Kill()
	long.Wait()

	out, err := dockerE(t, s, time.Minute, "version", "--format", "{{.Server.Version}}")
	must(t, out, err, "engine after an interrupted client")
	if out == "" {
		t.Error("engine did not answer after a client was killed mid-run")
	}

	// Teardown is not instantaneous; give it a moment, but far less than the
	// 5-minute socat idle timeout that used to be the only backstop.
	deadline := time.Now().Add(30 * time.Second)
	after := wslProcCount(t)
	for after > before && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		after = wslProcCount(t)
	}
	if after > before {
		t.Errorf("wsl.exe count %d did not return to the pre-interrupt %d; the interrupted client leaked its relay", after, before)
	}
}

func stageVsockServed(t *testing.T, s *state) {
	// The suite installed from the published release, so this asserts the
	// shipping artifact chain end to end: the rootfs carries skrog-agent,
	// the proxy's vsock dialer reached it, and no connection needed the socat
	// fallback. The fallback logs one edge-triggered warning the moment it is
	// first used; its absence over a suite's worth of traffic means the fast
	// path served everything.
	logBytes, err := os.ReadFile(filepath.Join(s.stateDir, "proxy-e2e.log"))
	if err != nil {
		t.Fatalf("reading proxy log: %v", err)
	}
	if strings.Contains(string(logBytes), "fallback transport") {
		t.Error("proxy degraded to the socat fallback; the published rootfs should carry a reachable skrog-agent")
	}
}

func stageUninstall(t *testing.T, s *state) {
	// The proxy must not outlive the engine it serves.
	if s.proxy != nil && s.proxy.Process != nil {
		s.proxy.Process.Kill()
		s.proxy.Wait()
	}
	if s.proxyLog != nil {
		s.proxyLog.Close()
	}

	out, err := run(t, 5*time.Minute, s.skrog, "uninstall", "--state-dir", s.stateDir, "--yes")
	must(t, out, err, "skrog uninstall")
}

func stageClean(t *testing.T, s *state) {
	out, _ := run(t, 30*time.Second, "wsl.exe", "--list", "--quiet")
	if strings.Contains(strings.ReplaceAll(out, "\x00", ""), distro) {
		t.Errorf("distro %q still registered after uninstall", distro)
	}
	if _, err := os.Stat(s.dataDir); !os.IsNotExist(err) {
		t.Errorf("data dir still present: %s", s.dataDir)
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "manifest.json")); !os.IsNotExist(err) {
		t.Error("install manifest still present after uninstall")
	}

	// The machine's own docker context must be exactly where the suite found
	// it. This is not tidiness: the `skrog` context is one global object
	// shared with any real install, so a suite that took it and did not hand
	// it back left the developer with a broken `docker --context skrog` and
	// no clue why (#217). An empty baseline is just as much of an assertion —
	// a suite that installs and uninstalls must leave no context behind
	// either.
	if after := skrogContextEndpoint(t, s); after != s.skrogCtxBefore {
		t.Errorf("the machine's `skrog` docker context changed: before=%q after=%q\n"+
			"the suite must hand back a context it took from another install",
			s.skrogCtxBefore, after)
	}

	// The bounded #35 leak means "returns to baseline" cannot be asserted yet;
	// what can be is that the suite did not permanently double the population.
	after := wslProcCount(t)
	t.Logf("wsl.exe count: before=%d after=%d", s.wslProcsBefore, after)
	if after > s.wslProcsBefore+3 {
		t.Errorf("wsl.exe count grew from %d to %d; relay processes are leaking beyond the bound",
			s.wslProcsBefore, after)
	}

	// The supervisor must not outlive the install it served (#474).
	//
	// Nothing else here can catch that, and until SupervisorRestart ran the
	// suite was not even exposed to it: the supervisor this asks about is not
	// the one stageProxy started and stageUninstall killed by handle.
	// `restart --supervisor` replaced that one, and the replacement was
	// spawned detached and released by `skrog start` — an orphan with no
	// parent to kill it. That is not an artefact of the suite. It is what a
	// supervisor looks like on every real install, where autostart or
	// skrogw.exe launched it exactly the same way.
	//
	// `skrog status` cannot answer this: it reports a supervisor only for an
	// install it can still find, so after uninstall it says "stopped" however
	// many are running. supervisor.lock is the product's own record, and the
	// file status itself reads — an exclusively-opened DELETE_ON_CLOSE handle
	// (internal/supervise), so it exists while a supervisor holds it and the
	// OS removes it when that process dies.
	lock := filepath.Join(s.stateDir, "supervisor.lock")
	if _, err := os.Stat(lock); err == nil {
		t.Errorf("a supervisor is still running after uninstall: %s is still held.\n"+
			"It serves a pipe into a distro that no longer exists, can re-point the "+
			"`skrog` docker context the uninstall just unwired, and holds skrog.exe open "+
			"so the directory it lives in cannot be deleted", lock)
	}
}

func stageDesktopIntact(t *testing.T, s *state) {
	if !s.ddWorkedBefore {
		t.Skip("Docker Desktop was not serving before the suite; nothing to compare against")
	}
	out, err := s.runDocker(t, 2*time.Minute, "--context", "desktop-linux", "run", "--rm", "hello-world")
	if err != nil {
		t.Fatalf("Docker Desktop broken after the suite: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Hello from Docker!") {
		t.Errorf("Docker Desktop output unexpected:\n%s", out)
	}
}

// forceCleanup removes whatever a failed run left, using the binary itself so
// the cleanup path is also the product's own.
func forceCleanup(s *state) {
	if s.proxy != nil && s.proxy.Process != nil {
		s.proxy.Process.Kill()
		s.proxy.Wait()
	}
	if s.proxyLog != nil {
		s.proxyLog.Close()
	}
	if s.skrog != "" && s.stateDir != "" {
		exec.Command(s.skrog, "uninstall", "--state-dir", s.stateDir, "--yes").Run()
	}
	exec.Command("wsl.exe", "--unregister", distro).Run()
}

// findDocker resolves the docker CLI: PATH first, then where Docker Desktop
// installs it. Test environments (notably Git Bash) often lack the PATH entry
// while the binary is right there.
func findDocker() string {
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	for _, c := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Docker", "Docker", "resources", "bin", "docker.exe"),
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// dockerEnv builds the child environment for docker invocations: the suite's
// engine selected, and the CLI's own directory on PATH. The latter matters
// when docker was found outside PATH — its credential helper
// (docker-credential-wincred) lives next to it and is resolved via PATH, and
// without it every pull fails with "error getting credentials". The same fact
// is why #9 bundles the helper alongside the CLI.
func (s *state) dockerEnv() []string {
	env := append(os.Environ(),
		"DOCKER_HOST="+dockerHost,
		"DOCKER_CONTEXT=",
	)
	dir := filepath.Dir(s.docker)
	for i, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			env[i] = kv + string(os.PathListSeparator) + dir
			return env
		}
	}
	return append(env, "PATH="+dir)
}

// hostEnv is the environment for docker commands aimed at the machine's own
// engines (Docker Desktop), not the suite's: PATH gains the CLI's directory so
// its credential helper resolves, and no DOCKER_HOST override is applied.
func (s *state) hostEnv() []string {
	env := os.Environ()
	dir := filepath.Dir(s.docker)
	for i, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			env[i] = kv + string(os.PathListSeparator) + dir
			return env
		}
	}
	return append(env, "PATH="+dir)
}

// runDocker runs the docker CLI with hostEnv, for Desktop-facing checks.
func (s *state) runDocker(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(s.docker, args...)
	cmd.Env = s.hostEnv()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return strings.TrimSpace(string(out)), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return strings.TrimSpace(string(out)), fmt.Errorf("docker timed out after %s", timeout)
	}
}

// stageAuditToggle is the regression test for #202: the audit setting has to
// take effect against a supervisor that is ALREADY RUNNING.
//
// The suite could not have caught the original bug, and did not: it turned
// audit on before the first install, so the supervisor read "on" at startup
// and everything downstream worked. The whole defect lived in the transition,
// which nothing exercised — the unit tests passed, and a real
// `docker run --privileged` on a machine that had just been told to deny it
// succeeded. So this stage changes the setting mid-life, both ways, and
// checks the log follows.
func stageAuditToggle(t *testing.T, s *state) {
	logPath := filepath.Join(s.stateDir, "audit.log")
	size := func() int64 {
		t.Helper()
		fi, err := os.Stat(logPath)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatalf("stat audit log: %v", err)
		}
		return fi.Size()
	}
	setAudit := func(v string) {
		t.Helper()
		out, err := run(t, 30*time.Second, s.skrog, "config",
			"--state-dir", s.stateDir, "set", "audit", v)
		must(t, out, err, "skrog config set audit "+v)
	}
	// One container-affecting call, which the log must either gain or ignore.
	touchEngine := func() {
		t.Helper()
		out, err := dockerE(t, s, 2*time.Minute, "run", "--rm", "hello-world")
		must(t, out, err, "docker run hello-world")
	}

	// --- off, mid-life: recording must stop, with no restart ---
	setAudit("off")
	// The switch closes the log on its next observed call, so let one pass
	// before measuring; otherwise this races the transition rather than
	// testing it.
	touchEngine()
	before := size()
	touchEngine()
	if got := size(); got != before {
		t.Errorf("audit log grew from %d to %d after `config set audit off` — "+
			"turning it off did not take effect", before, got)
	}

	// --- on again, mid-life: the original bug ---
	setAudit("on")
	deadline := time.Now().Add(60 * time.Second)
	for {
		touchEngine()
		if size() > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit log did not grow after `config set audit on` against a "+
				"running supervisor — the setting is not live (#202). log=%s size=%d",
				logPath, size())
		}
		time.Sleep(time.Second)
	}

	// And the new records are real events, not a truncated or corrupted file.
	out, err := run(t, 30*time.Second, s.skrog, "audit", "--state-dir", s.stateDir, "tail", "-n", "5")
	must(t, out, err, "audit tail after re-enabling")
	if !strings.Contains(out, "container-create") {
		t.Errorf("no container-create after re-enabling the audit log:\n%s", out)
	}
}

// stageSupervisorRestart covers `skrog restart --supervisor` (#202): before
// it, the only way to recycle the supervisor was to kill the process by hand,
// which is not a documented action and not something a user should have to
// discover.
func stageSupervisorRestart(t *testing.T, s *state) {
	supervisorRunning := func() bool {
		t.Helper()
		out, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
		if err != nil {
			return false
		}
		var st struct {
			Supervisor string `json:"supervisor"`
			Engine     string `json:"engine"`
		}
		if jerr := json.Unmarshal([]byte(out), &st); jerr != nil {
			t.Fatalf("status --json unparseable: %v\n%s", jerr, out)
		}
		return st.Supervisor == "running"
	}

	if !supervisorRunning() {
		t.Fatal("no supervisor running before the restart test")
	}

	// Delete the endpoint record first, so that finding one afterwards proves
	// the *replacement* supervisor wrote its own (#273) rather than the old
	// one's happening to survive. The record has to follow the live process:
	// that is the whole reason status reports what was bound instead of asking
	// the selector again.
	endpointFile := filepath.Join(s.stateDir, "endpoint.json")
	if err := os.Remove(endpointFile); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing the endpoint record before the restart: %v", err)
	}

	out, err := run(t, 5*time.Minute, s.skrog, "restart", "--state-dir", s.stateDir, "--supervisor")
	must(t, out, err, "skrog restart --supervisor")

	if !supervisorRunning() {
		t.Fatal("no supervisor running after `skrog restart --supervisor`")
	}

	status, err := run(t, 30*time.Second, s.skrog, "status", "--state-dir", s.stateDir, "--json")
	must(t, status, err, "skrog status --json after the restart")
	var after struct {
		Endpoint *struct {
			Pipe string `json:"pipe"`
		} `json:"endpoint"`
	}
	if jerr := json.Unmarshal([]byte(status), &after); jerr != nil {
		t.Fatalf("status --json unparseable: %v\n%s", jerr, status)
	}
	switch {
	case after.Endpoint == nil:
		t.Errorf("the replacement supervisor recorded no endpoint:\n%s", status)
	case after.Endpoint.Pipe != pipeName:
		t.Errorf("endpoint.pipe = %q after the restart, want %q", after.Endpoint.Pipe, pipeName)
	}
	// The point of recycling it is that the bridge comes back with it: a
	// supervisor that exited and left no replacement would fail every docker
	// command from here on, which is the failure this must never introduce.
	out, err = dockerE(t, s, 2*time.Minute, "run", "--rm", "hello-world")
	must(t, out, err, "docker run after a supervisor restart")

	// The request file must not survive: a leftover note would shut down the
	// next supervisor on sight.
	if _, err := os.Stat(filepath.Join(s.stateDir, "restart-request")); !os.IsNotExist(err) {
		t.Errorf("restart-request still present after the restart completed (err=%v)", err)
	}
}

// stageUpgradeCheck covers `skrog upgrade` (#191) against a fresh install:
// the engine it just installed is by definition the newest this build can
// reach, so the check must say so and the plan must be empty.
//
// The apply path is exercised by hand against a scratch install at an older
// engine rather than here, because making this stage install an old engine
// first would add minutes to every run for one assertion.
func stageUpgradeCheck(t *testing.T, s *state) {
	out, err := run(t, 2*time.Minute, s.skrog, "upgrade", "--state-dir", s.stateDir, "--json")
	must(t, out, err, "skrog upgrade --json")

	var rep struct {
		Streams []struct {
			Name    string `json:"name"`
			Current string `json:"current"`
			Status  string `json:"status"`
		} `json:"streams"`
		Offline bool `json:"offline"`
	}
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("upgrade --json unparseable: %v\n%s", jerr, out)
	}

	byName := map[string]string{}
	current := map[string]string{}
	for _, st := range rep.Streams {
		byName[st.Name] = st.Status
		current[st.Name] = st.Current
	}
	for _, want := range []string{"app", "engine", "cli"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no %q stream in the report: %s", want, out)
		}
	}
	if byName["engine"] != "current" {
		t.Errorf("engine status = %q right after install, want current (installed %q)",
			byName["engine"], current["engine"])
	}

	// --offline must still answer for the engine, because the manifest is
	// compiled in — and must not claim the app is current when it did not look.
	out, err = run(t, 30*time.Second, s.skrog, "upgrade", "--state-dir", s.stateDir, "--offline", "--json")
	must(t, out, err, "skrog upgrade --offline --json")
	if !strings.Contains(out, `"offline": true`) {
		t.Errorf("--offline did not report itself as offline:\n%s", out)
	}
	var off struct {
		Streams []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"streams"`
	}
	if jerr := json.Unmarshal([]byte(out), &off); jerr != nil {
		t.Fatalf("offline report unparseable: %v", jerr)
	}
	for _, st := range off.Streams {
		if st.Name == "app" && st.Status == "current" {
			t.Error("the app reads 'current' offline; a check that did not happen must not read as one that passed")
		}
		if st.Name == "engine" && st.Status != "current" {
			t.Errorf("engine status = %q offline, want current — the manifest is compiled in", st.Status)
		}
	}

	// A dry run on an up-to-date install has nothing to say and must exit 0.
	out, err = run(t, 30*time.Second, s.skrog, "upgrade", "--state-dir", s.stateDir, "--dry-run")
	must(t, out, err, "skrog upgrade --dry-run")
	if strings.Contains(out, "would run:") {
		t.Errorf("a dry run proposed work on a current install:\n%s", out)
	}
}

// skrogContextEndpoint reports where the machine's shared `skrog` docker
// context points, or "" when there is no such context.
//
// Deliberately tolerant: a missing context, a docker CLI that cannot run, and
// an empty answer are all "", because the assertion this feeds is "unchanged",
// and every one of those states compares correctly against itself.
func skrogContextEndpoint(t *testing.T, s *state) string {
	t.Helper()
	out, err := s.runDocker(t, 30*time.Second, "context", "inspect", "skrog",
		"--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
