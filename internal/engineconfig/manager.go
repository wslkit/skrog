package engineconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DefaultDaemonPath is where dockerd reads its config inside the distro.
const DefaultDaemonPath = "/etc/docker/daemon.json"

// Runner is the slice of wsl.WSL the manager needs: run a command in the distro
// and get its combined output. Kept minimal so tests fake one method.
type Runner interface {
	Exec(ctx context.Context, distro, user string, args ...string) (string, error)
}

// Manager reads and writes the engine's daemon.json in a distro, validating
// every change with `dockerd --validate` before it replaces the live file, and
// bouncing the engine (with rollback) so the change takes effect.
type Manager struct {
	// WSL runs commands in the distro.
	WSL Runner
	// Distro is the engine distro name.
	Distro string
	// DaemonPath overrides DefaultDaemonPath (tests).
	DaemonPath string
	// EngineRunning reports whether the engine currently answers; Set only
	// restarts an engine that is already up (a stopped engine picks up the new
	// config on its next start). Optional: nil means "assume not running".
	EngineRunning func(ctx context.Context) bool
	// Restart bounces the engine so dockerd re-reads daemon.json, returning nil
	// only when it comes back healthy. Optional: nil skips the restart (the
	// change still lands on disk). Injected so this package needn't import the
	// supervisor or provisioner.
	Restart func(ctx context.Context) error
}

func (m *Manager) daemonPath() string {
	if m.DaemonPath != "" {
		return m.DaemonPath
	}
	return DefaultDaemonPath
}

// Read returns the current daemon.json as a decoded map. A missing or empty
// file is an empty config, not an error.
func (m *Manager) Read(ctx context.Context) (map[string]any, error) {
	out, err := m.WSL.Exec(ctx, m.Distro, "root", "sh", "-c",
		"cat "+m.daemonPath()+" 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.daemonPath(), err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return map[string]any{}, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("existing %s is not valid JSON: %w", m.daemonPath(), err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// Get returns the CLI-string form of one engine key's current value, or "" when
// unset.
func (m *Manager) Get(ctx context.Context, name string) (string, error) {
	if _, ok := keyByName(name); !ok {
		return "", unknownKeyErr(name)
	}
	cfg, err := m.Read(ctx)
	if err != nil {
		return "", err
	}
	return renderValue(cfg[name]), nil
}

// List returns every allowlisted engine key's current value (empty when unset),
// keyed by the full "engine.<name>" form for a unified config listing.
func (m *Manager) List(ctx context.Context) (map[string]string, error) {
	cfg, err := m.Read(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range Keys() {
		out[Prefix+name] = renderValue(cfg[name])
	}
	return out, nil
}

// SetResult reports what Set did, so the caller can phrase the outcome.
type SetResult struct {
	// Applied is the normalized value now in daemon.json ("" when the key was cleared).
	Applied string
	// Restarted is true when the engine was bounced to pick up the change.
	Restarted bool
	// PendingRestart is true when the change is on disk but the engine was not
	// running, so it takes effect on the next start.
	PendingRestart bool
}

// Set validates and applies one engine key. The sequence is deliberate:
// validate the candidate with dockerd before touching the live file, back the
// live file up, replace it atomically, then bounce the engine — and if it does
// not come back, restore the backup and bounce again so a bad-but-valid config
// never leaves the engine down.
func (m *Manager) Set(ctx context.Context, name, raw string) (SetResult, error) {
	k, ok := keyByName(name)
	if !ok {
		return SetResult{}, unknownKeyErr(name)
	}
	value, clear, err := parseValue(k, raw)
	if err != nil {
		return SetResult{}, fmt.Errorf("engine.%s: %w", name, err)
	}

	cfg, err := m.Read(ctx)
	if err != nil {
		return SetResult{}, err
	}
	if clear {
		delete(cfg, name)
	} else {
		cfg[name] = value
	}
	keepLoopbackInsecure(cfg)

	res, err := m.commit(ctx, cfg)
	if err != nil {
		return SetResult{}, err
	}
	res.Applied = renderValue(cfg[name])
	return res, nil
}

// SetMany applies several engine keys in one shot: one validate, one write, one
// engine bounce — so a declarative install with a handful of daemon.json keys
// does not restart the engine once per key. Parsing/validation of every value
// happens before anything is written, so a single bad value rejects the whole
// batch. An empty value clears its key.
func (m *Manager) SetMany(ctx context.Context, kv map[string]string) (SetResult, error) {
	cfg, err := m.Read(ctx)
	if err != nil {
		return SetResult{}, err
	}
	for name, raw := range kv {
		k, ok := keyByName(name)
		if !ok {
			return SetResult{}, unknownKeyErr(name)
		}
		value, clear, err := parseValue(k, raw)
		if err != nil {
			return SetResult{}, fmt.Errorf("engine.%s: %w", name, err)
		}
		if clear {
			delete(cfg, name)
		} else {
			cfg[name] = value
		}
	}
	keepLoopbackInsecure(cfg)
	return m.commit(ctx, cfg)
}

// ApplyDefaults writes every entry of Defaults whose key is absent from
// daemon.json, validating the result with dockerd like any other change, and
// returns the keys it added. It never restarts the engine: the caller runs it
// right before dockerd launches, which is when a default can land for free.
// Keys the user has set (to any value) are left alone.
func (m *Manager) ApplyDefaults(ctx context.Context) ([]string, error) {
	cfg, err := m.Read(ctx)
	if err != nil {
		return nil, err
	}
	var added []string
	for name, raw := range Defaults {
		if _, set := cfg[name]; set {
			continue
		}
		k, ok := keyByName(name)
		if !ok {
			return nil, unknownKeyErr(name)
		}
		value, clear, err := parseValue(k, raw)
		if err != nil || clear {
			return nil, fmt.Errorf("engine.%s default %q: %v", name, raw, err)
		}
		cfg[name] = value
		added = append(added, name)
	}
	// Not a default, but it lands the same way and for the same reason: an
	// install whose insecure-registries predates engine 29.10.0 must not lose
	// loopback (and with it `skrog cache`) on upgrade. See LoopbackRegistries.
	if keepLoopbackInsecure(cfg) {
		added = append(added, "insecure-registries")
	}
	if len(added) == 0 {
		return nil, nil
	}
	sort.Strings(added)
	candidate, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := m.validate(ctx, candidate); err != nil {
		return nil, err
	}
	if err := m.writeDaemon(ctx, candidate); err != nil {
		return nil, err
	}
	return added, nil
}

// commit validates the candidate config, writes it atomically, and bounces the
// engine with rollback on failure — the shared tail of Set and SetMany. The
// sequence is deliberate: validate before the live file is touched, back it up,
// replace it, then restart; if it does not come back, restore and restart again
// so a valid-but-bad config never leaves the engine down.
func (m *Manager) commit(ctx context.Context, cfg map[string]any) (SetResult, error) {
	candidate, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return SetResult{}, err
	}
	if err := m.validate(ctx, candidate); err != nil {
		return SetResult{}, err
	}
	if err := m.writeDaemon(ctx, candidate); err != nil {
		return SetResult{}, err
	}

	var res SetResult
	running := m.EngineRunning != nil && m.EngineRunning(ctx)
	if !running || m.Restart == nil {
		res.PendingRestart = m.Restart != nil // only "pending" if a restart is possible at all
		return res, nil
	}

	if err := m.Restart(ctx); err != nil {
		// The config validated but the engine did not come back. Roll back to
		// the last-good file and bounce again; report the original failure.
		if rbErr := m.rollback(ctx); rbErr != nil {
			return SetResult{}, fmt.Errorf("engine failed to restart with the new config (%w) "+
				"AND rollback failed (%v); the engine may be down — check `skrog status`", err, rbErr)
		}
		return SetResult{}, fmt.Errorf("engine failed to restart with the new config; "+
			"rolled back to the previous daemon.json: %w", err)
	}
	res.Restarted = true
	return res, nil
}

// validate runs `dockerd --validate` against the candidate written to a temp
// file in the distro, so an invalid config is refused before the live file is
// touched. This is the single most important guard in the package.
func (m *Manager) validate(ctx context.Context, candidate []byte) error {
	tmp := m.daemonPath() + ".candidate"
	if err := m.writeFile(ctx, tmp, candidate); err != nil {
		return err
	}
	out, err := m.WSL.Exec(ctx, m.Distro, "root", "sh", "-c",
		"dockerd --validate --config-file "+tmp+" 2>&1; rc=$?; rm -f "+tmp+"; exit $rc")
	if err != nil {
		return fmt.Errorf("the engine rejected this config:\n%s", strings.TrimSpace(out))
	}
	return nil
}

// writeDaemon backs up the current daemon.json, then replaces it atomically.
func (m *Manager) writeDaemon(ctx context.Context, content []byte) error {
	p := m.daemonPath()
	// Back up only if a file exists, so rollback has a known-good target.
	if _, err := m.WSL.Exec(ctx, m.Distro, "root", "sh", "-c",
		"[ -f "+p+" ] && cp "+p+" "+p+".bak || true"); err != nil {
		return fmt.Errorf("backing up %s: %w", p, err)
	}
	return m.writeFile(ctx, p, content)
}

// rollback restores the backed-up daemon.json and bounces the engine.
func (m *Manager) rollback(ctx context.Context) error {
	p := m.daemonPath()
	if _, err := m.WSL.Exec(ctx, m.Distro, "root", "sh", "-c",
		"[ -f "+p+".bak ] && mv "+p+".bak "+p+" || rm -f "+p); err != nil {
		return err
	}
	if m.Restart != nil {
		return m.Restart(ctx)
	}
	return nil
}

// writeFile writes content to a path in the distro without needing stdin:
// base64 keeps arbitrary JSON out of shell-quoting range entirely.
func (m *Manager) writeFile(ctx context.Context, path string, content []byte) error {
	b64 := base64.StdEncoding.EncodeToString(content)
	// mkdir -p the parent so a first-ever write cannot fail on a missing dir.
	cmd := "mkdir -p \"$(dirname " + path + ")\" && printf %s " + b64 + " | base64 -d > " + path
	if out, err := m.WSL.Exec(ctx, m.Distro, "root", "sh", "-c", cmd); err != nil {
		return fmt.Errorf("writing %s: %w: %s", path, err, strings.TrimSpace(out))
	}
	return nil
}

func unknownKeyErr(name string) error {
	return fmt.Errorf("unknown engine config key %q (known: engine.%s)",
		name, strings.Join(Keys(), ", engine."))
}
