package main

import (
	"os"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/vmtop"
)

// fixtureSnapshot is the real sample vmtop's own tests use, taken once, so
// the rendering is exercised on numbers the kernel actually produced.
func fixtureSnapshot(t *testing.T) vmtop.Snapshot {
	t.Helper()
	b, err := os.ReadFile("../../internal/vmtop/testdata/sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := vmtop.ParseSample(string(b))
	return vmtop.Compute(s, s)
}

func TestRenderTopNamesEveryRow(t *testing.T) {
	var out strings.Builder
	renderTop(&out, fixtureSnapshot(t), "")
	text := out.String()
	for _, want := range []string{
		"skrog-t-wrong", "skrog-t-any", "engine (dockerd", "other WSL distros (0)",
		"WSL itself", "not charged to any group", "page cache",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	// A single sample has no CPU window: a dash, never a measured-looking 0.0.
	if !strings.Contains(text, "CPU -%") {
		t.Errorf("single-sample CPU should render as a dash:\n%s", text)
	}
}

// The advice this view exists to give: page cache dominating what the VM
// holds, with the setting that hands it back -- or what the file already says.
func TestRenderTopPageCacheAdvice(t *testing.T) {
	s := fixtureSnapshot(t)
	s.VM.MemUsedBytes = 4 << 30
	s.VM.PageCacheBytes = 3 << 30

	var unset strings.Builder
	renderTop(&unset, s, "")
	if !strings.Contains(unset.String(), "wsl.auto-memory-reclaim") {
		t.Errorf("with autoMemoryReclaim unset, the advice should name the setting:\n%s", unset.String())
	}

	var set strings.Builder
	renderTop(&set, s, "gradual")
	if !strings.Contains(set.String(), "autoMemoryReclaim=gradual") ||
		strings.Contains(set.String(), "skrog config set") {
		t.Errorf("with autoMemoryReclaim set, the advice should report it, not repeat the command:\n%s", set.String())
	}

	// And silent when the cache is small: the fixture's 178 MiB is noise.
	var quiet strings.Builder
	renderTop(&quiet, fixtureSnapshot(t), "")
	if strings.Contains(quiet.String(), "Linux will drop") {
		t.Errorf("advice shown for a small page cache:\n%s", quiet.String())
	}
}

func TestTopShape(t *testing.T) {
	snap := fixtureSnapshot(t)
	snap.Vmmem = &vmtop.Vmmem{Process: "vmmemWSL", WorkingSetBytes: 1}
	m := roundTrip(t, topJSON{Engine: "running", Distro: "skrog-engine", Reading: &snap})
	requireKeys(t, m, "engine", "distro", "reading")
	if _, ok := m["autoMemoryReclaim"]; ok {
		t.Error("autoMemoryReclaim should be omitted when ~/.wslconfig does not set it")
	}
	r, ok := m["reading"].(map[string]any)
	if !ok {
		t.Fatalf("reading should be an object: %v", m["reading"])
	}
	requireKeys(t, r, "takenAt", "windowSecs", "vm", "containers", "engine", "otherDistros",
		"otherDistroCount", "wsl", "otherContainers", "unchargedBytes", "vmmem")
	vm := r["vm"].(map[string]any)
	requireKeys(t, vm, "cpus", "cpuPercent", "memTotalBytes", "memFreeBytes", "memAvailableBytes",
		"memUsedBytes", "anonBytes", "pageCacheBytes", "kernelBytes", "pressure")
	c := r["containers"].([]any)[0].(map[string]any)
	requireKeys(t, c, "id", "name", "memoryBytes", "anonBytes", "fileBytes", "kernelBytes",
		"cpuPercent", "ioReadBytes", "ioWriteBytes", "pressure")
	requireKeys(t, c["pressure"].(map[string]any), "cpu", "memory", "io")
	requireKeys(t, r["vmmem"].(map[string]any), "process", "pid", "workingSetBytes",
		"privateWorkingSetBytes", "privateBytes")

	// Not running: no reading at all, rather than a zeroed one that reads as
	// an empty VM.
	down := roundTrip(t, topJSON{Engine: "idle", Distro: "skrog-engine"})
	if _, ok := down["reading"]; ok {
		t.Error("reading should be absent while the engine is not running")
	}
}
