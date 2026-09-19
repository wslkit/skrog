package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wslkit/skrog/internal/engineconfig"
	"github.com/wslkit/skrog/internal/gpu"
	"github.com/wslkit/skrog/internal/rootfsverify"
	"github.com/wslkit/skrog/internal/winpath"
	"github.com/wslkit/skrog/internal/wsl"
)

// DefaultDistro is the WSL distribution Skrog imports. Deliberately distinct
// so it never collides with a user's own Ubuntu (PLAN §04).
const DefaultDistro = "skrog-engine"

// Engine backends an install can use (#335).
//
// BackendDistro is a WSL2 distro Skrog imports, owns and can pin. BackendWslc
// is a WSL container session, where Microsoft ships the engine -- so there is
// no rootfs, no version to pin, and several commands have nothing to act on.
const (
	BackendDistro = "distro"
	BackendWslc   = "wslc"
)

// distroNameRE bounds what a distro name may contain (#93). Names flow into
// shells (the /mnt/wsl share/unshare scripts pass them as positional args, but
// the unshare's rm -rf operates on a path derived from the name) and into a
// profile-script line by wsl-integrate. Restricting to this charset — and
// forbidding the path-traversal spellings — closes the whole class rather than
// auditing each call site; every legitimate name (the default, a user's
// --distro) already fits.
var distroNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validateDistroName(name string) error {
	if !distroNameRE.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid distro name %q: use letters, digits, dot, dash, underscore", name)
	}
	return nil
}

// EngineSocket is where dockerd listens inside the distro.
const EngineSocket = "/var/run/docker.sock"

// Options configures an install. Zero values get sensible defaults, so callers
// only set what they mean to change.
type Options struct {
	// Distro is the WSL distribution name. Defaults to DefaultDistro.
	Distro string
	// StateDir holds the manifest and the rootfs cache.
	// Defaults to %LOCALAPPDATA%\Skrog.
	StateDir string
	// DataDir is where the distro's VHDX lives. Defaults to StateDir\distro.
	// Exposed because "move it off C:" is a perennial request (PLAN §03).
	DataDir string
	// RootfsURL and RootfsSHA256 identify the rootfs to install. The checksum
	// is mandatory: an unverified rootfs becomes root inside the engine VM.
	RootfsURL    string
	RootfsSHA256 string
	// EngineVersion is recorded in the manifest for `skrog version`.
	EngineVersion string
	// Headless suppresses anything that would wait for a human.
	Headless bool
	// StartTimeout bounds the wait for dockerd's socket. Defaults to 60s.
	StartTimeout time.Duration
	// Network configures the engine for corporate networks (#62): a proxy for
	// dockerd's pulls and extra CA certificates to trust. Applied on every
	// engine start, so a rootfs re-import keeps it.
	Network NetConfig
	// GPUEnabled writes the NVIDIA CDI spec into the distro so containers can use
	// the GPU (#83). Like Network, applied on every engine start so a rootfs
	// re-import keeps it.
	GPUEnabled bool
	// GPUVendor picks which vendor's CDI spec applyGPU writes (#185).
	// Empty means gpu.DefaultVendor, so an install that predates this stays
	// NVIDIA.
	GPUVendor string
	// VerifySignature checks the rootfs Sigstore signature before importing, in
	// addition to the always-enforced checksum (#147). Opt-in, because it needs
	// cosign on PATH and an air-gapped install has no transparency log to reach.
	VerifySignature bool
}

// NetConfig is the corporate-network configuration applied to the engine.
type NetConfig struct {
	// Proxy is the HTTP(S) proxy URL for dockerd (empty = none). NoProxy is the
	// comma-separated bypass list.
	Proxy   string
	NoProxy string
	// HostCAPEM is a PEM bundle of extra root CAs to trust — the fix for a
	// TLS-inspecting corporate proxy whose root the engine does not know. Empty
	// removes any Skrog-installed host CAs.
	HostCAPEM []byte
}

func (o Options) withDefaults() Options {
	if o.Distro == "" {
		o.Distro = DefaultDistro
	}
	if o.StateDir == "" {
		o.StateDir = defaultStateDir()
	}
	if o.DataDir == "" {
		o.DataDir = filepath.Join(o.StateDir, "distro")
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = 60 * time.Second
	}
	return o
}

func defaultStateDir() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "Skrog")
	}
	// Non-Windows only happens in tests; keep it deterministic rather than
	// panicking so the package stays testable everywhere.
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "skrog")
	}
	return filepath.Join(home, ".skrog")
}

// Manifest records what an install put on the machine, so uninstall can remove
// exactly that and `skrog version` can report it without re-deriving anything.
type Manifest struct {
	// Backend is which engine this install uses (#335). Empty or
	// BackendDistro means Skrog's own WSL2 distro; BackendWslc means a WSL
	// container session, in which case Distro, RootfsURL and the engine
	// version fields are all empty — Microsoft ships that engine.
	//
	// Empty rather than "distro" on existing installs, and omitempty on the
	// way out, so the manifest of a distro install is byte-identical to what
	// it was. Read it through Manifest.BackendName.
	Backend       string    `json:"backend,omitempty"`
	Distro        string    `json:"distro"`
	DataDir       string    `json:"dataDir"`
	RootfsURL     string    `json:"rootfsUrl"`
	RootfsSHA256  string    `json:"rootfsSha256"`
	EngineVersion string    `json:"engineVersion"`
	InstalledAt   time.Time `json:"installedAt"`
	// WSLVersion is what WSL reported at install time, useful when diagnosing
	// a machine whose WSL was updated afterwards.
	WSLVersion string `json:"wslVersion,omitempty"`
	// EngineRef is the revisioned engine label ("29.7.2-4"), which is the unit
	// an upgrade moves between -- EngineVersion is only the dockerd version, and
	// two rootfs revisions can carry the same one (#65). Empty on installs that
	// predate this field.
	EngineRef string `json:"engineRef,omitempty"`
	// PreviousEngineRef is where `skrog engine rollback` goes back to: the ref
	// that was installed before the last upgrade.
	PreviousEngineRef string `json:"previousEngineRef,omitempty"`
	// UpgradedAt is when the last engine upgrade landed.
	UpgradedAt time.Time `json:"upgradedAt,omitempty"`
	// DockerContextHost is the endpoint this install wired the shared `skrog`
	// docker context to (#217).
	//
	// The context is a single global object, so two installs on one machine
	// contend for it. Recording what we pointed it at is how `skrog
	// uninstall` can tell "this context is mine to remove" from "another
	// install owns it now" -- without which removing the second install broke
	// the first. Empty on installs that predate this field, and on machines
	// with no docker CLI to wire.
	DockerContextHost string `json:"dockerContextHost,omitempty"`
	// DockerContextPrevious is where the context pointed BEFORE this install
	// took it over. Uninstall hands it back rather than deleting a context
	// another install is still using. Empty means this install created the
	// context, and removing it is then the correct cleanup.
	DockerContextPrevious string `json:"dockerContextPrevious,omitempty"`
}

// Provisioner performs installs and removals.
type Provisioner struct {
	// WSL drives wsl.exe. Defaults to the real implementation.
	WSL wsl.WSL
	// Fetcher retrieves the rootfs. Defaults to HTTP.
	Fetcher Fetcher
	// Logger receives progress. Defaults to slog.Default().
	Logger *slog.Logger

	// defaultWSL is the backend built on first use and reused thereafter.
	//
	// Memoised rather than constructed per call, which is what this used to do.
	// The default backend now holds a COM apartment on a thread of its own
	// (#356), and building one per call would start a thread per call — the
	// supervisor asks on every health tick.
	wslOnce    sync.Once
	defaultWSL wsl.WSL
}

func (p *Provisioner) wsl() wsl.WSL {
	if p.WSL != nil {
		return p.WSL
	}
	p.wslOnce.Do(func() { p.defaultWSL = wsl.NewFast() })
	return p.defaultWSL
}

func (p *Provisioner) fetcher() Fetcher {
	if p.Fetcher != nil {
		return p.Fetcher
	}
	return HTTPFetcher{}
}

func (p *Provisioner) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// PreflightError reports that installation cannot proceed, carrying every
// problem so the user sees the full picture rather than one issue per run.
type PreflightError struct{ Report Report }

func (e *PreflightError) Error() string {
	msg := "preflight failed:"
	for _, p := range e.Report.Problems {
		msg += "\n- " + p.String()
	}
	return msg
}

// Install provisions the engine distro: preflight, verified download, import,
// then start.
//
// It is deliberately not idempotent over an existing distro — preflight refuses
// when the target is already registered, because that distro holds the user's
// images and volumes and silently reimporting would destroy them.
func (p *Provisioner) Install(ctx context.Context, opts Options) (*Manifest, error) {
	opts = opts.withDefaults()
	if err := validateDistroName(opts.Distro); err != nil {
		return nil, err
	}
	if opts.RootfsURL == "" {
		return nil, fmt.Errorf("install: RootfsURL is required")
	}

	report, err := p.Preflight(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, w := range report.Warnings {
		p.logger().Warn(w.Summary, "fix", w.Remedy)
	}
	if !report.OK {
		return nil, &PreflightError{Report: report}
	}

	rootfs := filepath.Join(opts.StateDir, "rootfs", filepath.Base(opts.RootfsURL))
	if err := p.fetchRootfs(ctx, opts.RootfsURL, opts.RootfsSHA256, rootfs); err != nil {
		return nil, err
	}

	// The checksum above is always enforced; a signature additionally ties the
	// bytes to the project's release workflow, which a forked manifest cannot
	// forge (#147). Opt-in, and a failure refuses the install rather than
	// importing something unverified.
	if opts.VerifySignature {
		if err := p.verifyRootfsSignature(ctx, opts, rootfs); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir %s: %w", opts.DataDir, err)
	}

	p.logger().Info("importing distro", "distro", opts.Distro, "dataDir", opts.DataDir)
	if err := p.wsl().Import(ctx, opts.Distro, opts.DataDir, rootfs); err != nil {
		return nil, fmt.Errorf("importing %s: %w", opts.Distro, err)
	}

	engineVersion := opts.EngineVersion
	if engineVersion == "" {
		// The rootfs records what it actually contains, which is more
		// trustworthy than a flag and is the only source available when the
		// rootfs came from --rootfs-url rather than the release manifest.
		engineVersion = p.engineVersionFromDistro(ctx, opts)
	}

	m := &Manifest{
		Distro:        opts.Distro,
		DataDir:       opts.DataDir,
		RootfsURL:     opts.RootfsURL,
		RootfsSHA256:  opts.RootfsSHA256,
		EngineVersion: engineVersion,
		InstalledAt:   time.Now().UTC(),
		WSLVersion:    report.Status.Version,
	}
	// Written before the engine starts: if start fails, uninstall still knows
	// what to clean up rather than leaving an orphaned distro behind.
	if err := p.writeManifest(opts, m); err != nil {
		return nil, err
	}

	if err := p.StartEngine(ctx, opts); err != nil {
		return m, fmt.Errorf("starting engine: %w", err)
	}
	return m, nil
}

// socketBusyScript counts ESTABLISHED connections to the engine socket inside
// the distro (ss ships in the rootfs via iproute2). The listening socket is
// state LISTEN and excluded.
const socketBusyScript = "ss -H -x state established src " + EngineSocket + " 2>/dev/null | wc -l"

// SocketBusy reports whether anything holds an active connection to the engine
// socket right now (#72). Called only when the pipe has no clients, so a
// positive result means a user the pipe cannot see — a docker client in an
// integrated distro over the /mnt/wsl share — is mid-operation, and idling the
// engine would kill its work. An error is returned so the caller can veto
// rather than stop on an unknown.
func (p *Provisioner) SocketBusy(ctx context.Context, opts Options) (bool, error) {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", socketBusyScript)
	if err != nil {
		return false, err
	}
	n := 0
	if _, e := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); e != nil {
		return false, fmt.Errorf("parsing socket connection count %q: %w", out, e)
	}
	return n > 0, nil
}

// EngineVersionFile is where the rootfs records the engine it carries.
const EngineVersionFile = "/etc/skrog/engine-version"

// engineVersionFromDistro reads the version the rootfs declares. Best-effort:
// a rootfs without the marker is unusual but not a reason to fail an install,
// and `skrog version` reports an unknown engine rather than a wrong one.
func (p *Provisioner) engineVersionFromDistro(ctx context.Context, opts Options) string {
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "cat", EngineVersionFile)
	if err != nil {
		p.logger().Debug("rootfs declares no engine version",
			"file", EngineVersionFile, "error", err)
		return ""
	}
	return strings.TrimSpace(out)
}

// Host-CA install paths. The bundle is staged whole, then split into one file
// per certificate because Alpine's update-ca-certificates skips any .crt that is
// not exactly one certificate. The glob is Skrog's own, so turning the feature
// off removes exactly what it added.
const (
	hostCABundle = "/etc/skrog/host-cas-bundle.pem"
	hostCADir    = "/usr/local/share/ca-certificates"
	hostCAGlob   = hostCADir + "/skrog-host-*.crt"
)

// proxyEnv builds the shell env file dockerd sources: HTTP(S)_PROXY and NO_PROXY
// in both cases. Empty proxy yields an empty file (clears any prior setting).
func proxyEnv(proxy, noProxy string) string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return ""
	}
	var b strings.Builder
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		fmt.Fprintf(&b, "export %s=%q\n", k, proxy)
	}
	np := "localhost,127.0.0.1"
	if extra := strings.TrimSpace(noProxy); extra != "" {
		np += "," + extra
	}
	fmt.Fprintf(&b, "export NO_PROXY=%q\nexport no_proxy=%q\n", np, np)
	return b.String()
}

// applyNetwork writes the proxy env file and installs (or removes) the host CA
// bundle. Best-effort: a network-config failure logs but does not stop the
// engine, which must still come up.
func (p *Provisioner) applyNetwork(ctx context.Context, opts Options) {
	if err := p.writeDistroFile(ctx, opts, "/etc/skrog/network.env",
		[]byte(proxyEnv(opts.Network.Proxy, opts.Network.NoProxy))); err != nil {
		p.logger().Warn("could not write engine proxy config", "error", err)
	}

	if len(opts.Network.HostCAPEM) > 0 {
		if err := p.writeDistroFile(ctx, opts, hostCABundle, opts.Network.HostCAPEM); err != nil {
			p.logger().Warn("could not stage host CAs", "error", err)
			return
		}
		// Split the bundle into one cert per file (Alpine requirement) under
		// Skrog's own prefix, then rebuild the trust store.
		split := "rm -f " + hostCAGlob + "; " +
			`awk '/-----BEGIN CERTIFICATE-----/{n++} {print > ("` + hostCADir + `/skrog-host-" n ".crt")}' ` + hostCABundle + "; " +
			"update-ca-certificates 2>&1"
		if out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", split); err != nil {
			p.logger().Warn("installing host CAs failed", "error", err, "output", strings.TrimSpace(out))
		} else {
			p.logger().Info("imported host CA certificates into the engine trust store")
		}
	} else {
		// Off: remove everything Skrog added and refresh the bundle.
		p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
			"rm -f "+hostCAGlob+" "+hostCABundle+"; update-ca-certificates >/dev/null 2>&1 || true")
	}
}

// applyEngineDefaults lands engineconfig.Defaults in the distro's daemon.json
// for keys the user has not set. Runs before dockerd launches; see Defaults for
// why each one exists.
func (p *Provisioner) applyEngineDefaults(ctx context.Context, opts Options) {
	m := &engineconfig.Manager{WSL: p.wsl(), Distro: opts.Distro}
	added, err := m.ApplyDefaults(ctx)
	if err != nil {
		p.logger().Warn("engine defaults not applied", "err", err)
		return
	}
	for _, k := range added {
		p.logger().Info("engine default applied", "key", "engine."+k, "value", engineconfig.Defaults[k])
	}
}

// applyGPU writes or removes the NVIDIA CDI spec in the distro (#83), so a
// container started with `--device nvidia.com/gpu=all` gets the WSL GPU mounts.
// Best-effort, like applyNetwork: a spec-write failure logs but never blocks the
// engine from coming up.
func (p *Provisioner) applyGPU(ctx context.Context, opts Options) {
	v := opts.gpuVendor()
	if opts.GPUEnabled {
		if err := p.writeDistroFile(ctx, opts, v.SpecPath(), v.Spec()); err != nil {
			p.logger().Warn("could not install the GPU CDI spec", "error", err, "vendor", v)
		} else {
			p.logger().Info("GPU CDI spec installed", "path", v.SpecPath(), "vendor", v)
		}
	}
	// Remove every spec this vendor is not using, so switching vendors (or
	// turning GPU off) never leaves a stale kind behind that a container could
	// still select.
	//
	// One exec, not one per vendor (#398). Each wsl round trip is ~165 ms on a
	// warm distro, and this runs on EVERY engine start — so the loop was
	// spending a third of a second per start on `rm -f` for files that, on the
	// overwhelming majority of machines, have never existed.
	stale := make([]string, 0, len(gpu.Vendors()))
	for _, other := range gpu.Vendors() {
		if opts.GPUEnabled && other == v {
			continue
		}
		stale = append(stale, other.SpecPath())
	}
	if len(stale) > 0 {
		p.wsl().Exec(ctx, opts.Distro, "root", append([]string{"rm", "-f"}, stale...)...)
	}
}

// gpuVendor resolves the configured vendor, defaulting rather than failing: a
// bad value is refused by `config set`, and the engine must still start.
func (o Options) gpuVendor() gpu.Vendor {
	v, err := gpu.ParseVendor(o.GPUVendor)
	if err != nil {
		return gpu.DefaultVendor
	}
	return v
}

// GPUAvailable reports whether the engine distro can see the GPU: WSL's driver
// projection (/dev/dxg and libcuda) must be present, which it is only on an
// NVIDIA machine with a WSL-capable driver. Host-side and cheap; boots nothing.
func (p *Provisioner) GPUAvailable(ctx context.Context, opts Options) bool {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"[ -e "+gpu.DxgDevice+" ] && [ -e "+opts.gpuVendor().ProbeLib()+" ] && echo ok")
	return err == nil && strings.Contains(out, "ok")
}

// ConfigureGPU writes (enabled) or removes (disabled) the CDI spec in the distro
// immediately. dockerd reads CDI specs dynamically, so no restart is strictly
// required, but callers typically restart to be certain the change is live.
func (p *Provisioner) ConfigureGPU(ctx context.Context, opts Options, enabled bool) error {
	opts = opts.withDefaults()
	opts.GPUEnabled = enabled
	if enabled {
		return p.writeDistroFile(ctx, opts, opts.gpuVendor().SpecPath(), opts.gpuVendor().Spec())
	}
	var err error
	for _, v := range gpu.Vendors() {
		_, err = p.wsl().Exec(ctx, opts.Distro, "root", "rm", "-f", v.SpecPath())
	}
	return err
}

// GPUSpecInstalled reports whether the CDI spec is present in the distro.
func (p *Provisioner) GPUSpecInstalled(ctx context.Context, opts Options) bool {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"[ -f "+opts.gpuVendor().SpecPath()+" ] && echo yes")
	return err == nil && strings.Contains(out, "yes")
}

// GPUHookInstalled reports whether nvidia-cdi-hook is present in the distro
// (#139). Its presence when dockerd starts is what makes moby route
// `docker run --gpus all` to the CDI spec; without it only
// `--device nvidia.com/gpu=all` works. Rootfs 29.7.2-4 and later ship it.
func (p *Provisioner) GPUHookInstalled(ctx context.Context, opts Options) bool {
	opts = opts.withDefaults()
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"command -v nvidia-cdi-hook >/dev/null 2>&1 && echo yes")
	return err == nil && strings.Contains(out, "yes")
}

// writeDistroFile writes content to a path in the distro. The content is staged
// in a host temp file the distro reads over the /mnt automount, rather than
// passed as a shell argument — a full CA bundle is hundreds of KB, well past the
// command-line length limit.
func (p *Provisioner) writeDistroFile(ctx context.Context, opts Options, path string, content []byte) error {
	tmp, err := os.CreateTemp(opts.StateDir, ".distrowrite-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	mnt, err := winpath.ToWSL(tmpName)
	if err != nil {
		return err
	}
	cmd := "mkdir -p \"$(dirname " + path + ")\" && cp '" + mnt + "' " + path
	_, err = p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", cmd)
	return err
}

// StartEngine launches dockerd and waits for its socket.
func (p *Provisioner) StartEngine(ctx context.Context, opts Options) error {
	opts = opts.withDefaults()

	if running, _ := p.engineRunning(ctx, opts); running {
		p.logger().Info("engine already running", "distro", opts.Distro)
		// Neither the agent nor the socket share is tied to dockerd's
		// lifetime: a supervisor that finds a healthy engine (its own
		// restart, say) must still make sure both are up.
		p.ensureAgentSecret(ctx, opts)
		p.startAgent(ctx, opts)
		p.shareEngineSocket(ctx, opts)
		return nil
	}

	// Corporate-network config is applied before launch and sourced by the
	// dockerd command, so proxy env and trusted CAs are in place for the very
	// first registry pull (#62).
	p.applyNetwork(ctx, opts)

	// GPU CDI spec, likewise applied before launch so the engine picks it up on
	// startup and a rootfs re-import keeps GPU access (#83).
	p.applyGPU(ctx, opts)

	// Engine defaults Skrog holds an opinion on (engineconfig.Defaults), for
	// installs whose daemon.json predates them. Only absent keys are written,
	// and only after `dockerd --validate` accepts the result; a failure here
	// is logged, not fatal -- the engine must still come up.
	p.applyEngineDefaults(ctx, opts)

	p.logger().Info("starting dockerd", "distro", opts.Distro)
	// Output goes to a log inside the distro; the caller gets it via
	// `skrog logs` rather than having it interleaved here.
	if _, err := p.wsl().Start(ctx, opts.Distro, "root",
		"sh", "-c", "[ -f /etc/skrog/network.env ] && . /etc/skrog/network.env; dockerd >>/var/log/dockerd.log 2>&1"); err != nil {
		return fmt.Errorf("launching dockerd: %w", err)
	}
	p.ensureAgentSecret(ctx, opts)
	p.startAgent(ctx, opts)

	deadline := time.Now().Add(opts.StartTimeout)
	for time.Now().Before(deadline) {
		if running, _ := p.engineRunning(ctx, opts); running {
			p.logger().Info("engine socket is up", "distro", opts.Distro)
			// Shared only once the socket exists: a bind mount of a missing
			// file cannot be made, and the share is re-done on every start
			// because the previous engine's bind went stale with it.
			p.shareEngineSocket(ctx, opts)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}

	// Include the daemon's own last words; without them this is undiagnosable.
	log, _ := p.wsl().Exec(ctx, opts.Distro, "root", "tail", "-30", "/var/log/dockerd.log")
	return fmt.Errorf("dockerd did not create %s within %s. Last log lines:\n%s",
		EngineSocket, opts.StartTimeout, log)
}

// SharedSocketPath is where the engine socket is bind-mounted for other WSL
// distros (#42): /mnt/wsl is a tmpfs shared VM-wide across every distro, and a
// bind mount of the socket file shares the live inode, which a symlink cannot
// (it would resolve in the reader's own namespace, where /var/run/docker.sock
// does not exist). Namespaced by distro name so a second install — the e2e
// suite runs one beside a real install — shares its own socket, not a
// collision.
func SharedSocketPath(distro string) string {
	return "/mnt/wsl/" + distro + "/docker.sock"
}

// shareEngineSocket publishes the engine socket at SharedSocketPath,
// best-effort: sharing is Desktop-parity plumbing for wsl-integrate, and its
// failure must not fail an engine start. The path travels as a positional
// parameter, never spliced into the script, so a hostile distro name cannot
// inject shell. A stale share from a previous engine run (the bind outlives
// the distro in the VM's tmpfs) is unmounted first.
//
// The socket is chmod'd 0666 so the *ordinary* user in an integrated distro
// can reach it — dockerd creates it 0660 root:root, and UIDs/groups do not
// map reliably across distros, so group-based access is unreliable where a
// world bit is not. This does not widen the trust boundary: the engine is
// already reachable by any process in the user's Windows session through the
// named pipe, and /mnt/wsl is shared only among that user's own distros in
// their own utility VM. chmod targets the shared inode, so the engine's own
// /var/run/docker.sock (which only root touches inside the engine distro) is
// affected too, which is immaterial there.
func (p *Provisioner) shareEngineSocket(ctx context.Context, opts Options) {
	const script = `dir=$(dirname "$1") && mkdir -p "$dir" && ` +
		`{ umount "$1" 2>/dev/null || true; } && rm -f "$1" && touch "$1" && ` +
		`mount --bind /var/run/docker.sock "$1" && chmod 0666 "$1"`
	if _, err := p.wsl().Exec(ctx, opts.Distro, "root",
		"sh", "-c", script, "sh", SharedSocketPath(opts.Distro)); err != nil {
		p.logger().Debug("engine socket not shared to /mnt/wsl", "error", err)
	}
}

// agentBinaries are the in-distro agent's names, newest first.
//
// The rename from Hawser to Skrog (#1) renamed the binary the rootfs build
// produces, but a published rootfs tarball is immutable and its checksum is
// pinned in this build's manifest — so every already-installed engine, and
// every rootfs cut before the rename, still carries `hawser-agent`. Looking
// only for the new name would have found nothing on those, and because
// agentStartCmd is deliberately never fatal, the failure would have been
// silent: no agent, no vsock, and the bridge quietly falling back to socat at
// ~165 ms per connection instead of ~0.6 ms.
//
// Drop the old name once the manifest's oldest published rootfs ships the new
// one, and not before.
var agentBinaries = []string{"skrog-agent", "hawser-agent"}

// agentStartCmd is what launches the agent (#40), guarded three ways: a
// rootfs that predates the agent has nothing to start (the socat relay stays
// the transport), an agent already running must not be doubled, and `exec`
// keeps the process tree flat. Never fatal by design — the engine is fully
// usable without the vsock path.
//
// The agent's -socket is passed explicitly as EngineSocket (#92): both
// transports must target the same engine socket. A host-side `--socket`
// override on `skrog proxy` steers only the socat fallback and cannot reach
// the agent (which runs in-distro and connects to dockerd's real socket
// there), so binding the agent to the same constant keeps the two from
// silently diverging.
func agentStartCmd() string {
	var b strings.Builder
	for _, bin := range agentBinaries {
		fmt.Fprintf(&b, "if command -v %s >/dev/null 2>&1; then "+
			"pgrep -x %s >/dev/null 2>&1 && exit 0; "+
			"exec %s -socket %s -secret-file %s >>/var/log/%s.log 2>&1; fi; ",
			bin, bin, bin, EngineSocket, DistroAgentSecret, bin)
	}
	b.WriteString("exit 0")
	return b.String()
}

func (p *Provisioner) startAgent(ctx context.Context, opts Options) {
	if _, err := p.wsl().Start(ctx, opts.Distro, "root", "sh", "-c", agentStartCmd()); err != nil {
		p.logger().Debug("agent not started", "error", err)
	}
}

// AgentSecretPath is where the host copy of the per-install agent secret lives
// (#81); the dialer reads it to authenticate the agent.
func AgentSecretPath(stateDir string) string {
	return filepath.Join(stateDir, "agent-secret")
}

// agentSecretScript generates the secret inside the distro on first run and
// prints it. Generating in-distro (from /dev/urandom) keeps the secret out of
// any process argv; the value crosses only the wsl.exe stdout pipe, host to
// distro, within the user's own session.
// DistroAgentSecret is where the secret lives inside the distro.
//
// It is passed to the agent explicitly (see agentStartCmd) rather than left to
// the agent's own default, because the default moved with the rename: an agent
// built before it defaults to /etc/hawser/agent-secret, and rootfs 29.7.2 --
// still a live target for `--engine-version` and `engine rollback` -- ships
// exactly that agent. The host would write the secret here, demand the
// authenticated handshake, and the agent would look somewhere else, find
// nothing, offer v1, and be refused as a downgrade. Silently, because
// startAgent is never fatal: no vsock, socat at ~165ms per connection instead
// of ~0.6ms (#239).
//
// Both the old and the new agent accept -secret-file, so naming it is enough;
// only the default differs.
const (
	distroConfDir     = "/etc/skrog"
	DistroAgentSecret = distroConfDir + "/agent-secret"
)

const agentSecretScript = `f=` + DistroAgentSecret + `; ` +
	`[ -s "$f" ] || { mkdir -p ` + distroConfDir + ` && umask 077 && ` +
	`head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$f"; }; cat "$f"`

// ensureAgentSecret makes the agent's vsock handshake mutually authenticated
// (#81): a per-install secret lives root-only in the distro and user-only on
// the host, so a sibling distro squatting the vsock port cannot prove itself.
// Best-effort — a failure leaves both ends on the pre-#81 handshake rather
// than breaking the engine — and idempotent: the secret is generated once and
// reused, so agent and dialer always agree.
//
// Gated on the agent supporting auth: a /1 agent (an older rootfs) would
// answer the unauthenticated handshake, which a secret-holding host refuses as
// a downgrade — so against such a rootfs we provision no secret and remove any
// stale one, keeping the vsock path working rather than forcing socat.
func (p *Provisioner) ensureAgentSecret(ctx context.Context, opts Options) {
	hostPath := AgentSecretPath(opts.StateDir)

	ver, _ := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c",
		"skrog-agent -version 2>/dev/null || hawser-agent -version 2>/dev/null || true")
	if !strings.Contains(ver, "skrog-agent/2") && !strings.Contains(ver, "hawser-agent/2") {
		// No auth-capable agent: ensure the host holds no secret, so the
		// dialer uses the v1 handshake this agent understands.
		os.Remove(hostPath)
		return
	}

	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", agentSecretScript)
	if err != nil {
		p.logger().Debug("agent auth secret not provisioned; using the unauthenticated handshake", "error", err)
		return
	}
	secret := strings.TrimSpace(out)
	if secret == "" {
		return
	}
	if b, err := os.ReadFile(hostPath); err == nil && strings.TrimSpace(string(b)) == secret {
		return // already mirrored
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		p.logger().Debug("cannot mirror agent secret to host", "error", err)
		return
	}
	tmp := hostPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(secret+"\n"), 0o600); err != nil {
		p.logger().Debug("cannot write host agent secret", "error", err)
		return
	}
	if err := os.Rename(tmp, hostPath); err != nil {
		os.Remove(tmp)
		p.logger().Debug("cannot commit host agent secret", "error", err)
	}
}

// enginePing asks dockerd itself, over its socket, using only what the rootfs
// already ships (socat): a stale socket file left by a crashed dockerd must
// read as DOWN, not up (#82 — `test -S` said "running" forever after an
// OOM-kill, so the supervisor never repaired and status lied).
const enginePing = `printf 'GET /_ping HTTP/1.1\r\nHost: skrog\r\nConnection: close\r\n\r\n'` +
	` | socat -t 2 - UNIX-CONNECT:` + EngineSocket

func (p *Provisioner) engineRunning(ctx context.Context, opts Options) (bool, error) {
	// Never exec into the distro without knowing it is already running:
	// wsl.exe BOOTS a stopped distro to run the command (#82), so a health
	// probe against an idle-stopped engine would revive the distro/VM every
	// few seconds and defeat the RAM reclaim idle-stop exists for. Listing is
	// a pure host-side query.
	distros, err := p.wsl().List(ctx)
	if err != nil {
		return false, err
	}
	alive := false
	for _, d := range distros {
		if d.Name == opts.Distro && strings.EqualFold(d.State, "Running") {
			alive = true
			break
		}
	}
	if !alive {
		return false, nil
	}

	out, err := p.wsl().Exec(ctx, opts.Distro, "root", "sh", "-c", enginePing)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "200 OK"), nil
}

// Uninstall removes the distro and Skrog's own state, and nothing else.
//
// Best-effort by design: a partially installed machine must still come clean,
// so a missing distro or absent state directory is not an error. Errors are
// collected and reported together.
func (p *Provisioner) Uninstall(ctx context.Context, opts Options) error {
	opts = opts.withDefaults()

	// A recorded manifest is more trustworthy than the caller's options: it says
	// where this install actually put things.
	if m, err := p.ReadManifest(opts); err == nil {
		if m.Distro != "" {
			opts.Distro = m.Distro
		}
		if m.DataDir != "" {
			opts.DataDir = m.DataDir
		}
	}

	var errs []error

	distros, err := p.wsl().List(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("listing distros: %w", err))
	}
	registered := false
	for _, d := range distros {
		if d.Name == opts.Distro {
			registered = true
		}
	}

	if registered {
		// Clear the /mnt/wsl share first, while the distro can still run a
		// command: after unregister the dead socket file would sit in the
		// VM's shared tmpfs until the next VM restart. Best-effort — a
		// stopped distro would be booted just to clean a tmpfs entry.
		const unshare = `umount "$1" 2>/dev/null; rm -rf "$(dirname "$1")"`
		p.wsl().Exec(ctx, opts.Distro, "root",
			"sh", "-c", unshare, "sh", SharedSocketPath(opts.Distro))

		p.logger().Info("terminating distro", "distro", opts.Distro)
		if err := p.wsl().Terminate(ctx, opts.Distro); err != nil {
			// Not fatal: unregister stops it anyway.
			p.logger().Warn("terminate failed, continuing", "error", err)
		}
		p.logger().Info("unregistering distro", "distro", opts.Distro)
		if err := p.wsl().Unregister(ctx, opts.Distro); err != nil {
			errs = append(errs, fmt.Errorf("unregistering %s: %w", opts.Distro, err))
		}
	} else {
		p.logger().Info("distro not registered, nothing to unregister", "distro", opts.Distro)
	}

	// Only paths Skrog created. DataDir is removed because wsl --unregister
	// deletes the VHDX but leaves the directory.
	if err := os.RemoveAll(opts.DataDir); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", opts.DataDir, err))
	}
	if err := os.Remove(p.manifestPath(opts)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing manifest: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("uninstall completed with errors: %w", errsJoin(errs))
	}
	return nil
}

func errsJoin(errs []error) error {
	msg := ""
	for i, e := range errs {
		if i > 0 {
			msg += "; "
		}
		msg += e.Error()
	}
	return fmt.Errorf("%s", msg)
}

func (p *Provisioner) manifestPath(opts Options) string {
	return filepath.Join(opts.withDefaults().StateDir, "manifest.json")
}

func (p *Provisioner) writeManifest(opts Options, m *Manifest) error {
	path := p.manifestPath(opts)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	// Atomic write (#93): a crash mid-write left a truncated manifest.json,
	// after which ReadManifest errors and the supervisor exits "no install
	// found" until a reinstall. Temp-plus-rename means a reader sees either
	// the old file or the whole new one, never a partial.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("committing manifest: %w", err)
	}
	return nil
}

// ReadManifest returns what a previous install recorded.
func (p *Provisioner) ReadManifest(opts Options) (*Manifest, error) {
	b, err := os.ReadFile(p.manifestPath(opts))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	// An install made before the move to the wslkit organisation recorded a
	// URL that only resolves through GitHub's redirect (#212). Correcting it
	// here means every reader — `engine rollback`, which re-fetches from this
	// exact URL, above all — stops depending on that redirect. The file on
	// disk is rewritten by the next manifest write; nothing needs migrating
	// for behaviour to be right in the meantime.
	m.RootfsURL, _ = CanonicalRootfsURL(m.RootfsURL)
	return &m, nil
}

// EngineRunning reports whether the engine socket answers inside the distro.
func (p *Provisioner) EngineRunning(ctx context.Context, opts Options) bool {
	opts = opts.withDefaults()
	running, _ := p.engineRunning(ctx, opts)
	return running
}

// EngineRunningErr is EngineRunning without the lie that a failed probe means
// a stopped engine (#437).
//
// Collapsing the error into false is right for a `waitFor` poll, which is what
// most callers are: they are asking "is it up YET", and "cannot tell" and "not
// yet" both mean keep waiting. It is wrong for the supervisor, whose next move
// after seeing false is to START the engine — so a probe that merely failed
// would have it start one that is probably already running.
//
// Same probe, both answers kept. Use this where the difference matters.
func (p *Provisioner) EngineRunningErr(ctx context.Context, opts Options) (bool, error) {
	return p.engineRunning(ctx, opts.withDefaults())
}

// StopEngine terminates the engine's own distro — and nothing else. This is
// the only stop primitive Skrog has on purpose: `wsl --shutdown` stops every
// distro on the machine, including Docker Desktop's and the user's own, and is
// never called (PLAN §02; the coexistence note on #35).
func (p *Provisioner) StopEngine(ctx context.Context, opts Options) error {
	opts = opts.withDefaults()
	p.logger().Info("terminating distro", "distro", opts.Distro)
	return p.wsl().Terminate(ctx, opts.Distro)
}

// SaveManifest persists an updated install manifest. Exported for `skrog
// engine upgrade` (#65), which changes what is installed without reinstalling:
// the record of which engine is in the distro, and which one to roll back to,
// has to move with it.
func (p *Provisioner) SaveManifest(opts Options, m *Manifest) error {
	return p.writeManifest(opts.withDefaults(), m)
}

// verifyRootfsSignature checks the rootfs against the Sigstore signature the
// release workflow published beside it (#147).
//
// It refuses rather than warns: verification was asked for, and an install that
// says "could not verify, carrying on" is the failure this exists to prevent.
// The two "cannot check" cases get their own messages, because "no verifier
// installed" and "this release was never signed" need different actions.
func (p *Provisioner) verifyRootfsSignature(ctx context.Context, opts Options, tarball string) error {
	v := &rootfsverify.Verifier{
		Fetch:   p.fetcher(),
		Repo:    signingRepo,
		TempDir: filepath.Join(opts.StateDir, "rootfs"),
	}
	res, err := v.Verify(ctx, opts.RootfsURL, tarball)
	switch {
	case errors.Is(err, rootfsverify.ErrNoVerifier):
		return fmt.Errorf("%w.\n"+
			"  install.verify-signature is on, and verification needs cosign on PATH:\n"+
			"    winget install sigstore.cosign   (or see https://docs.sigstore.dev)\n"+
			"  Turn it off with `skrog config set install.verify-signature off` to install\n"+
			"  on the SHA-256 pin alone, which is always enforced", err)
	case err != nil:
		var nm *rootfsverify.ErrNoMaterial
		if errors.As(err, &nm) {
			return fmt.Errorf("%w.\n"+
				"  Releases from before signing was added (and rootfs images you built\n"+
				"  yourself) carry no signature. The SHA-256 pin still applies; turn the\n"+
				"  check off with `skrog config set install.verify-signature off` to\n"+
				"  install this one", err)
		}
		return err
	}
	p.logger().Info("rootfs signature verified", "digest", res.Digest, "identity", res.SignedBy)
	return nil
}

// signingRepo is the repository whose release workflow signs the rootfs. A
// signature made by any other repository's workflow -- a fork's included -- is
// rejected, so this is a trust anchor and not a convenience.
const signingRepo = "wslkit/skrog"

// BackendName is the manifest's backend, resolving the empty value every
// install written before #335 carries.
//
// A nil receiver reports the distro backend too: callers reach this from a
// manifest that may not have loaded, and "no manifest" has never meant "wslc".
func (m *Manifest) BackendName() string {
	if m == nil || m.Backend == "" {
		return BackendDistro
	}
	return m.Backend
}

// IsWslc reports whether this install serves a WSL container session.
func (m *Manifest) IsWslc() bool { return m.BackendName() == BackendWslc }
