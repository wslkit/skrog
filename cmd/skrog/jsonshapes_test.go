package main

import (
	"encoding/json"
	"testing"

	"github.com/wslkit/skrog/internal/runner"
	"github.com/wslkit/skrog/internal/skrogfile"
	"github.com/wslkit/skrog/internal/snapshot"
)

// These tests pin the --json contract (#137, docs/cli-json.md): the keys tools
// depend on, and the deliberate null-vs-empty choices. A rename or removal here
// is a breaking change for every consumer, so it must be a conscious one.

// roundTrip marshals v and decodes it generically, the way a consumer sees it.
func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	return m
}

func requireKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %v", k, m)
		}
	}
}

func TestStatusShape(t *testing.T) {
	m := roundTrip(t, statusJSON{StateDir: `C:\x`, Supervisor: "running", Engine: "idle", Desired: "running"})
	requireKeys(t, m, "installed", "stateDir", "supervisor", "engine", "desired", "gpu")
	// Empty optionals are omitted, not emitted as "".
	for _, k := range []string{"distro", "profile"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q should be omitted when empty", k)
		}
	}
	gpu, ok := m["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("gpu should be an object: %v", m["gpu"])
	}
	requireKeys(t, gpu, "enabled", "probed", "visible", "specInstalled")
}

func TestCLIStatusShape(t *testing.T) {
	m := roundTrip(t, cliStatusJSON{Arch: "amd64", BinDir: `C:\b`, Tools: []cliToolJSON{}})
	requireKeys(t, m, "arch", "binDir", "onPath", "tools")
	if tools, ok := m["tools"].([]any); !ok || tools == nil {
		t.Errorf("tools must be an array even when empty, got %v", m["tools"])
	}
	tool := roundTrip(t, cliToolJSON{Name: "docker", Role: "cli"})
	requireKeys(t, tool, "name", "version", "role", "path", "installed", "available")
}

func TestConfigListNullVsEmptyEngine(t *testing.T) {
	// No engine installed: engine is null — a different answer from "installed,
	// nothing set" ({}), and consumers branch on it.
	m := roundTrip(t, configListJSON{Settings: map[string]string{"gpu": "off"}})
	requireKeys(t, m, "settings", "engine")
	if m["engine"] != nil {
		t.Errorf("engine should be null with no engine, got %v", m["engine"])
	}
	m = roundTrip(t, configListJSON{Settings: map[string]string{}, Engine: map[string]string{}})
	if eng, ok := m["engine"].(map[string]any); !ok || eng == nil {
		t.Errorf("engine should be {} when installed with nothing set, got %v", m["engine"])
	}
}

func TestProfileListShape(t *testing.T) {
	m := roundTrip(t, profileListJSON{Profiles: []profileEntryJSON{}})
	requireKeys(t, m, "profiles")
	if _, ok := m["active"]; ok {
		t.Error("active should be omitted when no profile is active")
	}
	if p, ok := m["profiles"].([]any); !ok || p == nil {
		t.Errorf("profiles must be an array even when empty, got %v", m["profiles"])
	}
	e := roundTrip(t, profileEntryJSON{Name: "work", Active: true})
	requireKeys(t, e, "name", "active")
}

func TestSnapshotResultShapes(t *testing.T) {
	requireKeys(t, roundTrip(t, snapshotRestoredJSON{Restored: "x"}), "restored")
	requireKeys(t, roundTrip(t, snapshotDeletedJSON{Deleted: "x"}), "deleted")
	// The list path emits snapshot.Meta values; pin its keys here too.
	requireKeys(t, roundTrip(t, snapshot.Meta{Name: "x"}), "name", "created", "distro", "sha256", "sizeBytes")
	// An empty list is [] — the list code builds a non-nil slice for exactly this.
	b, _ := json.Marshal([]snapshot.Meta{})
	if string(b) != "[]" {
		t.Errorf("empty snapshot list = %s, want []", b)
	}
}

// TestSkrogfileJSONMirrorsYAML: `profile show --json` and `config export --json`
// must emit the same document a skrog.yaml holds, so the JSON keys are the
// kebab-case YAML keys, not Go field names.
func TestSkrogfileJSONMirrorsYAML(t *testing.T) {
	on := true
	m := roundTrip(t, skrogfile.File{
		Distro: "d", DataDir: "x", EngineVersion: "1", IdleTimeout: "off", Autostart: &on,
		Engine: map[string]string{"a": "b"}, Hooks: map[string]string{"post-start": "s"},
		Integrations: []string{"Ubuntu"},
	})
	requireKeys(t, m, "distro", "data-dir", "engine-version", "idle-timeout", "autostart", "engine", "hooks", "integrations")
	for _, bad := range []string{"Distro", "DataDir", "EngineVersion"} {
		if _, ok := m[bad]; ok {
			t.Errorf("Go field name %q leaked into JSON", bad)
		}
	}
}

func TestTraceShape(t *testing.T) {
	m := roundTrip(t, traceJSON{Command: []string{"x"}, Actions: map[string]int{}, Images: []string{}, Containers: []string{}})
	requireKeys(t, m, "command", "exitCode", "ms", "events", "actions", "images", "containers")
	if _, ok := m["note"]; ok {
		t.Error("note should be omitted when empty")
	}
	for _, k := range []string{"images", "containers"} {
		if a, ok := m[k].([]any); !ok || a == nil {
			t.Errorf("%s must be an array even when empty, got %v", k, m[k])
		}
	}
}

func TestRemoteShapes(t *testing.T) {
	m := roundTrip(t, remoteListJSON{Current: "local", Remotes: []remoteEntryJSON{}})
	requireKeys(t, m, "current", "remotes")
	if a, ok := m["remotes"].([]any); !ok || a == nil {
		t.Errorf("remotes must be an array even when empty, got %v", m["remotes"])
	}
	e := roundTrip(t, remoteEntryJSON{Current: true})
	// The embedded remote.Info flattens: its keys sit beside current.
	requireKeys(t, e, "name", "host", "added", "certNotAfter", "dir", "current")
	requireKeys(t, roundTrip(t, remoteTestJSON{}), "name", "serverVersion", "ms")
}

func TestHealthcheckAndLogShapes(t *testing.T) {
	m := roundTrip(t, healthcheckJSON{Supervisor: "running", Engine: "idle", Ready: true, Reason: "r"})
	requireKeys(t, m, "installed", "supervisor", "engine", "ready", "reason")
	// reason is never omitted: a runner log must always be able to say why.
	m = roundTrip(t, healthcheckJSON{})
	requireKeys(t, m, "reason", "ready")

	l := roundTrip(t, logLineJSON{Source: "dockerd", Line: "x"})
	requireKeys(t, l, "source", "line")
}

func TestPrewarmShape(t *testing.T) {
	m := roundTrip(t, prewarmJSON{File: "images.txt", Concurrency: 3, Images: []prewarmImageJSON{}})
	requireKeys(t, m, "file", "concurrency", "pulled", "failed", "ms", "images")
	if a, ok := m["images"].([]any); !ok || a == nil {
		t.Errorf("images must be an array even when empty, got %v", m["images"])
	}
	img := roundTrip(t, prewarmImageJSON{Ref: "alpine:3.20", OK: true})
	requireKeys(t, img, "ref", "ok", "ms")
	if _, ok := img["error"]; ok {
		t.Error("error should be omitted for a successful pull")
	}
}

func TestRunnerCheckShape(t *testing.T) {
	m := roundTrip(t, runnerCheckJSON{Ready: true, Findings: []runner.Finding{}})
	requireKeys(t, m, "ready", "findings")
	if a, ok := m["findings"].([]any); !ok || a == nil {
		t.Errorf("findings must be an array even when empty, got %v", m["findings"])
	}
	f := roundTrip(t, runner.Finding{Name: "autologon", Status: runner.OK, Summary: "s"})
	requireKeys(t, f, "name", "status", "summary")
	if _, ok := f["remedy"]; ok {
		t.Error("remedy should be omitted when empty")
	}
}

func TestResetShape(t *testing.T) {
	m := roundTrip(t, resetJSON{Snapshot: "golden", Millis: 4200})
	requireKeys(t, m, "snapshot", "ms")
	if _, ok := m["engineVersion"]; ok {
		t.Error("engineVersion should be omitted when unknown")
	}
	m = roundTrip(t, resetJSON{Snapshot: "golden", EngineVersion: "29.7.2"})
	requireKeys(t, m, "engineVersion")
}

func TestPruneShape(t *testing.T) {
	m := roundTrip(t, pruneJSON{Steps: []pruneStepJSON{}})
	requireKeys(t, m, "reclaimedBytes", "failed", "steps")
	if a, ok := m["steps"].([]any); !ok || a == nil {
		t.Errorf("steps must be an array even when empty, got %v", m["steps"])
	}
	s := roundTrip(t, pruneStepJSON{Name: "images", ReclaimedBytes: 5})
	requireKeys(t, s, "name", "reclaimedBytes")
	if _, ok := s["error"]; ok {
		t.Error("error should be omitted on success")
	}
}

func TestCompactShape(t *testing.T) {
	m := roundTrip(t, compactJSONShape{Distro: "skrog-engine", Path: `C:\x\ext4.vhdx`})
	requireKeys(t, m, "distro", "path", "trimmed", "beforeBytes", "afterBytes",
		"reclaimedBytes", "waitedSeconds", "restarted", "dryRun")
	// offeredBytes is fstrim's misleading figure: absent unless it ran, so no
	// consumer sees a zero and reads it as "nothing was trimmed".
	if _, ok := m["offeredBytes"]; ok {
		t.Error("offeredBytes should be omitted when fstrim did not run")
	}
	// held only appears when other distros are holding the disk; its presence
	// is the machine-readable form of exit code 11.
	if _, ok := m["held"]; ok {
		t.Error("held should be omitted when nothing is holding the disk")
	}
	m = roundTrip(t, compactJSONShape{OfferedBytes: 1078939029504, Held: []string{"docker-desktop"}})
	requireKeys(t, m, "offeredBytes", "held")
}

func TestEngineListShape(t *testing.T) {
	m := roundTrip(t, engineListJSON{Available: []engineEntryJSON{}})
	requireKeys(t, m, "available")
	if a, ok := m["available"].([]any); !ok || a == nil {
		t.Errorf("available must be an array even when empty, got %v", m["available"])
	}
	// installed/previous are omitted rather than empty strings: "no install"
	// and "installed, name unknown" are different states.
	for _, k := range []string{"installed", "previous"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be omitted when unknown", k)
		}
	}
	e := roundTrip(t, engineEntryJSON{Ref: "29.7.2-4", Version: "29.7.2", Default: true, Published: true})
	requireKeys(t, e, "ref", "version", "default", "published")
}

func TestEngineUpgradeShape(t *testing.T) {
	m := roundTrip(t, engineUpgradeJSON{To: "29.7.2-4"})
	requireKeys(t, m, "to", "rolledBack", "dryRun")
	// from is omitted on an install that predates the bookkeeping; replaced and
	// engineVersion are absent on a dry run.
	for _, k := range []string{"from", "replaced", "engineVersion"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be omitted when empty", k)
		}
	}
	m = roundTrip(t, engineUpgradeJSON{From: "29.7.2-3", To: "29.7.2-4",
		Replaced: []string{"dockerd"}, EngineVersion: "29.7.2", RolledBack: true})
	requireKeys(t, m, "from", "replaced", "engineVersion")
	if m["rolledBack"] != true {
		t.Error("rolledBack must survive the round trip: it is how a caller learns the engine is back")
	}
}

func TestWSLConfigShape(t *testing.T) {
	m := roundTrip(t, wslConfigJSON{
		Path:      `C:\Users\me\.wslconfig`,
		Effective: map[string]string{},
		Desired:   map[string]string{},
	})
	requireKeys(t, m, "path", "exists", "effective", "desired", "applied")
	// pending absent is the signal that a repeated `apply --yes` is a no-op.
	if _, ok := m["pending"]; ok {
		t.Error("pending should be omitted when there is nothing to do")
	}
	c := roundTrip(t, wslConfigChangeJSON{Key: "memory", New: "4GB", Added: true})
	requireKeys(t, c, "key", "new", "added", "section")
	if _, ok := c["old"]; ok {
		t.Error("old should be omitted when the key is being added")
	}
	c = roundTrip(t, wslConfigChangeJSON{Key: "memory", Old: "8GB", New: "4GB"})
	requireKeys(t, c, "old")
}

func TestStatusStatsShape(t *testing.T) {
	// The default status shape must not change: it is a pinned readiness-probe
	// contract, and stats are opt-in.
	m := roundTrip(t, statusJSON{})
	if _, ok := m["stats"]; ok {
		t.Error("stats appears without --stats")
	}

	m = roundTrip(t, statusJSON{Stats: &statsJSON{}})
	stats, ok := m["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats = %v", m["stats"])
	}
	requireKeys(t, stats, "probed")
	// Every group is a pointer, so "engine was down" is absent rather than a
	// wall of zeroes that reads like an empty engine.
	for _, k := range []string{"engine", "disk", "vm", "supervisor", "bridge"} {
		if _, ok := stats[k]; ok {
			t.Errorf("%s present when it was not collected", k)
		}
	}
}

func TestStatsGroupShapes(t *testing.T) {
	e := roundTrip(t, engineStatsJSON{})
	requireKeys(t, e, "containers", "containersRunning", "containersPaused",
		"containersStopped", "images", "volumes", "imagesBytes", "volumesBytes",
		"buildCacheBytes", "reclaimableBytes")

	d := roundTrip(t, diskStatsJSON{})
	requireKeys(t, d, "path", "sizeOnDiskBytes", "guestUsedBytes", "reclaimableBytes", "hostFreeBytes")

	v := roundTrip(t, vmStatsJSON{})
	requireKeys(t, v, "cpus", "memTotalBytes", "memAvailableBytes", "swapTotalBytes")
	// Configured sizing is omitted when ~/.wslconfig sets none: "unset" and
	// "set to empty" are different answers.
	if _, ok := v["configuredMemory"]; ok {
		t.Error("configuredMemory present with no sizing configured")
	}

	b := roundTrip(t, bridgeStatsJSON{Transport: "vsock"})
	requireKeys(t, b, "connections", "bytesToEngine", "bytesToClient", "activeConns", "transport")

	s := roundTrip(t, supervisorStatsJSON{})
	requireKeys(t, s, "fresh", "readingAgeSeconds", "uptimeSeconds",
		"engineUptimeSeconds", "engineStarts", "idleStops")
}

func TestStatusEndpointShape(t *testing.T) {
	// Absent is the answer when no supervisor is running (#273): there is
	// nothing being served, and a pipe name would be a guess.
	m := roundTrip(t, statusJSON{})
	if _, ok := m["endpoint"]; ok {
		t.Error("endpoint appears with no supervisor running")
	}

	m = roundTrip(t, statusJSON{Endpoint: &endpointJSON{
		Pipe:       `\.\pipe\docker_engine`,
		DockerHost: "npipe:////./pipe/docker_engine",
	}})
	ep, ok := m["endpoint"].(map[string]any)
	if !ok {
		t.Fatalf("endpoint should be an object: %v", m["endpoint"])
	}
	requireKeys(t, ep, "pipe", "dockerHost")
	// Both spellings travel: a consumer setting DOCKER_HOST must not have to
	// know how to convert one into the other.
	if ep["pipe"] != `\.\pipe\docker_engine` {
		t.Errorf("pipe = %v", ep["pipe"])
	}
	if ep["dockerHost"] != "npipe:////./pipe/docker_engine" {
		t.Errorf("dockerHost = %v", ep["dockerHost"])
	}
	if _, ok := ep["reason"]; ok {
		t.Error("reason should be omitted when there is none")
	}
}
