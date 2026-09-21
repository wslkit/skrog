package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockercli"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/hooks"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/remote"
	"github.com/wslkit/skrog/internal/runner"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/version"
	"github.com/wslkit/skrog/internal/vpnfingerprint"
	"github.com/wslkit/skrog/internal/wsl"
	"github.com/wslkit/skrog/internal/wslconfig"
)

// Facts is everything the checks read, gathered once from the machine. Checks
// are pure functions of a Facts value, so a test builds the struct by hand and
// never touches WSL, PATH, the registry, or an engine.
type Facts struct {
	// StateDir is Skrog's resolved state directory.
	StateDir string
	// AppVersion is Skrog's own build version.
	AppVersion string

	// WSL is the host's WSL status. WSLErr is set when querying it failed.
	WSL    wsl.Status
	WSLErr string

	// WSLFastPath reports whether the COM fast path to wslservice is in use,
	// and why not when it is not (#356). A machine that has silently fallen
	// back to spawning wsl.exe is not broken, only slower -- which is exactly
	// the kind of thing that goes unnoticed until someone profiles it.
	WSLFastPath    bool
	WSLFastPathWhy string

	// Report is the `skrog version` picture: docker binaries on PATH, active
	// context, negotiated API version, and the install manifest. Never nil after
	// Gather.
	Report *version.Report

	// EngineReachable is true when the engine answers in its distro. EngineIdle
	// is true when it is down by design (idle timeout), which is healthy.
	EngineReachable bool
	EngineIdle      bool
	// Desired is the supervisor's desired engine state ("running"/"stopped").
	Desired string
	// SupervisorHeld is true when the single-instance lock is held, i.e. a
	// supervisor is running.
	SupervisorHeld bool

	// ServedEndpoint is the DOCKER_HOST the running supervisor actually bound,
	// and ActiveEndpoint is the one the active docker context points at. Both
	// are empty when there is nothing to ask — no supervisor, no context, or no
	// docker CLI to ask with.
	//
	// They exist so checks can compare endpoints rather than context names:
	// when Skrog takes the default pipe, the stock `default` context already
	// reaches it, and a name comparison calls that a misconfiguration (#283).
	ServedEndpoint string
	ActiveEndpoint string

	// CredHelpers are the docker credential helpers the CLI config references,
	// each annotated with whether its binary resolves on PATH.
	CredHelpers []CredHelper

	// Disk describes free space on the volume holding the engine's data.
	Disk DiskInfo
	// DiskWarnBelow is the configured free-space floor (`disk.warn-below`,
	// #145) in bytes; 0 means checkDisk's built-in default.
	DiskWarnBelow uint64

	// SSHAgent is how `docker build --ssh default` would find an agent (#391).
	SSHAgent SSHAgentInfo

	// MultiArch is whether the default buildx driver can cross-build (#384).
	MultiArch MultiArchInfo

	// MountTransport is what the engine distro actually mounts Windows drives
	// over: "virtiofs", "9p", or "" when the engine was down and nothing was
	// measured (#327). Read live rather than inferred from ~/.wslconfig,
	// which disagrees with reality in both directions that matter.
	MountTransport string

	// Session0 is advisory guidance about unattended (no-logon) operation; see
	// checkSession0.
	Session0 Session0Info

	// Proxy is the configured engine proxy URL; ImportHostCAs whether host CA
	// trust is on (#62).
	Proxy         string
	ImportHostCAs bool

	// VPNs are the corporate VPN clients detected from the host's active network
	// adapters, each with its recommended connectivity settings (#63).
	VPNs []vpnfingerprint.Match

	// CLI is the bundled docker CLI's install/PATH state (#66).
	CLI CLIStatus

	// GPU is the NVIDIA GPU-passthrough state (#83).
	GPU GPUStatus

	// Remotes are the registered remote engines (#138), so checkContext can tell
	// "docker is on a remote we know" from "docker is aimed somewhere odd".
	Remotes []remote.Info

	// Runner is the unattended-host setup (#150): auto-logon, autostart,
	// supervisor, engine. Its account fields are compared, never rendered.
	Runner runner.Facts

	// WSLSizing is the WSL2 VM.s effective sizing from the global ~/.wslconfig,
	// plus what Skrog.s own settings would change (#148).
	WSLSizing WSLSizingInfo

	// InjectedModules are third-party DLLs loaded into Skrog.s own process
	// (#166) -- endpoint-security agents, almost always. Reported because they
	// are the known cause of a supervisor crash no dump can explain.
	InjectedModules []hooks.Module
}

// GPUStatus describes GPU passthrough (#83): whether it is turned on in config,
// and — only when the engine is already running, so doctor never boots it (#82)
// — whether the distro actually sees the GPU and the CDI spec is installed.
type GPUStatus struct {
	// EngineInstalled gates the check; GPU is meaningless without an engine.
	EngineInstalled bool `json:"engineInstalled"`
	// ConfigEnabled is the `gpu` setting (`skrog enable-gpu`).
	ConfigEnabled bool `json:"configEnabled"`
	// Vendor is which vendor spec is configured (#185); empty means nvidia.
	Vendor string `json:"vendor,omitempty"`
	// Probed is true when the running engine was queried for the two below;
	// false means the engine was down, so they are not authoritative.
	Probed bool `json:"probed"`
	// Visible is true when /dev/dxg and the WSL CUDA library are present.
	Visible bool `json:"visible"`
	// SpecInstalled is true when the CDI spec is present in the distro.
	SpecInstalled bool `json:"specInstalled"`
}

// CLIStatus describes the bundled docker CLI (#66): whether it is installed,
// where, whether its directory is on the user PATH, and which docker the shell
// actually resolves — so checkCLI can tell "installed and active" from "installed
// but Docker Desktop still wins".
type CLIStatus struct {
	Installed    bool   `json:"installed"`
	BinDir       string `json:"binDir"`
	OnPath       bool   `json:"onPath"`
	ActiveDocker string `json:"activeDocker,omitempty"`
	// ActiveOrigin is what `skrog version` attributed ActiveDocker to, empty
	// when nothing recognised it; ShadowScope is which PATH its directory is
	// on. Together they let checkCLI say what is actually in the way and what
	// would actually move it, instead of guessing Docker Desktop (#282).
	ActiveOrigin string          `json:"activeOrigin,omitempty"`
	ShadowScope  dockercli.Scope `json:"-"`
}

// CredHelper is one docker credential helper referenced by the CLI config, and
// whether its executable can be found. A referenced-but-missing helper is what
// broke `skrog migrate` live (docker-credential-wincred absent from PATH).
type CredHelper struct {
	// Name is the helper's short name, e.g. "desktop", "wincred".
	Name string `json:"name"`
	// Source says where it was configured: "credsStore" or "credHelpers[<reg>]".
	Source string `json:"source"`
	// Binary is the executable docker will exec, e.g. "docker-credential-wincred".
	Binary string `json:"binary"`
	// Resolved is true when Binary was found on PATH; Path is where.
	Resolved bool   `json:"resolved"`
	Path     string `json:"path,omitempty"`
}

// DiskInfo is free/total space on a volume, in bytes. Err is set when the query
// failed (a non-Windows host, or a path that does not exist yet).
type DiskInfo struct {
	Path       string `json:"path"`
	FreeBytes  uint64 `json:"freeBytes"`
	TotalBytes uint64 `json:"totalBytes"`
	Err        string `json:"err,omitempty"`
}

// SSHAgentInfo is how `docker build --ssh default` would find an SSH agent.
// Err is set when the question could not be asked (a non-Windows host).
type SSHAgentInfo struct {
	// AuthSock is SSH_AUTH_SOCK when set; buildx prefers it over the pipe.
	AuthSock string `json:"authSock,omitempty"`
	// PipeExists is true when the Windows OpenSSH agent pipe is present.
	PipeExists bool   `json:"pipeExists"`
	Err        string `json:"err,omitempty"`
}

// MultiArchInfo is the foreign-architecture emulation available to the default
// buildx driver (#384). Probed is false when the engine was down, in which case
// Handlers says nothing — doctor never boots a distro to answer (#82).
//
// The handlers are read from the WSL2 utility VM's binfmt_misc table, which
// every distro on the machine shares, so an entry here may belong to someone's
// Ubuntu rather than to Skrog.
type MultiArchInfo struct {
	Probed   bool     `json:"probed"`
	Handlers []string `json:"handlers,omitempty"`
}

// Session0Info carries the advisory session-0 guidance. Verifying the account
// right reliably needs elevation, so doctor explains it rather than asserting a
// value it may not be able to read as a standard user.
type Session0Info struct {
	// AutostartConfigured is true when a logon autostart is registered; it is
	// the common path and makes the session-0 note purely informational.
	AutostartConfigured bool `json:"autostartConfigured"`
}

// GatherOptions locates what Gather needs.
type GatherOptions struct {
	StateDir   string
	SkrogBin   string
	AppVersion string
	// AutostartConfigured reports whether logon autostart is set up; supplied by
	// the caller because it lives in an OS-specific package.
	AutostartConfigured bool
}

// Gather reads the machine into Facts, degrading gracefully: doctor is what you
// run when things are broken, so a component that cannot be read is recorded as
// unknown rather than aborting the run.
func Gather(ctx context.Context, opts GatherOptions) Facts {
	stateDir := opts.StateDir
	pOpts := provision.Options{StateDir: stateDir}
	p := &provision.Provisioner{}
	w := wsl.NewFast()
	defer w.Close()

	f := Facts{
		StateDir:   stateDir,
		AppVersion: opts.AppVersion,
		Desired:    string(supervise.ReadDesired(stateDir)),
		Session0:   Session0Info{AutostartConfigured: opts.AutostartConfigured},
	}

	f.WSLFastPath, f.WSLFastPathWhy = w.Accelerated()

	if st, err := w.Status(ctx); err != nil {
		f.WSLErr = err.Error()
	} else {
		f.WSL = st
	}

	f.Report = (&version.Collector{
		App:         opts.AppVersion,
		Env:         version.Env{SkrogBin: opts.SkrogBin},
		WSL:         w,
		Provisioner: p,
		Options:     pOpts,
	}).Collect(ctx)

	// Reachability is a host-side probe that never boots a stopped distro (#82).
	if f.Report.Engine.Installed {
		pOpts.Distro = f.Report.Engine.Distro
		f.EngineReachable = p.EngineRunning(ctx, pOpts)
		f.EngineIdle = supervise.ReadEngineState(stateDir) == supervise.EngineIdle
		f.GPU.EngineInstalled = true
	}
	f.SupervisorHeld = supervise.Held(stateDir)

	// Only under the lock: the endpoint record outlives a supervisor killed
	// hard, and comparing against a pipe nothing is serving would be worse than
	// not comparing at all (#273).
	f.ServedEndpoint, f.ActiveEndpoint = endpointFacts(ctx, stateDir,
		f.SupervisorHeld, f.Report.Context,
		supervise.ReadEndpoint,
		(&dockerctx.Manager{}).EndpointOf)

	f.CredHelpers = discoverCredHelpers(dockerConfigPath(), execLookPath)
	f.Disk = diskInfo(engineDataDir(stateDir))
	f.SSHAgent = sshAgentInfo()

	if c, err := config.Load(stateDir); err == nil {
		f.Proxy = c.Proxy
		f.ImportHostCAs = c.ImportHostCAs
		f.GPU.ConfigEnabled = c.GPU
		f.GPU.Vendor = c.GPUVendor
		pOpts.GPUVendor = c.GPUVendor
		f.DiskWarnBelow = c.DiskWarnBelow
	}

	// GPU distro probes only when the engine is already up: GPUAvailable uses
	// wsl exec, which would boot a stopped distro, and doctor must not (#82).
	if f.EngineReachable {
		f.GPU.Probed = true
		f.GPU.Visible = p.GPUAvailable(ctx, pOpts)
		f.GPU.SpecInstalled = p.GPUSpecInstalled(ctx, pOpts)
		// Same gate, same reason (#82): reading the handler table execs in the
		// distro, and doctor must never boot a stopped one to answer.
		f.MultiArch.Probed = true
		f.MultiArch.Handlers = p.BinfmtHandlers(ctx, pOpts)
		// Same gate again (#82). One grep of /proc/mounts, and the only
		// honest way to answer "am I on virtiofs": the config file says what
		// was asked for, not what took.
		f.MountTransport = p.MountTransport(ctx, pOpts)
	}

	f.VPNs = vpnfingerprint.Detect(gatherAdapters(ctx))
	f.CLI = gatherCLIStatus(stateDir)
	// Best-effort: an unreadable remotes dir means "no remotes", not a failure.
	f.Remotes, _ = (&remote.Manager{StateDir: stateDir}).List()

	// Runner readiness (#150). The Winlogon read is a plain HKLM query (no
	// elevation) and the rest reuses facts already gathered above.
	f.Runner = runner.Facts{
		AutostartRegistered: opts.AutostartConfigured,
		SupervisorRunning:   f.SupervisorHeld,
		Engine:              runnerEngineState(f),
	}
	if w, err := runner.ReadWinlogon(); err == nil {
		f.Runner.AutoLogonConfigured = w.Configured()
		f.Runner.AutoLogonUser, f.Runner.AutoLogonDomain = w.DefaultUserName, w.DefaultDomainName
		f.Runner.PlaintextPassword = w.HasDefaultPassword
	}
	f.Runner.CurrentUser, f.Runner.CurrentDomain = runner.CurrentAccount()
	// Probed here too, so `skrog doctor` on a runner covers sleep as well —
	// otherwise Evaluate would see no power facts and stay silent about it.
	f.Runner.Power = runner.ReadPower()

	// Third-party DLLs in this very process (#166): read from our own module
	// list, so it costs a snapshot call and no privileges.
	f.InjectedModules = hooks.Injected()

	// The global ~/.wslconfig, read only (#148).
	f.WSLSizing = gatherWSLSizing(stateDir)

	return f
}

// gatherCLIStatus reads the bundled docker CLI's install and PATH state (#66).
func gatherCLIStatus(stateDir string) CLIStatus {
	s := CLIStatus{}
	if stateDir == "" {
		return s
	}
	s.BinDir = filepath.Join(stateDir, "bin")
	if _, err := os.Stat(filepath.Join(s.BinDir, "docker.exe")); err == nil {
		s.Installed = true
	}
	if onPath, err := dockercli.UserPathContains(s.BinDir); err == nil {
		s.OnPath = onPath
	}
	if active, err := execLookPath("docker"); err == nil {
		s.ActiveDocker = active
		activeDir := filepath.Dir(active)
		// Only interesting when something else wins: the scope of our own bin
		// dir is never in question, and PathScopeOf reads the registry.
		if !strings.EqualFold(filepath.Clean(activeDir), filepath.Clean(s.BinDir)) {
			s.ShadowScope = dockercli.PathScopeOf(activeDir)
			for _, b := range version.FindDockerBinaries(version.Env{SkrogBin: s.BinDir}) {
				if strings.EqualFold(b.Path, active) {
					s.ActiveOrigin = string(b.Origin)
					break
				}
			}
		}
	}
	return s
}

// gatherAdapters reads the host's network adapters via Get-NetAdapter (a
// standard-user cmdlet, no elevation). It degrades to nil off Windows or when
// the cmdlet is unavailable: VPN detection is advisory, so a missing reading is
// simply "no VPN detected", never an error.
func gatherAdapters(ctx context.Context) []vpnfingerprint.Adapter {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Get-NetAdapter | Select-Object Name,InterfaceDescription,Status | ConvertTo-Json -Compress")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseAdapters(out)
}

// parseAdapters turns Get-NetAdapter's JSON into adapters. ConvertTo-Json emits
// a bare object for a single adapter and an array for several, so both shapes
// are handled. Pure, so the parsing is unit-tested without a host.
func parseAdapters(jsonOut []byte) []vpnfingerprint.Adapter {
	type raw struct {
		Name                 string `json:"Name"`
		InterfaceDescription string `json:"InterfaceDescription"`
		Status               string `json:"Status"`
	}
	trimmed := bytes.TrimSpace(jsonOut)
	if len(trimmed) == 0 {
		return nil
	}
	var many []raw
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &many); err != nil {
			return nil
		}
	} else {
		var one raw
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil
		}
		many = []raw{one}
	}
	out := make([]vpnfingerprint.Adapter, 0, len(many))
	for _, r := range many {
		out = append(out, vpnfingerprint.Adapter{
			Name:        r.Name,
			Description: r.InterfaceDescription,
			// Get-NetAdapter reports "Up" for a connected adapter.
			Up: r.Status == "Up",
		})
	}
	return out
}

// dockerConfigPath returns the docker CLI config location, honoring DOCKER_CONFIG.
func dockerConfigPath() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// engineDataDir is the volume whose free space matters: the distro's VHDX grows
// there. It mirrors provision's default (StateDir\distro) without importing its
// unexported helper.
func engineDataDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "distro")
}

func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// discoverCredHelpers reads the docker CLI config and reports each referenced
// credential helper with whether its binary resolves. Pure but for the injected
// lookPath and the file read, so the parsing is unit-tested via parseCredHelpers.
func discoverCredHelpers(configPath string, lookPath func(string) (string, error)) []CredHelper {
	if configPath == "" {
		return nil
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil // no config, or unreadable: nothing configured to check
	}
	return parseCredHelpers(b, lookPath)
}

// parseCredHelpers turns a docker config.json into the list of helpers it wires
// up, deduplicated, each resolved against lookPath.
func parseCredHelpers(configJSON []byte, lookPath func(string) (string, error)) []CredHelper {
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil
	}

	seen := map[string]bool{}
	var out []CredHelper
	add := func(name, source string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		bin := "docker-credential-" + name
		h := CredHelper{Name: name, Source: source, Binary: bin}
		if path, err := lookPath(bin); err == nil {
			h.Resolved, h.Path = true, path
		}
		out = append(out, h)
	}

	add(cfg.CredsStore, "credsStore")
	for registry, name := range cfg.CredHelpers {
		add(name, "credHelpers["+registry+"]")
	}
	return out
}

// WSLSizingInfo is the WSL2 VM's sizing picture (#148): what ~/.wslconfig sets
// today, what Skrog's settings ask for that is not there yet, and the host's
// RAM — which is what WSL's 50% default is half of, and therefore the only way
// to tell a sensible default from a tight one.
type WSLSizingInfo struct {
	Path      string            `json:"path"`
	Effective map[string]string `json:"effective"`
	// Pending renders each unapplied change as "key=value".
	Pending []string `json:"pending,omitempty"`
	// HostBytes is physical RAM; 0 when it could not be read, which the check
	// treats as "do not judge".
	HostBytes uint64 `json:"hostBytes,omitempty"`
	Err       string `json:"err,omitempty"`
}

// gatherWSLSizing reads the global ~/.wslconfig and compares it with Skrog's
// recorded wsl.* settings. Read-only: doctor never writes a file every distro
// on the machine shares.
func gatherWSLSizing(stateDir string) WSLSizingInfo {
	info := WSLSizingInfo{Effective: map[string]string{}, HostBytes: hostRAM()}
	path, err := wslconfig.Path()
	if err != nil {
		info.Err = err.Error()
		return info
	}
	info.Path = path
	f, err := wslconfig.Load(path)
	if err != nil {
		info.Err = err.Error()
		return info
	}
	info.Effective = f.All()

	desired := map[string]string{}
	for skrogKey, wslKey := range config.WSLKeys {
		if v, err := config.Get(stateDir, skrogKey); err == nil && strings.TrimSpace(v) != "" {
			desired[wslKey] = v
		}
	}
	for _, c := range f.Plan(desired) {
		info.Pending = append(info.Pending, c.Key+"="+c.New)
	}
	return info
}

// endpointFacts gathers the pair checkContext compares: what the running
// supervisor bound, and what the active docker context points at.
//
// Split out with its dependencies as parameters because inline in Gather it had
// no coverage at all -- deleting the context lookup reverted #283 entirely and
// the whole suite stayed green (#289). Gather itself cannot be unit-tested: it
// reads WSL, the registry, the filesystem and an engine.
//
// Both lookups are best-effort and both halves must be present to mean
// anything. No supervisor means nothing was bound; no record means nothing to
// compare; a context that cannot be inspected must never be assumed to match,
// because "equal" is what suppresses the warning.
func endpointFacts(
	ctx context.Context,
	stateDir string,
	supervisorHeld bool,
	activeContext string,
	readEndpoint func(string) (supervise.Endpoint, bool),
	endpointOf func(context.Context, string) (string, error),
) (served, active string) {
	// Only under the lock: the record outlives a supervisor killed hard, and
	// naming a pipe nothing is listening on is worse than saying nothing (#273).
	if !supervisorHeld {
		return "", ""
	}
	e, ok := readEndpoint(stateDir)
	if !ok {
		return "", ""
	}
	served = pipeproxy.DockerHostFor(e.Pipe)
	if activeContext != "" {
		active, _ = endpointOf(ctx, activeContext)
	}
	return served, active
}
