package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/audit"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/wslc"
)

// wslcBackend brings up the wslc engine transport: resolve a session, put the
// agent in its root namespace, and hand back a dialer pointed at it (#316,
// #320).
//
// Unlike the distro backend there is no FallbackDialer here. The socat
// fallback exists because an older rootfs may have no agent; a wslc session
// has no agent at all until we put one there, so if the bootstrap failed the
// right answer is a clear error, not a slow path that also will not work.
func wslcBackend(ctx context.Context, agentPath, stateDir string, log *slog.Logger) (pipeproxy.Dialer, *wslc.PortWatcher, string, error) {
	l := wslc.New()

	ver, err := l.Version(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("wslc is not usable on this machine: %w", err)
	}

	session, err := l.ResolveSession(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	log.Info("using wslc session", "session", session, "wslc", ver)
	if session != wslc.SessionName {
		// Worth saying out loud: Skrog's containers and the user's own `wslc`
		// containers share one engine and one policy surface here (#322).
		log.Warn("sharing the wslc CLI's default session; a dedicated session "+
			"needs an API the shipped CLI does not expose",
			"session", session)
	}

	agent, err := loadGuestAgent(ctx, agentPath)
	if err != nil {
		return nil, nil, "", err
	}

	secret, err := wslcSecret(stateDir)
	if err != nil {
		return nil, nil, "", err
	}

	// place reports the caveat; bootstrap (used for recovery) swallows it,
	// because by then the caller has already been told once and a re-bootstrap
	// that "fails" for a known limitation must not fail the dial.
	place := func(ctx context.Context) error {
		// Re-resolve the session every time: the one we started with may have
		// gone with its VM, and ResolveSession is a single cheap CLI call.
		s, err := l.ResolveSession(ctx)
		if err != nil {
			return err
		}
		return l.Bootstrap(ctx, s, agent, secret)
	}
	bootstrap := func(ctx context.Context) error {
		if err := place(ctx); err != nil && !errors.Is(err, wslc.ErrNoPortForwarding) {
			return err
		}
		return nil
	}

	// ErrNoPortForwarding is a caveat, not a failure: the engine works, only
	// published ports do not. Say it once here rather than letting it surface
	// as a port that mysteriously refuses.
	ports := true
	if err := place(ctx); err != nil {
		if !errors.Is(err, wslc.ErrNoPortForwarding) {
			return nil, nil, "", fmt.Errorf("bootstrapping the agent into session %q: %w", session, err)
		}
		ports = false
		log.Warn("published ports will not reach Windows on this agent",
			"reason", "the agent lifted from the engine distro predates -forward-port",
			"fix", "pass --agent with a binary built from this tree")
	}
	log.Info("agent running in the wslc session", "bytes", len(agent), "vsock-port", wslc.AgentPort)

	// Cooldown disabled: the dialer's post-failure pause exists to stop a
	// rootfs with no agent from paying a dial timeout per connection. Here a
	// failure means the VM restarted and the agent needs re-placing, so pausing
	// would only delay the fix.
	engine := &wslc.Dialer{
		Inner:     &pipeproxy.VsockDialer{Port: wslc.AgentPort, Secret: secret, Cooldown: -1},
		Bootstrap: bootstrap,
		Logger:    log,
	}
	// The forward transport needs the same recovery: one idle termination
	// takes both listeners down together.
	forward := &wslc.Dialer{
		Inner:     &pipeproxy.VsockDialer{Port: wslc.ForwardPort, Secret: secret, Cooldown: -1},
		Bootstrap: bootstrap,
		Logger:    log,
	}
	if !ports {
		// No forward listener in the guest: a watcher would open Windows
		// listeners that can never connect, which is worse than no listener.
		return engine, nil, session, nil
	}
	watcher := &wslc.PortWatcher{
		EngineDial:  engine.Dial,
		ForwardDial: forward.Dial,
		Logger:      log,
	}
	watcher.Lease = &wslc.Lease{
		Local:   l,
		Session: session,
		Logger:  log,
	}
	return engine, watcher, session, nil
}

// loadGuestAgent finds a linux skrog-agent to place in the session.
//
// Three sources, in the order that gives the right answer soonest:
//
//  1. An explicit --agent path. The caller means it.
//  2. skrog-agent NEXT TO THE EXECUTABLE. Releases ship it there precisely so
//     this backend needs no distro and no build step (#335).
//  3. Lifted out of the engine distro, which has one on PATH.
//
// The shipped copy is preferred over the distro's because it is the one that
// matches this binary. Lifting from the distro reaches whatever the installed
// rootfs happens to carry, and an older one silently costs published ports --
// the agent predating -forward-port is exactly the trap that made people pass
// --agent by hand.
func loadGuestAgent(ctx context.Context, path string) ([]byte, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the agent binary: %w", err)
		}
		return b, nil
	}

	if p, err := shippedAgentPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b, nil
		}
	}

	b, err := liftAgentFromDistro(ctx, provision.DefaultDistro)
	if err != nil {
		return nil, fmt.Errorf("no agent binary: expected skrog-agent next to %s, "+
			"and could not lift one from the engine distro either. "+
			"Pass --agent <path to a linux skrog-agent>, or build one with "+
			"`GOOS=linux GOARCH=amd64 go build -o skrog-agent ./guest/agent` (%w)",
			exeDirForMessage(), err)
	}
	return b, nil
}

// executablePath is os.Executable, indirected so tests can point the lookup at
// a temporary directory instead of the test binary's own.
var executablePath = os.Executable

// liftAgentFromDistro is agentFromDistro, indirected for the same reason: a
// developer machine HAS an engine distro, so a test asserting the
// no-agent-anywhere path would pass in CI and fail locally.
var liftAgentFromDistro = agentFromDistro

// shippedAgentPath is the agent a release puts beside skrog.exe.
func shippedAgentPath() (string, error) {
	exe, err := executablePath()
	if err != nil {
		return "", err
	}
	// Resolved through any symlink -- see internal/selfexe and #360. Kept
	// inline rather than calling selfexe.Path so the test seam
	// (executablePath) stays local to this file.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "skrog-agent"), nil
}

// exeDirForMessage names the directory in the error above, or a placeholder
// when the executable cannot be located -- the error is still useful without it.
func exeDirForMessage() string {
	p, err := shippedAgentPath()
	if err != nil {
		return "skrog.exe"
	}
	return p
}

// agentFromDistro copies the agent out of the engine distro.
//
// base64 rather than a raw stream on purpose: wsl.exe's output goes through
// UTF-16 detection and newline handling, which is fine for text and silently
// corrupts a binary.
func agentFromDistro(ctx context.Context, distro string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "-u", "root", "--exec",
		"sh", "-c", "base64 -w0 \"$(command -v skrog-agent)\"")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading skrog-agent from distro %s: %w", distro, err)
	}
	// wsl.exe may hand back UTF-16; base64's alphabet is ASCII, so dropping
	// NULs is enough to normalise either encoding.
	clean := strings.Map(func(r rune) rune {
		if r == 0 || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, string(out))
	if clean == "" {
		return nil, fmt.Errorf("distro %s has no skrog-agent on PATH", distro)
	}
	b, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("decoding the agent from distro %s: %w", distro, err)
	}
	return b, nil
}

// wslcSecret reuses the install's agent secret when there is one, so both
// backends authenticate the same way, and otherwise mints an ephemeral one.
//
// Ephemeral is safe because we are also the party installing it in the guest:
// the two ends are configured together in this process. What it must never be
// is empty — that would leave the agent speaking the unauthenticated v1
// handshake, and any process on the host can reach a VM's vsock ports.
func wslcSecret(stateDir string) (string, error) {
	if stateDir != "" {
		if b, err := os.ReadFile(provision.AgentSecretPath(stateDir)); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s, nil
			}
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating an agent secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// runProxyWslc serves the pipe against a wslc session instead of Skrog's own
// distro. Kept separate from runProxy rather than threaded through it: almost
// nothing before the listener is shared — there is no manifest, no distro to
// start, and path translation is not merely skipped but wrong (see below).
func runProxyWslc(agentPath, pipeName, sddl string, noContext bool, opts provision.Options, log *slog.Logger) int {
	// ONE interruptible context for everything, established before anything is
	// started. It used to be interruptCtx() -- literally context.Background()
	// and documented for short-lived setup calls -- which was then handed to the
	// port watcher and, through it, to the session lease. Nothing ever cancelled
	// it, so a clean Ctrl-C left the held wslc.exe orphaned (Windows does not
	// kill children with their parent) and a ~750 MB session VM resident for up
	// to DefaultLeaseHold with nothing using it.
	ctx, stop := interruptible()
	defer stop()

	dialer, watcher, session, err := wslcBackend(ctx, agentPath, optsWithResolvedStateDir(opts).StateDir, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	// Published ports are carried by Skrog on this backend, because the relay
	// that would carry them is driven from the Windows side by wslcsession and
	// a bridge talking to the engine socket never invokes it (#330). The
	// watcher opens and closes the host listeners as containers come and go.
	//
	// Nil when the guest agent is too old to forward; the warning has already
	// been logged and serving docker without published ports is the better of
	// the two available outcomes.
	if watcher != nil {
		go watcher.Run(ctx)
		defer watcher.StopAll()
	}

	selected, reason := selectWslcPipe(pipeName)
	dockerHost := pipeproxy.DockerHostFor(selected)

	listener, err := pipeproxy.Listen(selected, sddl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	defer listener.Close()

	log.Info("serving pipe", "pipe", selected, "reason", reason)
	log.Info("relaying to the wslc engine", "session", session)

	if !noContext {
		mgr := &dockerctx.Manager{}
		if err := mgr.EnsureNamed(ctx, dockerctx.WslcName,
			"Skrog via a WSL container session (experimental)", dockerHost); err != nil {
			log.Warn("could not wire the docker context", "error", err)
		}
	}

	// Not plain RewriteBinds here, and that is not an oversight.
	//
	// That rewrite maps a Windows bind source to /mnt/<drive>, which is correct
	// for a distro that auto-mounts drives and wrong for a wslc session, where
	// each Windows folder is its own virtiofs share at /mnt/{GUID} and there is
	// no /mnt/c at all. It would hand dockerd a path that does not exist. The
	// share table below does the backend's own translation instead (#321);
	// guest-absolute sources (/var/run/docker.sock, /tmp) still pass through
	// untouched, which is what makes docker-in-docker and Ryuk viable here.
	//
	// The administrator's WSL container policy is enforced here, standing in
	// for the checks in wslcsession that a direct docker.sock relay bypasses
	// (#322). Skrog reads WSL's own configuration, so a deployed allowlist
	// means the same thing through this pipe as through `wslc`.
	policies, err := wslc.ReadPolicies()
	if err != nil {
		// Fail closed. A policy that cannot be read is not the same as no
		// policy, and guessing in the permissive direction is how a bypass
		// ships.
		fmt.Fprintf(os.Stderr, "skrog: cannot read the WSL container policy: %v\n", err)
		return exitError
	}
	// Plugin hooks cannot be stood in for the way the policy can (#406).
	if err := refuseIfPluginsPresent(optsWithResolvedStateDir(opts).StateDir, log); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if policies.Restrictive() {
		log.Info("enforcing the deployed WSL container policy",
			"registry-allowlist", policies.RegistryAllowlist,
			"privileged-allowed", policies.PrivilegedAllowed)
	}

	// Skrog's own policy.yaml and the audit log, wired exactly as the distro
	// backend wires them. The audit log is worth having here for its own sake:
	// it records every container-affecting call through the pipe, which is a
	// record WSLC itself does not keep (#322).
	sd := optsWithResolvedStateDir(opts).StateDir
	settings := config.NewWatcher(sd)
	ownPolicy := policy.NewWatcher(sd)
	ownPolicy.OnError = func(err error) {
		log.Error("policy file is not valid", "error", err, "path", policy.Path(sd))
	}
	auditor := &audit.Switch{
		Enabled: func() bool { return settings.Config().Audit },
		Open: func() (io.WriteCloser, error) {
			return logging.NewRotatingWriter(filepath.Join(sd, "audit.log"), 0, 0)
		},
	}
	defer auditor.Close()

	// Bind sources are resolved through the share table: a pipe means this
	// engine's socket (#164), a Windows drive resolves to the virtiofs share
	// that exposes it (#321), and a guest path passes through untouched.
	shares := &wslc.ShareTable{
		Local:      wslc.New(),
		Session:    session,
		EngineDial: dialer.Dial,
		Logger:     log,
	}
	// Bounded, and NOT ctx -- ctx is already cancelled by the time this runs.
	// Unbounded, this could cold-boot a session VM that had idle-terminated,
	// purely to delete holder containers that went with it, making Ctrl-C
	// appear to hang and leaving a freshly booted VM behind.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), shareCleanupTimeout)
		defer cancel()
		shares.Close(cleanup)
	}()

	// Re-read on every judged request, so a policy deployed or tightened while
	// the bridge is up is honoured without a restart (#354). Seeded with the
	// startup read above, which is the one that fails closed.
	policyWatcher := wslc.NewPolicyWatcher(policies)
	policyWatcher.Logger = log

	gate := combinedGate{wsl: &wslc.PolicyGate{Source: policyWatcher.Policies}, skrog: ownPolicy}
	srv := &pipeproxy.Server{
		Dialer:  dialer,
		Logger:  log,
		Handler: pipeproxy.RewriteBindsFor(shares.Translator(ctx), auditor, gate),
	}

	fmt.Fprintf(os.Stderr, `
Bridge is up against the wslc session %q (experimental).

  docker --context %s ps
  $env:DOCKER_HOST = "%s"; docker ps

This serves its own pipe and its own docker context, so it runs alongside a
normal Skrog install rather than taking it over: %q stays on the distro
engine. Pass --pipe to serve somewhere else.

Policy and audit are enforced here: the machine's deployed WSL container
policy, Skrog's own policy.yaml, and the audit log.

Windows-path bind mounts work by sharing the drive into the session, which
leaves a %s* holder container running for as long as the bridge does.

Not yet supported: UDP published ports.

Ctrl-C to stop.

`, session, dockerctx.WslcName, dockerHost, dockerctx.Name, wslc.HolderPrefix)

	if err := srv.Serve(ctx, listener); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	log.Info("bridge stopped")
	return exitOK
}

// selectWslcPipe picks the pipe for this backend.
//
// Deliberately NOT pipeproxy.SelectPipeName: that one competes for
// \.\pipe\docker_engine, which is right for the engine this machine
// installed and wrong for a second backend running beside it (#335). Taking
// the default pipe would mean whichever bridge started last owned plain
// `docker`, silently changing which engine a user's commands reached.
//
// So this backend serves its own pipe and its own docker context, and a user
// chooses with `docker context use`. An explicit --pipe still wins: someone
// who wants the wslc session on the default pipe can say so, and on a machine
// with no distro install that is a reasonable thing to want.
func selectWslcPipe(preferred string) (name, reason string) {
	if preferred == "" {
		return pipeproxy.WslcPipeName, "the wslc backend's own pipe, so it coexists with the distro backend"
	}
	return pipeproxy.SelectPipeName(preferred)
}

// shareCleanupTimeout bounds removing the share holders on shutdown. Long
// enough for a live session to answer, short enough that a terminated one does
// not make Ctrl-C look wedged -- and the holders are gone with the VM anyway.
const shareCleanupTimeout = 5 * time.Second
