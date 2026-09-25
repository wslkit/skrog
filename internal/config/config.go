// Package config is Skrog's user-tunable settings: a small JSON file in the
// state directory, edited through `skrog config` rather than by hand so
// every value is validated on the way in.
//
// Settings are read fresh at each use (the supervisor reads per tick), so a
// `skrog config set` takes effect without restarting anything.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/emulation"
	"github.com/wslkit/skrog/internal/gpu"
	"github.com/wslkit/skrog/internal/wslconfig"
)

// KeyIdleTimeout is how long the bridge must be quiet (no open connections,
// no running containers) before the supervisor stops the engine to return
// its RAM. DefaultIdleTimeout when unset; "off" disables idle stops entirely.
const KeyIdleTimeout = "idle-timeout"

// Lifecycle hook keys (#70): each holds a path to an executable the supervisor
// runs on the named engine event, time-bounded and best-effort — a failing or
// slow hook is logged but never blocks the lifecycle. Empty (the default) means
// no hook. HookKeys enumerates them; the supervisor reads the path for an event
// as "hook." + event.
const (
	KeyHookPostStart  = "hook.post-start"   // engine started (recovery or first start)
	KeyHookPreStop    = "hook.pre-stop"     // engine about to stop on `skrog stop`
	KeyHookOnIdleStop = "hook.on-idle-stop" // engine stopped by the idle timeout
	KeyHookOnWake     = "hook.on-wake"      // engine cold-started on demand
)

// HookKeys lists the hook config keys, in lifecycle order.
func HookKeys() []string {
	return []string{KeyHookPostStart, KeyHookPreStop, KeyHookOnIdleStop, KeyHookOnWake}
}

// KeyAudit toggles the container-affecting API audit log (#121). "on" writes a
// JSON-lines record of pulls, container create/start/stop/remove, exec and
// builds to audit.log in the state dir; "off" (the default) disables it.
// Changing it takes effect on the next docker call: the supervisor follows
// the setting live, and opens or closes the log as it flips (#202).
const KeyAudit = "audit"

// Corporate-network keys (#62), applied to the engine on its next start --
// `skrog restart` is the way to ask for one. The supervisor re-reads them at
// every engine start rather than capturing them at launch, which is what
// makes that true (#202). NetworkKeys enumerates them.
const (
	// KeyProxy is the HTTP(S) proxy URL dockerd uses for registry pulls.
	KeyProxy = "network.proxy"
	// KeyNoProxy is the comma-separated proxy bypass list.
	KeyNoProxy = "network.no-proxy"
	// KeyImportHostCAs, when on, trusts the host's root CA store inside the
	// engine — the fix for a TLS-inspecting corporate proxy.
	KeyImportHostCAs = "network.import-host-cas"
	// KeyPublishScope decides how far a published container port reaches (#508):
	// "loopback" (the default) leaves WSL's own behaviour alone, "lan" has the
	// supervisor relay published ports to every interface.
	//
	// Opt-in because "lan" puts dev containers on the network. That is a change
	// to the machine's exposure, and the same bar emulation.platforms is held
	// to: a setting that reaches past the thing you were configuring is a
	// decision, not a default to discover afterwards.
	KeyPublishScope = "network.publish-scope"
)

// NetworkKeys lists the corporate-network keys.
func NetworkKeys() []string {
	return []string{KeyProxy, KeyNoProxy, KeyImportHostCAs, KeyPublishScope}
}

// KeyGPU, when on, installs the NVIDIA CDI spec in the engine on every start so
// containers can use the GPU (#83). "off" (the default) removes it. Set through
// `skrog enable-gpu` rather than by hand, since that also verifies the GPU is
// visible and restarts the engine.
const KeyGPU = "gpu"

// KeyGPUVendor selects which vendor's CDI spec `gpu` installs (#185). Separate
// from KeyGPU rather than folded into it ("gpu = nvidia|amd|off") so every
// install that already says `gpu on` keeps working and keeps meaning NVIDIA.
// Empty is nvidia. "amd" is experimental -- see `skrog enable-gpu --help`.
const KeyGPUVendor = "gpu.vendor"

// KeyVerifySignature, when on, checks the rootfs Sigstore signature before it
// is imported, in addition to the always-enforced SHA-256 pin (#147). Opt-in:
// it needs cosign on PATH, and an air-gapped install has no transparency log
// to reach.
const KeyVerifySignature = "install.verify-signature"

// WSL VM sizing (#148). These live in the GLOBAL ~/.wslconfig, which every
// WSL2 distro on the machine shares -- so setting one here records an
// intention, and `skrog wsl-config apply` is what writes it, after showing
// the diff. Nothing propagates on its own.
const (
	KeyWSLMemory            = "wsl.memory"
	KeyWSLProcessors        = "wsl.processors"
	KeyWSLSwap              = "wsl.swap"
	KeyWSLAutoMemoryReclaim = "wsl.auto-memory-reclaim"
	// KeyWSLVirtiofs mounts Windows drives over virtiofs instead of 9p in every
	// WSL2 distro (#327). Needs WSL 2.9+; "true" or "false".
	KeyWSLVirtiofs = "wsl.virtiofs"
)

// WSLKeys lists the sizing keys, and maps each to its .wslconfig name.
var WSLKeys = map[string]string{
	KeyWSLMemory:            "memory",
	KeyWSLProcessors:        "processors",
	KeyWSLSwap:              "swap",
	KeyWSLAutoMemoryReclaim: "autoMemoryReclaim",
	KeyWSLVirtiofs:          "virtiofs",
}

// KeyDiskWarnBelow is the free-space floor on the engine data volume under
// which `skrog doctor` warns (#145) — a size such as 10GB or 8GiB. Empty means
// the built-in 5 GiB. Runners with small disks raise it so a full volume is
// flagged before pulls start failing.
const KeyDiskWarnBelow = "disk.warn-below"

// KeyEmulationPlatforms lets the engine run containers built for another CPU
// architecture (#462): a comma-separated list such as "linux/amd64", or empty
// (the default) for none.
//
// Off by default, and the default is the substance of it. Registering a QEMU
// interpreter is not a distro-local act: binfmt_misc belongs to the KERNEL,
// and on WSL2 one kernel is shared by every distro in the utility VM. Handlers
// Skrog registers change how the user's Ubuntu executes foreign binaries too,
// and replace any that tonistiigi/binfmt or a distro's qemu-user-static put
// there. docs/docker-cli.md reasoned from exactly that to "Skrog does not do
// this unasked", on the same consent grounds as ~/.wslconfig; this key is the
// asking.
//
// It exists for `docker run --platform`. Cross-architecture BUILDS already
// work with nothing installed -- a docker-container buildx builder bundles
// its own emulators -- so anyone setting this to fix a build is solving the
// wrong problem, and docs/docker-cli.md says which command to use instead.
const KeyEmulationPlatforms = "emulation.platforms"

// KeyPruneEvery is how often the supervisor reclaims disk on its own (#393):
// a duration like 168h, or "off" (the default). Automatic deletion is opt-in
// and stays that way — a tool that removes a user's images because a timer
// fired, without being asked, has earned every bit of the distrust that
// follows.
const KeyPruneEvery = "prune.every"

// KeyPruneKeepSince is the age guard on an automatic prune: nothing younger
// than this is touched. It maps to `prune --until`.
//
// There is no "no guard" setting on purpose. An unguarded automatic prune
// would delete the image someone pulled an hour ago for tomorrow's demo, and
// the only signal it left would be a slow pull later. The default is a week.
const KeyPruneKeepSince = "prune.keep-since"

// KeyPruneBuildCache widens an automatic prune to the BuildKit cache. Off by
// default: the cache is expensive to rebuild and cheap to keep, so dropping it
// is a choice rather than housekeeping.
//
// There is deliberately NO equivalent for volumes. `skrog prune --volumes`
// exists for a human who typed it; a timer must never be able to delete a
// database because nothing referenced it this week.
const KeyPruneBuildCache = "prune.build-cache"

// KeyAutostart records whether the user wants the supervisor to start at
// logon (#515): "on" or "off", and unset on an install from before it existed.
//
// It records INTENT; it is not the mechanism. What starts the supervisor is
// the per-user Run entry (internal/autostart), and `skrog config set autostart`
// writes or removes that entry first and records the choice only once it has.
// A stored flag that merely claimed autostart was on would be the same drift
// #515 was: a machine that said one thing and did another after a reboot.
//
// What the record buys is the difference between "turned off on purpose" and
// "went missing". Without it, a missing Run entry looks the same either way,
// so doctor could only guess, and could not safely repair it.
const KeyAutostart = "autostart"

// path is the settings file inside the state dir.
func path(stateDir string) string {
	return filepath.Join(stateDir, "config.json")
}

// Config is the parsed, validated view.
type Config struct {
	// IdleTimeout of zero means idle stops are off.
	IdleTimeout time.Duration
	// Audit enables the container-affecting API audit log.
	Audit bool
	// Proxy / NoProxy configure dockerd's registry proxy; ImportHostCAs trusts
	// the host root CA store inside the engine.
	Proxy         string
	NoProxy       string
	ImportHostCAs bool
	// PublishScope is "loopback" (default) or "lan"; see KeyPublishScope.
	PublishScope string
	// GPU installs the CDI spec so containers can use the GPU (#83).
	GPU bool
	// GPUVendor is which vendor's spec that is (#185). Empty means nvidia.
	GPUVendor string
	// VerifySignature checks the rootfs signature before import (#147).
	VerifySignature bool
	// DiskWarnBelow is the doctor free-space floor in bytes; 0 means default.
	DiskWarnBelow uint64
	// EmulationPlatforms are the foreign architectures the engine may run
	// containers for (#462), comma-separated GOARCH values. Empty means none,
	// which is the default.
	EmulationPlatforms string
	// PruneEvery of zero means the supervisor never prunes on its own (#393).
	PruneEvery time.Duration
	// PruneKeepSince is the age guard on an automatic prune; zero means the
	// built-in default rather than "no guard", which must not be expressible.
	PruneKeepSince time.Duration
	// PruneBuildCache widens an automatic prune to the BuildKit cache.
	PruneBuildCache bool
}

// DefaultPruneKeepSince is the age guard used when prune.every is set and
// prune.keep-since is not. A week: long enough that a Friday pull survives to
// Monday, short enough to be worth running.
const DefaultPruneKeepSince = 168 * time.Hour

// KeepSince is the age guard actually in force, defaulting rather than
// returning zero — callers must never be able to launch an unguarded prune by
// forgetting to check.
func (c Config) KeepSince() time.Duration {
	if c.PruneKeepSince <= 0 {
		return DefaultPruneKeepSince
	}
	return c.PruneKeepSince
}

// DefaultIdleTimeout is the idle-timeout an install gets without setting one:
// five minutes, the same as Docker Desktop's Resource Saver, which stops its
// engine after five minutes with no containers running -- measured on the
// maintainer's machine, 2026-09-25, from Desktop's own log (last request
// 00:38:23, "idle: shutdown" 00:43:24).
//
// It used to be off. An engine nobody is using then holds its RAM forever,
// which is the opposite of what someone moving from Desktop expects. The cost
// of it being on is one cold start (4-8 s measured) on the first docker
// command after an idle stop; nothing running is ever stopped, because an
// idle stop needs no running containers and no open connections.
const DefaultIdleTimeout = 5 * time.Minute

// Defaults is the configuration with nothing set: what Load starts from, and
// what the watcher uses when there is no settings file at all. One place, so
// the two cannot disagree about what "unset" means.
func Defaults() Config {
	return Config{
		IdleTimeout:  DefaultIdleTimeout,
		PublishScope: PublishScopeLoopback,
	}
}

// Load parses the settings file. A missing file is the default configuration,
// not an error; a corrupt one is an error, because silently reverting a
// user's settings to defaults is worse than telling them.
func Load(stateDir string) (Config, error) {
	raw, err := load(stateDir)
	if err != nil {
		return Config{}, err
	}
	c := Defaults()
	if v, ok := raw[KeyIdleTimeout]; ok {
		d, err := parseIdleTimeout(v)
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", KeyIdleTimeout, err)
		}
		c.IdleTimeout = d
	}
	c.Audit = raw[KeyAudit] == "on"
	c.Proxy = raw[KeyProxy]
	c.NoProxy = raw[KeyNoProxy]
	c.ImportHostCAs = raw[KeyImportHostCAs] == "on"
	c.PublishScope = raw[KeyPublishScope]
	if c.PublishScope == "" {
		c.PublishScope = PublishScopeLoopback
	}
	c.GPU = raw[KeyGPU] == "on"
	c.GPUVendor = raw[KeyGPUVendor]
	c.VerifySignature = raw[KeyVerifySignature] == "on"
	if v := strings.TrimSpace(raw[KeyDiskWarnBelow]); v != "" {
		n, err := parseSize(v)
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", KeyDiskWarnBelow, err)
		}
		c.DiskWarnBelow = n
	}
	if v, ok := raw[KeyPruneEvery]; ok {
		d, err := parseIdleTimeout(v)
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", KeyPruneEvery, err)
		}
		c.PruneEvery = d
	}
	if v, ok := raw[KeyPruneKeepSince]; ok {
		d, err := parseIdleTimeout(v)
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", KeyPruneKeepSince, err)
		}
		c.PruneKeepSince = d
	}
	c.EmulationPlatforms = raw[KeyEmulationPlatforms]
	c.PruneBuildCache = raw[KeyPruneBuildCache] == "on"
	return c, nil
}

func load(stateDir string) (map[string]string, error) {
	b, err := os.ReadFile(path(stateDir))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path(stateDir), err)
	}
	return m, nil
}

// validators parse-and-normalize each known key; unknown keys are refused so
// a typo ("idle-timout") fails loudly instead of configuring nothing.
var validators = map[string]func(string) (string, error){
	KeyIdleTimeout: func(v string) (string, error) {
		d, err := parseIdleTimeout(v)
		if err != nil {
			return "", err
		}
		if d == 0 {
			return "off", nil
		}
		return d.String(), nil
	},
	KeyHookPostStart:      validateHookPath,
	KeyHookPreStop:        validateHookPath,
	KeyHookOnIdleStop:     validateHookPath,
	KeyHookOnWake:         validateHookPath,
	KeyAudit:              validateOnOff,
	KeyProxy:              validateProxy,
	KeyNoProxy:            func(v string) (string, error) { return strings.TrimSpace(v), nil },
	KeyImportHostCAs:      validateOnOff,
	KeyPublishScope:       validatePublishScope,
	KeyGPU:                validateOnOff,
	KeyGPUVendor:          validateGPUVendor,
	KeyDiskWarnBelow:      validateSize,
	KeyEmulationPlatforms: validateEmulationPlatforms,
	KeyPruneEvery:         validateDurationOrOff,
	KeyPruneKeepSince:     validatePruneKeepSince,
	KeyPruneBuildCache:    validateOnOff,
	KeyVerifySignature:    validateOnOff,
	KeyAutostart:          validateOnOff,

	// Validated the way WSL reads them, so a typo fails here rather than
	// silently sizing the VM as something else (#148).
	KeyWSLMemory:            func(v string) (string, error) { return wslconfig.Validate(wslconfig.KeyMemory, v) },
	KeyWSLProcessors:        func(v string) (string, error) { return wslconfig.Validate(wslconfig.KeyProcessors, v) },
	KeyWSLSwap:              func(v string) (string, error) { return wslconfig.Validate(wslconfig.KeySwap, v) },
	KeyWSLAutoMemoryReclaim: func(v string) (string, error) { return wslconfig.Validate(wslconfig.KeyAutoMemoryReclaim, v) },
	KeyWSLVirtiofs:          func(v string) (string, error) { return wslconfig.Validate(wslconfig.KeyVirtiofs, v) },
}

// validateProxy accepts an http(s) URL, or empty to clear.
func validateProxy(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", fmt.Errorf("%q must be an http:// or https:// URL", v)
	}
	return v, nil
}

// validateEmulationPlatforms accepts the platforms the engine may emulate
// (#462), normalised to the GOARCH spelling the rest of the code uses.
//
// Rejecting here rather than at engine start is the point. A typo in this key
// would otherwise surface as "my arm64 containers still do not run", hours
// later, with the supervisor log as the only clue -- and the failure mode of
// emulation is silence, not an error, because an unregistered handler simply
// never matches.
func validateEmulationPlatforms(v string) (string, error) {
	handlers, err := emulation.Parse(v)
	if err != nil {
		return "", err
	}
	archs := make([]string, 0, len(handlers))
	for _, h := range handlers {
		archs = append(archs, h.Arch)
	}
	return strings.Join(archs, ","), nil
}

// OnOff parses a boolean-ish setting the way every on/off key does, for a
// caller that has to act on the value before it is stored (#515: `config set
// autostart` changes the Run entry first).
func OnOff(v string) (bool, error) {
	n, err := validateOnOff(v)
	return n == "on", err
}

// validateOnOff normalizes a boolean-ish setting to "on" or "off".
func validateOnOff(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes":
		return "on", nil
	case "off", "false", "0", "no", "":
		return "off", nil
	}
	return "", fmt.Errorf("%q is not on or off", v)
}

// validateHookPath accepts a path to an existing file, or empty/"off" to clear
// the hook. Existence is checked at set time so a typo fails loudly here rather
// than silently doing nothing when the event fires.
func validateHookPath(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "off") {
		return "", nil
	}
	if _, err := os.Stat(v); err != nil {
		return "", fmt.Errorf("%q is not a readable path: %w", v, err)
	}
	return v, nil
}

func parseIdleTimeout(v string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (try 20m, 1h30m, or off)", v)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", v)
	}
	if d < 10*time.Second {
		return 0, fmt.Errorf("%q is below the 10s minimum; use off to disable", v)
	}
	return d, nil
}

// validateDurationOrOff normalizes a duration setting, writing "off" for zero
// so `skrog config` reads the same way the user would say it.
func validateDurationOrOff(v string) (string, error) {
	d, err := parseIdleTimeout(v)
	if err != nil {
		return "", err
	}
	if d == 0 {
		return "off", nil
	}
	return d.String(), nil
}

// validatePruneKeepSince is validateDurationOrOff minus the escape hatch: an
// automatic prune with no age guard would delete an image pulled an hour ago,
// so "off" is refused here rather than silently meaning "delete everything
// unused". Clearing the key (empty) restores the default guard.
func validatePruneKeepSince(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", nil
	}
	d, err := parseIdleTimeout(v)
	if err != nil {
		return "", err
	}
	if d == 0 {
		return "", fmt.Errorf("an automatic prune always keeps a window; " +
			"clear the key to use the default (168h) rather than turning the guard off")
	}
	return d.String(), nil
}

// Keys lists the settable keys, for help text.
func Keys() []string {
	ks := make([]string, 0, len(validators))
	for k := range validators {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Get returns the stored value for a known key, or its default.
func Get(stateDir, key string) (string, error) {
	if _, ok := validators[key]; !ok {
		return "", fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(Keys(), ", "))
	}
	raw, err := load(stateDir)
	if err != nil {
		return "", err
	}
	if v, ok := raw[key]; ok {
		return v, nil
	}
	return defaultFor(key), nil
}

func defaultFor(key string) string {
	switch key {
	case KeyAudit, KeyImportHostCAs, KeyGPU, KeyVerifySignature:
		return "off"
	case KeyIdleTimeout:
		return DefaultIdleTimeout.String()
	case KeyGPUVendor:
		// Not an on/off key: unset means the default vendor, not disabled.
		return string(gpu.DefaultVendor)
	case KeyPublishScope:
		// Also not on/off: unset means WSL's own behaviour, not "disabled".
		return PublishScopeLoopback
	case KeyDiskWarnBelow:
		return "5GiB"
	}
	return ""
}

// commandHint answers a key that people reasonably reach for but which is an
// operation rather than a stored value. PLAN.md and #64 both spell the data
// dir as `config set data-dir`, so it will be typed; "unknown config key" is a
// dead end when the thing they want does exist, under a command.
func commandHint(key string) string {
	if key == "data-dir" {
		return "`data-dir` is not a stored setting: moving the engine data exports, " +
			"moves and re-imports the distro. Run `skrog relocate <new-directory>` " +
			"(`skrog relocate --help`)."
	}
	return ""
}

// Set validates and stores one key, atomically.
func Set(stateDir, key, value string) error {
	validate, ok := validators[key]
	if !ok {
		if h := commandHint(key); h != "" {
			return errors.New(h)
		}
		return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(Keys(), ", "))
	}
	normalized, err := validate(value)
	if err != nil {
		return err
	}
	raw, err := load(stateDir)
	if err != nil {
		return err
	}
	raw[key] = normalized

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, path(stateDir)); err != nil {
		return fmt.Errorf("committing config: %w", err)
	}
	return nil
}

// All returns every stored setting plus defaults for unset known keys.
func All(stateDir string) (map[string]string, error) {
	raw, err := load(stateDir)
	if err != nil {
		return nil, err
	}
	for _, k := range Keys() {
		if _, ok := raw[k]; !ok {
			raw[k] = defaultFor(k)
		}
	}
	return raw, nil
}

// validateSize accepts a human size (10GB, 8GiB, 512MB) or empty to clear,
// storing the trimmed spelling the user wrote.
func validateSize(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if _, err := parseSize(v); err != nil {
		return "", err
	}
	return v, nil
}

var sizeUnits = map[string]float64{
	"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// parseSize turns "10GB" / "8GiB" / "512MB" into bytes (decimal or binary
// units, case-insensitive). A bare number is refused: a floor without a unit is
// a typo waiting to be 10 bytes.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	if num == "" || unit == "" {
		return 0, fmt.Errorf("%q is not a size (try 10GB or 8GiB)", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q is not a size (try 10GB or 8GiB)", s)
	}
	mult, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q (B, kB, MB, GB, TB, KiB, MiB, GiB, TiB)", s, unit)
	}
	return uint64(f * mult), nil
}

// validateGPUVendor accepts the vendors internal/gpu knows, so a typo is
// refused here rather than producing a CDI spec no container can select.
func validateGPUVendor(v string) (string, error) {
	parsed, err := gpu.ParseVendor(strings.TrimSpace(strings.ToLower(v)))
	if err != nil {
		return "", err
	}
	return string(parsed), nil
}

// The two values KeyPublishScope accepts.
const (
	// PublishScopeLoopback leaves WSL's own forwarding alone: a published port
	// answers on 127.0.0.1 and nowhere else. The default, and the behaviour
	// every release before this one had.
	PublishScopeLoopback = "loopback"
	// PublishScopeLAN has the supervisor relay published ports to every
	// interface, so another device on the network can reach them.
	PublishScopeLAN = "lan"
)

// validatePublishScope refuses anything but the two known scopes.
//
// Spelled-out words rather than a boolean because the set is not closed: a
// future scope ("policy", say, deciding per container) has somewhere to go,
// and `network.publish-scope = on` would have meant nothing.
func validatePublishScope(v string) (string, error) {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "", PublishScopeLoopback:
		return PublishScopeLoopback, nil
	case PublishScopeLAN:
		return PublishScopeLAN, nil
	default:
		return "", fmt.Errorf("%q is not a publish scope; use %q (the default) or %q",
			v, PublishScopeLoopback, PublishScopeLAN)
	}
}
