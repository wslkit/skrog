package main

import (
	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/remote"
	"github.com/wslkit/skrog/internal/runner"
	"github.com/wslkit/skrog/internal/vmtop"
)

// The --json shapes: the CLI contract that machine consumers — the VS Code
// extension, CI scripts, fleet tooling — depend on (#137). They are named types
// so jsonshapes_test.go can pin every key. Changes are additive only; exit codes
// keep the same meaning as the human output (0 ok, 1 error, 2 usage, 3 not
// installed / not found), and a non-zero exit still emits the JSON where there is
// something to say. Documented in docs/cli-json.md.

// statusJSON is `skrog status --json`.
type statusJSON struct {
	Installed bool `json:"installed"`
	// Backend is which engine this install serves. Always "distro" since the
	// session backend was removed (#451), and still always present once
	// installed: dropping a key from a pinned contract (#179) breaks a
	// consumer that switches on it, where an unchanging value does not.
	Backend    string  `json:"backend,omitempty"`
	Distro     string  `json:"distro,omitempty"`
	StateDir   string  `json:"stateDir"`
	Supervisor string  `json:"supervisor"` // running | stopped
	Engine     string  `json:"engine"`     // running | idle | stopped
	Desired    string  `json:"desired"`    // running | stopped
	Profile    string  `json:"profile,omitempty"`
	GPU        gpuJSON `json:"gpu"`
	// Endpoint is where the engine is answering (#273). Absent when no
	// supervisor is running: nothing is being served then, and naming a pipe
	// would be a guess rather than a report.
	Endpoint *endpointJSON `json:"endpoint,omitempty"`
	// Stats is present only with --stats (#179); the default shape is a pinned
	// readiness-probe contract and does not change.
	Stats *statsJSON `json:"stats,omitempty"`
}

// gpuJSON is GPU passthrough state (#83). visible and specInstalled are probed
// only while the engine is running AND gpu is enabled — probing would boot a
// stopped distro, which status must never do (#82) — and probed says whether
// they are authoritative.
type gpuJSON struct {
	Enabled bool `json:"enabled"`
	// Vendor is which vendor spec is configured (#185); omitted for the
	// nvidia default, so the shape is unchanged for every existing install.
	Vendor        string `json:"vendor,omitempty"`
	Probed        bool   `json:"probed"`
	Visible       bool   `json:"visible"`
	SpecInstalled bool   `json:"specInstalled"`
}

// endpointJSON is what the running supervisor bound — read back from its own
// record, never recomputed, because the selector's answer depends on what else
// held the default pipe at the time (see supervise.Endpoint).
type endpointJSON struct {
	// Pipe is the named pipe, `\\.\pipe\docker_engine` form; DockerHost is the
	// same thing as the docker CLI wants it, so a consumer setting DOCKER_HOST
	// does not have to know the npipe:// spelling.
	Pipe       string `json:"pipe"`
	DockerHost string `json:"dockerHost"`
	// Reason explains the choice — the default pipe was free, or something
	// else already had it.
	Reason string `json:"reason,omitempty"`
}

// cliStatusJSON is `skrog cli status --json` (#66).
type cliStatusJSON struct {
	Arch         string        `json:"arch"`
	BinDir       string        `json:"binDir"`
	OnPath       bool          `json:"onPath"`
	ActiveDocker string        `json:"activeDocker,omitempty"`
	Tools        []cliToolJSON `json:"tools"`
}

// cliToolJSON is one bundled tool. Available is whether the manifest publishes
// it for the host arch at all (the docker CLI has no Windows arm64 build).
type cliToolJSON struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Role      string `json:"role"` // cli | plugin | helper
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	Available bool   `json:"available"`
}

// configListJSON is `skrog config --json`. Engine is null when no engine is
// installed and {} when one is installed with nothing set — different answers.
type configListJSON struct {
	Settings map[string]string `json:"settings"`
	Engine   map[string]string `json:"engine"`
}

// profileListJSON is `skrog profile --json`; profiles is always an array.
type profileListJSON struct {
	Active   string             `json:"active,omitempty"`
	Profiles []profileEntryJSON `json:"profiles"`
}

type profileEntryJSON struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// snapshotRestoredJSON and snapshotDeletedJSON are the results of those verbs.
// `snapshot save --json` emits the snapshot.Meta, `snapshot list --json` an
// array of them (always an array, never null).
type snapshotRestoredJSON struct {
	Restored string `json:"restored"`
}

type snapshotDeletedJSON struct {
	Deleted string `json:"deleted"`
}

// remoteListJSON is `skrog remote list --json` (#138). current is "local" when
// docker is on the skrog context, a remote's name when on skrog-<name>, and ""
// when docker is on some other context entirely.
type remoteListJSON struct {
	Current string            `json:"current"`
	Remotes []remoteEntryJSON `json:"remotes"`
}

// remoteEntryJSON is a registered remote plus whether docker is on it now.
type remoteEntryJSON struct {
	remote.Info
	Current bool `json:"current"`
}

// remoteTestJSON is `skrog remote test <name> --json`.
type remoteTestJSON struct {
	Name          string `json:"name"`
	ServerVersion string `json:"serverVersion"`
	Millis        int64  `json:"ms"`
}

// traceJSON is `skrog audit trace --json -- <cmd>` (#152): what the command
// did to the engine. actions counts audit events by action name; images and
// containers are the distinct ones touched (always arrays). exitCode is the
// traced command's own, which the process also exits with.
type traceJSON struct {
	Command    []string       `json:"command"`
	ExitCode   int            `json:"exitCode"`
	Millis     int64          `json:"ms"`
	Events     int            `json:"events"`
	Actions    map[string]int `json:"actions"`
	Images     []string       `json:"images"`
	Containers []string       `json:"containers"`
	Note       string         `json:"note,omitempty"`
}

// healthcheckJSON is `skrog healthcheck --json` (#146). ready is the verdict
// the exit code carries (0 ready, 1 not, 3 not installed); reason always says
// why, in words a runner log can show.
type healthcheckJSON struct {
	Installed  bool   `json:"installed"`
	Supervisor string `json:"supervisor"` // running | stopped
	Engine     string `json:"engine"`     // running | idle | stopped
	Ready      bool   `json:"ready"`
	Reason     string `json:"reason"`
}

// logLineJSON is one line of `skrog logs --json` (#146): the same envelope for
// every source so a log shipper needs one pipeline. line is the raw record; a
// shipper that wants dockerd's logfmt or the audit JSON parses it further.
type logLineJSON struct {
	Source string `json:"source"` // supervisor | dockerd | audit
	Line   string `json:"line"`
}

// prewarmJSON is `skrog prewarm --json <file>` (#149). images is in list order
// and always an array; the exit code is 0 only when failed is 0.
type prewarmJSON struct {
	File        string             `json:"file"`
	Concurrency int                `json:"concurrency"`
	Pulled      int                `json:"pulled"`
	Failed      int                `json:"failed"`
	Millis      int64              `json:"ms"`
	Images      []prewarmImageJSON `json:"images"`
}

type prewarmImageJSON struct {
	Ref    string `json:"ref"`
	OK     bool   `json:"ok"`
	Millis int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

// runnerCheckJSON is `skrog runner check --json` (#150). ready is the verdict
// the exit code carries (0 ready — warnings allowed — 1 not ready, 3 not
// installed); findings is always an array, each with a remedy when not ok.
type runnerCheckJSON struct {
	Ready    bool             `json:"ready"`
	Findings []runner.Finding `json:"findings"`
}

// resetJSON is `skrog reset --to <snapshot> --json` (#142): what the engine
// was reset to and how long the whole cycle took (verify, unregister, import,
// engine back) — the number a runner's clean-slate budget is measured against.
type resetJSON struct {
	Snapshot      string `json:"snapshot"`
	EngineVersion string `json:"engineVersion,omitempty"`
	Millis        int64  `json:"ms"`
}

// pruneJSON is `skrog prune --json` (#145): bytes reclaimed per step and in
// total. steps is always an array in plan order; error is omitted on success.
// Exit 0 only when failed is 0.
type pruneJSON struct {
	ReclaimedBytes int64           `json:"reclaimedBytes"`
	Failed         int             `json:"failed"`
	Steps          []pruneStepJSON `json:"steps"`
}

type pruneStepJSON struct {
	Name           string `json:"name"` // containers | images | volumes | build-cache
	ReclaimedBytes int64  `json:"reclaimedBytes"`
	Error          string `json:"error,omitempty"`
}

// compactJSONShape is `skrog compact --json` (#64). reclaimedBytes is the only
// honest measure of what happened: beforeBytes/afterBytes are the .vhdx's size
// ON DISK, and offeredBytes is what fstrim printed -- the disk's whole free
// extent, not space reclaimed, which is why it is named "offered". held is set
// when the disk could not be released, and pairs with exit code 11.
type compactJSONShape struct {
	Distro         string   `json:"distro"`
	Path           string   `json:"path"`
	Trimmed        bool     `json:"trimmed"`
	OfferedBytes   uint64   `json:"offeredBytes,omitempty"`
	BeforeBytes    uint64   `json:"beforeBytes"`
	AfterBytes     uint64   `json:"afterBytes"`
	ReclaimedBytes uint64   `json:"reclaimedBytes"`
	WaitedSeconds  float64  `json:"waitedSeconds"`
	Restarted      bool     `json:"restarted"`
	DryRun         bool     `json:"dryRun"`
	Steps          []string `json:"steps,omitempty"`
	Held           []string `json:"held,omitempty"`
}

// policyShowJSON is `skrog policy show --json` (#120).
type policyShowJSON struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Active   bool   `json:"active"`
	Enforced bool   `json:"enforced"`
	// Rules are the EFFECTIVE rules: the machine layer tightened by the user's
	// (#386). Unchanged shape for a machine with no fleet policy, which is
	// most of them.
	Rules policy.Rules `json:"rules"`
	// Source reports what each layer contributed, so "why can I not run this"
	// has an answer that names the file to argue with.
	Source policy.Source `json:"source"`
	// MachineProvenance says where the machine layer came from and whether it
	// was trusted (#418). A fleet dashboard needs to tell a machine with no
	// policy from one whose policy was refused; they were indistinguishable
	// here, and the second is the one worth an alert.
	MachineProvenance policy.Provenance `json:"machineProvenance"`
}

// policyTestJSON is `skrog policy test --json` (#120): the verdict on one
// request, so a script can gate on it without parsing prose.
type policyTestJSON struct {
	Denied bool   `json:"denied"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// relocateJSONShape is `skrog relocate --json` (#64). needBytes/freeBytes are
// the space check, and are the whole payload of a --dry-run.
type relocateJSONShape struct {
	Distro      string   `json:"distro"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	MovedBytes  int64    `json:"movedBytes"`
	SHA256      string   `json:"sha256,omitempty"`
	NeedBytes   uint64   `json:"needBytes"`
	FreeBytes   uint64   `json:"freeBytes"`
	Steps       []string `json:"steps,omitempty"`
	DryRun      bool     `json:"dryRun"`
	Restarted   bool     `json:"restarted"`
	ArchiveKept string   `json:"archiveKept,omitempty"`
}

// engineListJSON is `skrog engine list --json` (#65): what this build can
// install, what is installed, and what a rollback would return to. available
// is always an array; ref is the revisioned label (29.7.2-4), which is the
// unit an upgrade moves between, while version is only the dockerd version.
type engineListJSON struct {
	Installed string            `json:"installed,omitempty"`
	Previous  string            `json:"previous,omitempty"`
	Available []engineEntryJSON `json:"available"`
}

type engineEntryJSON struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Default bool   `json:"default"`
	// Published is false for a manifest entry with no checksum yet: a
	// placeholder that cannot be installed.
	Published bool `json:"published"`
}

// engineUpgradeJSON is `skrog engine upgrade|rollback --json` (#65).
// rolledBack true with a non-zero exit is the interesting case: the upgrade
// failed and the previous engine was put back, so the engine is up.
type engineUpgradeJSON struct {
	From          string   `json:"from,omitempty"`
	To            string   `json:"to"`
	Replaced      []string `json:"replaced,omitempty"`
	EngineVersion string   `json:"engineVersion,omitempty"`
	RolledBack    bool     `json:"rolledBack"`
	DryRun        bool     `json:"dryRun"`
	Steps         []string `json:"steps,omitempty"`
}

// wslConfigJSON is `skrog wsl-config show|apply --json` (#148). effective is
// what ~/.wslconfig says now, desired what Skrog's own settings ask for, and
// pending the difference -- so a converge run can tell "already right" from
// "would change something" without parsing prose. applied is false for show
// and for an apply with nothing to do.
type wslConfigJSON struct {
	Path      string                `json:"path"`
	Exists    bool                  `json:"exists"`
	Effective map[string]string     `json:"effective"`
	Desired   map[string]string     `json:"desired"`
	Pending   []wslConfigChangeJSON `json:"pending,omitempty"`
	Applied   bool                  `json:"applied"`
}

type wslConfigChangeJSON struct {
	Key string `json:"key"`
	// Old is absent when the key is being added.
	Old   string `json:"old,omitempty"`
	New   string `json:"new"`
	Added bool   `json:"added"`
}

// statsJSON is the `stats` object `skrog status --stats --json` adds (#179).
// The default status shape is untouched: it is a pinned readiness-probe
// contract, and statistics are opt-in.
//
// probed says whether the engine was up. Statistics are never collected by
// starting it (#82), so probed=false means engine/disk/vm are absent rather
// than zero -- "no data" and "zero containers" must not look alike.
//
// supervisor and bridge come from the file the supervisor flushes, so they are
// present even when the engine is down, and carry the reading's age: a
// supervisor that died leaves its last numbers behind, and fresh=false is how a
// reader knows not to trust them as current.
type statsJSON struct {
	Probed     bool                 `json:"probed"`
	Supervisor *supervisorStatsJSON `json:"supervisor,omitempty"`
	Bridge     *bridgeStatsJSON     `json:"bridge,omitempty"`
	Engine     *engineStatsJSON     `json:"engine,omitempty"`
	Disk       *diskStatsJSON       `json:"disk,omitempty"`
	VM         *vmStatsJSON         `json:"vm,omitempty"`
	// Errors names what could not be read, so a partial reading is honest
	// rather than silently short.
	Errors []string `json:"errors,omitempty"`
}

type supervisorStatsJSON struct {
	Fresh            bool    `json:"fresh"`
	ReadingAgeSecs   float64 `json:"readingAgeSeconds"`
	StartedAt        string  `json:"startedAt,omitempty"`
	UptimeSecs       float64 `json:"uptimeSeconds"`
	EngineStartedAt  string  `json:"engineStartedAt,omitempty"`
	EngineUptimeSecs float64 `json:"engineUptimeSeconds"`
	EngineStarts     int     `json:"engineStarts"`
	IdleStops        int     `json:"idleStops"`
	LastIdleStopAt   string  `json:"lastIdleStopAt,omitempty"`
	LastWakeAt       string  `json:"lastWakeAt,omitempty"`
}

// bridgeStatsJSON is what the pipe carried. transport is "vsock" (fast path),
// "fallback" (the socat relay, ~165 ms per connection instead of ~0.6 ms) or
// "direct"; it is the field that explains a slow docker with a healthy engine.
type bridgeStatsJSON struct {
	Connections   uint64 `json:"connections"`
	BytesToEngine uint64 `json:"bytesToEngine"`
	BytesToClient uint64 `json:"bytesToClient"`
	ActiveConns   int    `json:"activeConns"`
	Transport     string `json:"transport"`
}

type engineStatsJSON struct {
	Version          string `json:"version,omitempty"`
	Containers       int    `json:"containers"`
	Running          int    `json:"containersRunning"`
	Paused           int    `json:"containersPaused"`
	Stopped          int    `json:"containersStopped"`
	Images           int    `json:"images"`
	Volumes          int    `json:"volumes"`
	ImagesBytes      uint64 `json:"imagesBytes"`
	VolumesBytes     uint64 `json:"volumesBytes"`
	BuildCacheBytes  uint64 `json:"buildCacheBytes"`
	ReclaimableBytes uint64 `json:"reclaimableBytes"`
}

// diskStatsJSON is the host-side footprint. reclaimableBytes is size-on-disk
// minus what the guest uses -- roughly what `skrog compact` could return, and
// an estimate rather than a promise, since compaction works in blocks.
type diskStatsJSON struct {
	Path             string `json:"path"`
	SizeOnDiskBytes  uint64 `json:"sizeOnDiskBytes"`
	GuestUsedBytes   uint64 `json:"guestUsedBytes"`
	ReclaimableBytes uint64 `json:"reclaimableBytes"`
	HostFreeBytes    uint64 `json:"hostFreeBytes"`
}

// vmStatsJSON pairs what the VM has with what ~/.wslconfig asked for: the two
// disagreeing is the trap `skrog wsl-config` closes (#148).
type vmStatsJSON struct {
	CPUs                 int    `json:"cpus"`
	MemTotalBytes        uint64 `json:"memTotalBytes"`
	MemAvailableBytes    uint64 `json:"memAvailableBytes"`
	SwapTotalBytes       uint64 `json:"swapTotalBytes"`
	ConfiguredMemory     string `json:"configuredMemory,omitempty"`
	ConfiguredProcessors string `json:"configuredProcessors,omitempty"`
}

// topJSON is `skrog top --json` (#511). engine is the same vocabulary as
// status: running, idle or stopped. reading is present only while the engine
// runs -- top never starts it to take one (#82) -- and its shape is
// vmtop.Snapshot, documented key by key in docs/cli-json.md.
type topJSON struct {
	Engine string `json:"engine"`
	Distro string `json:"distro,omitempty"`
	// AutoMemoryReclaim is ~/.wslconfig's setting verbatim, omitted when the
	// file does not set it: WSL's own default is not assumed.
	AutoMemoryReclaim string          `json:"autoMemoryReclaim,omitempty"`
	Reading           *vmtop.Snapshot `json:"reading,omitempty"`
	// Error is set on a --stream line whose reading failed while the engine
	// was running; the stream carries on rather than ending on one bad read.
	Error string `json:"error,omitempty"`
}
