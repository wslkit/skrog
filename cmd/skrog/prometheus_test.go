package main

import (
	"strings"
	"testing"
)

func renderProm(t *testing.T, st statusJSON) string {
	t.Helper()
	var b strings.Builder
	if err := writePrometheus(&b, st, "0.5.1"); err != nil {
		t.Fatalf("writePrometheus: %v", err)
	}
	return b.String()
}

// A sample line must appear exactly, labels and all, or a dashboard silently
// charts nothing.
func wantLine(t *testing.T, out, line string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if l == line {
			return
		}
	}
	t.Errorf("missing line:\n  %s\ngot:\n%s", line, out)
}

func TestPrometheusNotInstalled(t *testing.T) {
	out := renderProm(t, statusJSON{Installed: false})

	wantLine(t, out, "skrog_installed 0")
	// "No engine here" is a fact a fleet wants, so build_info still goes out.
	if !strings.Contains(out, `skrog_build_info{version="0.5.1"} 1`) {
		t.Errorf("build_info missing on an uninstalled host:\n%s", out)
	}
	// Emitting zeros for engine state would make "not installed" and
	// "installed but stopped" indistinguishable, which is the whole point.
	if strings.Contains(out, "skrog_engine_state") {
		t.Errorf("engine state emitted with no install:\n%s", out)
	}
}

func TestPrometheusEngineStateIsAnEnum(t *testing.T) {
	out := renderProm(t, statusJSON{Installed: true, Engine: "idle", Desired: "running"})

	// Every state present, 1 on the live one: a dashboard should not have to
	// carry a mapping from an opaque integer.
	wantLine(t, out, `skrog_engine_state{state="running"} 0`)
	wantLine(t, out, `skrog_engine_state{state="idle"} 1`)
	wantLine(t, out, `skrog_engine_state{state="stopped"} 0`)
	wantLine(t, out, `skrog_desired_state{state="running"} 1`)
}

func TestPrometheusFullReading(t *testing.T) {
	st := statusJSON{
		Installed: true, Backend: "distro", Distro: "skrog-engine",
		Engine: "running", Desired: "running", Supervisor: "running",
		GPU: gpuJSON{Enabled: true},
		Stats: &statsJSON{
			Probed:     true,
			Supervisor: &supervisorStatsJSON{Fresh: true, UptimeSecs: 3600, EngineStarts: 2, IdleStops: 1},
			Bridge: &bridgeStatsJSON{
				Connections: 42, BytesToEngine: 1024, BytesToClient: 2048,
				ActiveConns: 3, Transport: "vsock",
			},
			Engine: &engineStatsJSON{
				Version: "29.8.1", Running: 2, Paused: 0, Stopped: 5,
				Images: 17, Volumes: 4, ReclaimableBytes: 999,
			},
			Disk: &diskStatsJSON{SizeOnDiskBytes: 8 << 30, HostFreeBytes: 100 << 30},
			VM:   &vmStatsJSON{CPUs: 4, MemTotalBytes: 8 << 30},
		},
	}
	out := renderProm(t, st)

	wantLine(t, out, "skrog_installed 1")
	wantLine(t, out, "skrog_supervisor_running 1")
	wantLine(t, out, `skrog_containers{state="running"} 2`)
	wantLine(t, out, `skrog_containers{state="stopped"} 5`)
	wantLine(t, out, "skrog_images 17")
	wantLine(t, out, "skrog_bridge_connections_total 42")
	wantLine(t, out, `skrog_bridge_bytes_total{direction="to_engine"} 1024`)
	wantLine(t, out, `skrog_bridge_bytes_total{direction="to_client"} 2048`)
	wantLine(t, out, `skrog_bridge_transport{transport="vsock"} 1`)
	wantLine(t, out, `skrog_bridge_transport{transport="fallback"} 0`)
	wantLine(t, out, "skrog_vm_cpus 4")
	wantLine(t, out, "skrog_engine_starts_total 2")

	// The engine version belongs on build_info, not in a metric name.
	if !strings.Contains(out, `engine_version="29.8.1"`) {
		t.Errorf("engine version not on build_info:\n%s", out)
	}
}

// Every metric name carries HELP and TYPE exactly once, even with several
// label sets. A textfile of bare samples is legal and unreadable later.
func TestPrometheusHeadersOncePerName(t *testing.T) {
	out := renderProm(t, statusJSON{
		Installed: true, Engine: "running",
		Stats: &statsJSON{Bridge: &bridgeStatsJSON{Transport: "vsock"}},
	})

	if got := strings.Count(out, "# TYPE skrog_bridge_transport "); got != 1 {
		t.Errorf("TYPE for a 3-sample metric appeared %d times, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, "# HELP skrog_engine_state "); got != 1 {
		t.Errorf("HELP for a 3-sample metric appeared %d times, want 1:\n%s", got, out)
	}
	// Each name's header must precede its samples.
	for _, name := range []string{"skrog_installed", "skrog_engine_state"} {
		h := strings.Index(out, "# HELP "+name+" ")
		s := strings.Index(out, "\n"+name)
		if h < 0 || s < 0 || h > s {
			t.Errorf("%s: header at %d does not precede sample at %d", name, h, s)
		}
	}
}

// A Windows path or pipe name is full of backslashes, and an unescaped one
// makes the whole file unparseable.
func TestPrometheusEscapesLabelValues(t *testing.T) {
	out := renderProm(t, statusJSON{Installed: true, Distro: `weird\name"quoted`})
	wantLine(t, out, `skrog_build_info{distro="weird\\name\"quoted",version="0.5.1"} 1`)
}

// Labels are sorted so the file is byte-stable between runs; otherwise every
// write churns a diff and a no-op looks like news.
func TestPrometheusLabelsAreSorted(t *testing.T) {
	got := formatLabels(map[string]string{"zeta": "1", "alpha": "2", "mid": "3"})
	if want := `{alpha="2",mid="3",zeta="1"}`; got != want {
		t.Errorf("formatLabels = %s, want %s", got, want)
	}
}

// Counts must not render as 1e+06 or 3.0 — a whole number prints whole.
func TestPrometheusFormatsWholeNumbersPlainly(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{{0, "0"}, {3, "3"}, {1_000_000, "1000000"}, {8 << 30, "8589934592"}, {1.5, "1.5"}} {
		if got := formatValue(tc.in); got != tc.want {
			t.Errorf("formatValue(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
