package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/wslc"
)

// wslcEngineAdapter is supervise.Engine for a WSL container session (#335).
//
// The distro adapter starts and stops a distro Skrog imported. Here the engine
// belongs to Microsoft and the unit of lifecycle is the session VM, which is a
// better fit than it sounds: starting one is creating it, stopping one is
// terminating it, and a terminated session costs nothing while a stopped
// distro still holds its VHD open.
//
// Idle-stop therefore means something concrete on this backend -- roughly
// 820 MB of a second VM handed back -- so it is wired the same way as the
// distro's rather than disabled.
type wslcEngineAdapter struct {
	local   *wslc.Local
	agent   []byte
	secret  string
	session string
	log     *slog.Logger

	// lease is released while the engine is deliberately stopped, so it does
	// not re-take the session the supervisor just terminated. Nil is fine.
	lease *wslc.Lease
}

// Running reports whether the session is up AND our agent is in it.
//
// Both halves matter. A session whose VM came back after idle-termination has
// a tmpfs root, so it is running with no agent -- reporting that as healthy
// would leave the supervisor content while every docker call failed.
func (e *wslcEngineAdapter) Running(ctx context.Context) (bool, error) {
	// No session configured is a definite "not running", not a failed probe.
	if e.session == "" {
		return false, nil
	}
	// Both probes distinguish "the answer is no" from "I could not ask"
	// (#437). Folding an error into false told the reconciler the engine was
	// down, and its response to that is to start one.
	ok, err := e.local.HasSession(ctx, e.session)
	if err != nil {
		return false, fmt.Errorf("checking for the wslc session: %w", err)
	}
	if !ok {
		return false, nil
	}
	running, err := e.local.AgentRunning(ctx, e.session)
	if err != nil {
		return false, fmt.Errorf("checking the agent in session %s: %w", e.session, err)
	}
	return running, nil
}

// Start resolves a session -- creating one if nothing is running -- and places
// the agent. Idempotent, which is what the supervisor's restart loop needs.
func (e *wslcEngineAdapter) Start(ctx context.Context) error {
	if e.lease != nil {
		e.lease.Resume()
	}
	session, err := e.local.ResolveSession(ctx)
	if err != nil {
		return err
	}
	e.session = session
	// ErrNoPortForwarding is a caveat about published ports, not a failed
	// start: the engine is reachable either way, and failing here would put
	// the supervisor into a restart loop over a working bridge.
	if err := e.local.Bootstrap(ctx, session, e.agent, e.secret); err != nil {
		if !isNoPortForwarding(err) {
			return err
		}
		e.log.Warn("published ports will not reach Windows on this agent")
	}
	return nil
}

// Stop terminates the session VM, and only ever the session Skrog resolved.
//
// The same rule the distro adapter follows for distros (#35): Skrog shares the
// machine. Terminating every session would take down whatever the user is
// running with `wslc` by hand.
func (e *wslcEngineAdapter) Stop(ctx context.Context) error {
	// Release the lease FIRST. Otherwise the terminate below ends the held
	// process, the lease sees an error, and one second later it takes the
	// session again -- undoing the stop.
	if e.lease != nil {
		e.lease.Pause()
	}
	if e.session == "" {
		return nil
	}
	return e.local.Terminate(ctx, e.session)
}

func isNoPortForwarding(err error) bool {
	return errors.Is(err, wslc.ErrNoPortForwarding)
}

// wslcSupervised is everything the supervisor needs to serve a wslc session:
// the transport, the engine lifecycle, the bind translation, and the port
// watcher that carries published ports to Windows (#335).
type wslcSupervised struct {
	Dialer    pipeproxy.Dialer
	Engine    supervise.Engine
	Translate pipeproxy.SourceTranslator
	// Policy is re-read on every judged request, so a policy deployed or
	// tightened while this long-lived supervisor runs is honoured without a
	// restart (#354).
	Policy  *wslc.PolicyWatcher
	Session string
	// Busy vetoes an idle stop while real containers are running. Non-nil, or
	// the supervisor vetoes EVERY idle stop (see supervise.maybeIdleStop).
	Busy      func(ctx context.Context) (bool, error)
	stopWatch context.CancelFunc
	watcher   *wslc.PortWatcher
	shares    *wslc.ShareTable
}

// Close releases what the stack holds: the port listeners and the share holder
// containers. Called on shutdown, so a supervisor that exits cleanly leaves no
// skrog-share-* containers behind.
func (s *wslcSupervised) Close() {
	if s.stopWatch != nil {
		s.stopWatch()
	}
	if s.watcher != nil {
		s.watcher.StopAll()
	}
	if s.shares != nil {
		// Bounded: a session that has idle-terminated must not be cold-booted
		// just to remove holder containers that went down with it.
		cleanup, cancel := context.WithTimeout(context.Background(), shareCleanupTimeout)
		defer cancel()
		s.shares.Close(cleanup)
	}
}

// startWslcStack brings the backend up for the supervisor.
//
// It reuses wslcBackend, which `skrog proxy --engine wslc` already uses, so the
// two entry points cannot drift into behaving differently -- the supervised
// path was the one place that would have been tempting to reimplement.
func startWslcStack(ctx context.Context, agentPath, stateDir string, log *slog.Logger) (*wslcSupervised, error) {
	dialer, watcher, session, err := wslcBackend(ctx, agentPath, stateDir, log)
	if err != nil {
		return nil, err
	}

	// The FIRST read fails closed, exactly as the proxy path does: a deployed
	// allowlist that cannot be read is not the same as no allowlist (#322).
	// Here, where nothing is serving yet, refusing to start is the right
	// answer.
	policies, err := wslc.ReadPolicies()
	if err != nil {
		return nil, fmt.Errorf("cannot read the WSL container policy: %w", err)
	}
	// Plugin hooks cannot be stood in for the way the policy can (#406), so a
	// machine with plugins registered gets a refusal rather than a backend
	// that quietly does not call them.
	if err := refuseIfPluginsPresent(stateDir, log); err != nil {
		return nil, err
	}
	if policies.Restrictive() {
		log.Info("enforcing the deployed WSL container policy",
			"registry-allowlist", policies.RegistryAllowlist,
			"privileged-allowed", policies.PrivilegedAllowed)
	}

	// From then on it is re-read per judged request. This supervisor starts at
	// logon and runs for months, so a snapshot would keep enforcing whatever
	// was deployed on the day it started (#354).
	policyWatcher := wslc.NewPolicyWatcher(policies)
	policyWatcher.Logger = log

	l := wslc.New()
	agent, err := loadGuestAgent(ctx, agentPath)
	if err != nil {
		return nil, err
	}
	secret, err := wslcSecret(stateDir)
	if err != nil {
		return nil, err
	}

	shares := &wslc.ShareTable{
		Local:      l,
		Session:    session,
		EngineDial: dialer.Dial,
		Logger:     log,
	}

	var lease *wslc.Lease
	if watcher != nil {
		lease = watcher.Lease
	}

	s := &wslcSupervised{
		Dialer:    dialer,
		Translate: shares.Translator(ctx),
		Policy:    policyWatcher,
		Session:   session,
		Busy:      wslcBusy(runningContainerNames(dialer)),
		watcher:   watcher,
		shares:    shares,
		Engine: &wslcEngineAdapter{
			local:   l,
			agent:   agent,
			secret:  secret,
			session: session,
			log:     log,
			lease:   lease,
		},
	}

	if watcher != nil {
		wctx, cancel := context.WithCancel(ctx)
		s.stopWatch = cancel
		go watcher.Run(wctx)
	}
	return s, nil
}

// Handler is the pipe handler for this backend: the share table's bind
// translation, the audit log, and the machine's deployed WSL policy stacked in
// front of Skrog's own policy.yaml.
//
// Built here rather than in supervise.go so the wslc types stay in one file and
// the shared path keeps exactly the handler it always had.
func (s *wslcSupervised) Handler(auditor pipeproxy.AuditSink, own pipeproxy.Gate) func(net.Conn, io.ReadWriteCloser) error {
	return pipeproxy.RewriteBindsFor(s.Translate, auditor,
		combinedGate{wsl: &wslc.PolicyGate{Source: s.Policy.Policies}, skrog: own})
}

// wslcStatus reports the engine state and session name for `skrog status` on a
// wslc install, without starting anything.
//
// "running" requires both a live session AND our agent in it: a session whose
// VM came back after idle-termination has a tmpfs root and no agent, and
// calling that running would tell a readiness probe the engine is up while
// every docker call fails. Listing sessions never boots one, so this stays
// safe to poll (#82).
func wslcStatus(ctx context.Context) (state, session string) {
	l := wslc.New()
	sessions, err := l.Sessions(ctx)
	if err != nil || len(sessions) == 0 {
		return "stopped", ""
	}
	name := wslc.DefaultSessionName()
	found := ""
	for _, s := range sessions {
		if s.DisplayName == wslc.SessionName || s.DisplayName == name {
			found = s.DisplayName
			break
		}
	}
	if found == "" {
		found = sessions[0].DisplayName
	}
	if running, err := l.AgentRunning(ctx, found); err == nil && running {
		return "running", found
	}
	return "stopped", found
}

// requireDistroInstall resolves the engine distro for a command that only means
// something on the distro backend, and explains itself when there is not one.
//
// It exists because those commands all reported "no install found. Run `skrog
// install` first." on a wslc install (#335) -- which is both wrong and the
// least useful thing to say, since there IS an install and running `skrog
// install` again would not change anything. The distinction matters: "you have
// nothing installed" and "this command does not apply to what you installed"
// need different actions from the user.
//
// cmd is the command as a user types it ("compact"), and why says what the
// command operates on, so the message names the actual reason rather than a
// generic refusal.
func requireDistroInstall(p *provision.Provisioner, opts provision.Options, cmd, why string) (string, bool) {
	distro, msg := resolveDistroFor(p, opts, cmd, why)
	if msg != "" {
		fmt.Fprint(os.Stderr, msg)
		return "", false
	}
	return distro, true
}

// resolveDistroFor is the decision, kept separate from printing it so the
// wording is testable without redirecting os.Stderr. An empty msg means the
// distro is usable; otherwise msg is the whole thing to write, newline
// included.
func resolveDistroFor(p *provision.Provisioner, opts provision.Options, cmd, why string) (distro, msg string) {
	if m, err := p.ReadManifest(opts); err == nil && m.IsWslc() {
		return "", fmt.Sprintf(
			"skrog: `skrog %s` does not apply on this machine.\n\n"+
				"  This install's engine is a WSL container session (backend wslc), and\n"+
				"  %s\n\n"+
				"  See docs/wslc-backend.md for what this backend does and does not do.\n",
			cmd, why)
	}
	if d, ok := resolveDistro(p, opts); ok {
		return d, ""
	}
	return "", "skrog: no install found. Run `skrog install` first.\n"
}

// engineUp answers "is the engine answering" for whichever backend this
// install uses.
//
// `skrog start`, `stop` and `restart` each polled p.EngineRunning directly,
// which asks a DISTRO. On a wslc install that is the wrong question, and the
// three commands failed outright at the resolve step before ever reaching it
// (#335) -- while the documentation told people to run them.
func engineUp(ctx context.Context, target engineTarget, p *provision.Provisioner, opts provision.Options) bool {
	if target.isWslc() {
		state, _ := wslcStatus(ctx)
		return state == "running"
	}
	return p.EngineRunning(ctx, opts)
}

// stopEngineDirectly stops the engine with no supervisor to do it.
func stopEngineDirectly(ctx context.Context, target engineTarget, p *provision.Provisioner, opts provision.Options, log *slog.Logger) error {
	if !target.isWslc() {
		return p.StopEngine(ctx, opts)
	}
	// Only ever the session Skrog resolved: terminating every session would
	// take down whatever the user is running with `wslc` by hand (#35).
	l := wslc.New()
	session, err := l.ResolveSession(ctx)
	if err != nil {
		return err
	}
	return l.Terminate(ctx, session)
}

// wslcBusy is the idle-stop veto for this backend: are any containers running
// that are not Skrog's own bookkeeping?
//
// The generic probe would be wrong here. Windows-path bind mounts leave one
// `skrog-share-<drive>` holder running for as long as the bridge does (#321),
// and those are running containers -- so the moment anyone bound a Windows
// folder, an unfiltered probe would report busy forever and the session would
// never be reclaimed.
//
// Skrog's own holders are not work: terminating the session takes them with it,
// and the share table re-establishes them on the next bind.
func wslcBusy(names func(ctx context.Context) ([]string, error)) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		running, err := names(ctx)
		if err != nil {
			// An error is a veto, exactly as the distro probe treats it:
			// stopping the engine kills whatever runs in it, so idling
			// requires a definite "nothing is running".
			return false, err
		}
		for _, n := range running {
			if !strings.HasPrefix(strings.TrimPrefix(n, "/"), wslc.HolderPrefix) {
				return true, nil
			}
		}
		return false, nil
	}
}

// runningContainerNames lists the names of running containers over the engine
// dialer, the same transport the pipe uses.
func runningContainerNames(dialer pipeproxy.Dialer) func(ctx context.Context) ([]string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				rwc, err := dialer.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return rwcConn{rwc}, nil
			},
			// One probe, one connection: the engine socket is not a place to
			// pool idle keep-alives that would themselves look like activity.
			DisableKeepAlives: true,
		},
	}
	return func(ctx context.Context) ([]string, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://engine/"+wslc.APIVersion+"/containers/json", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("engine returned %s to the container probe", resp.Status)
		}
		var containers []struct {
			Names []string `json:"Names"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
			return nil, err
		}
		var out []string
		for _, c := range containers {
			out = append(out, c.Names...)
		}
		return out, nil
	}
}
