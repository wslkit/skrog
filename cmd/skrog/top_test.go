package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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
		"memUsedBytes", "anonBytes", "pageCacheBytes", "kernelBytes", "unitemisedBytes", "pressure")
	c := r["containers"].([]any)[0].(map[string]any)
	requireKeys(t, c, "id", "name", "memoryBytes", "anonBytes", "fileBytes", "kernelBytes",
		"cpuPercent", "ioReadBytes", "ioWriteBytes", "pids", "pressure")
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

// fakeDistro answers every exec with the recorded sample.
type fakeDistro struct{ out string }

func (f fakeDistro) Exec(context.Context, string, string, ...string) (string, error) {
	return f.out, nil
}

// lineWriter records lines and ends the stream after n of them.
type lineWriter struct {
	lines  []string
	buf    strings.Builder
	n      int
	cancel context.CancelFunc
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		s := w.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		w.lines = append(w.lines, s[:i])
		w.buf.Reset()
		w.buf.WriteString(s[i+1:])
		if len(w.lines) >= w.n {
			w.cancel()
		}
	}
	return len(p), nil
}

// --json --stream: one whole object per line. A line while the engine is down
// carries its state and no reading; the first reading after it has no CPU
// window, and says so with windowSecs 0; the next one has a window.
func TestStreamTopJSONOneObjectPerLine(t *testing.T) {
	b, err := os.ReadFile("../../internal/vmtop/testdata/sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := []string{"idle", "running", "running"}
	calls := 0
	state := func(context.Context) string {
		s := states[min(calls, len(states)-1)]
		calls++
		return s
	}
	w := &lineWriter{n: 3, cancel: cancel}
	reader := &vmtop.Reader{WSL: fakeDistro{string(b)}}

	if code := streamTopJSON(ctx, w, 300*time.Millisecond, "skrog-engine", state, reader, ""); code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if len(w.lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(w.lines), w.lines)
	}
	var got []topJSON
	for i, l := range w.lines {
		var v topJSON
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %d is not one JSON object: %v: %s", i+1, err, l)
		}
		got = append(got, v)
	}
	if got[0].Engine != "idle" || got[0].Reading != nil {
		t.Errorf("line 1 = %+v, want the idle state and no reading", got[0])
	}
	if got[1].Reading == nil || got[1].Reading.WindowSecs != 0 {
		t.Errorf("line 2 should be a reading with no CPU window yet: %+v", got[1].Reading)
	}
	if got[2].Reading == nil || got[2].Reading.WindowSecs <= 0 {
		t.Errorf("line 3 should average CPU over the gap since line 2: %+v", got[2].Reading)
	}
}

func TestVmmemGap(t *testing.T) {
	snap := func(host, guest uint64) vmtop.Snapshot {
		s := vmtop.Snapshot{VM: vmtop.VM{MemUsedBytes: guest}}
		if host > 0 {
			s.Vmmem = &vmtop.Vmmem{PrivateWorkingSetBytes: host}
		}
		return s
	}
	const mib = 1 << 20
	for _, tc := range []struct {
		name        string
		host, guest uint64
		want        uint64
	}{
		// This machine, 2026-09-22: 845.5 MiB against 592.4 MiB used, a 253 MiB
		// surplus -- just under the floor, so no line. The floor was set
		// before looking, and is not moved to make this example show.
		{"measured surplus, under the floor", 845 * mib, 592 * mib, 0},
		{"surplus over the floor", 1100 * mib, 592 * mib, 508 * mib},
		{"small surplus is noise", 700 * mib, 592 * mib, 0},
		{"large but under a quarter", 4900 * mib, 4000 * mib, 0},
		{"host below guest", 400 * mib, 592 * mib, 0},
		{"vmmem unread", 0, 592 * mib, 0},
	} {
		if got := vmmemGap(snap(tc.host, tc.guest)); got != tc.want {
			t.Errorf("%s: gap = %d MiB, want %d MiB", tc.name, got/mib, tc.want/mib)
		}
	}
}

// The VM line names all four parts, so what is on screen adds up; and the
// derived row does not claim to know it is all the kernel.
func TestRenderTopItemisesUsedAndLabelsTheRemainder(t *testing.T) {
	var out strings.Builder
	s := fixtureSnapshot(t)
	s.Vmmem = &vmtop.Vmmem{Process: "vmmem", PrivateWorkingSetBytes: s.VM.MemUsedBytes + 400<<20}
	renderTop(&out, s, "")
	text := out.String()
	for _, want := range []string{
		"not itemised by the kernel",
		"kernel and drivers, not charged to any group",
		"Windows holds 400.0 MiB more for the VM than the VM is using",
		"wsl.auto-memory-reclaim",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not charged to any group (kernel)") {
		t.Error("the derived row still claims to be the kernel")
	}
}

// LIMIT is the container's own memory.max, and a dash without one -- never
// the VM total, which docker stats prints as every container's "limit".
func TestRenderTopShowsLimitAndPIDs(t *testing.T) {
	s := fixtureSnapshot(t)
	s.Containers[0].LimitBytes = 256 << 20
	s.Containers[0].PIDs = 7
	var out strings.Builder
	renderTop(&out, s, "")
	text := out.String()
	if !strings.Contains(text, "LIMIT") || !strings.Contains(text, "PIDS") {
		t.Fatalf("header lacks LIMIT or PIDS:\n%s", text)
	}
	var limited, unlimited string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, s.Containers[0].Name+" "):
			limited = line
		case strings.HasPrefix(line, s.Containers[1].Name+" "):
			unlimited = line
		}
	}
	if !strings.Contains(limited, "256.0 MiB") || !strings.Contains(limited, " 7 ") {
		t.Errorf("limited row should show its 256 MiB limit and 7 PIDs: %q", limited)
	}
	if f := strings.Fields(unlimited); len(f) < 4 || f[3] != "-" {
		t.Errorf("unlimited row should show a dash for LIMIT: %q", unlimited)
	}
	if strings.Contains(unlimited, "7.6 GiB") {
		t.Errorf("unlimited row shows the VM total as a limit: %q", unlimited)
	}
}
