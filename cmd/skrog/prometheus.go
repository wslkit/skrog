package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Prometheus text exposition for `skrog status --prometheus` (#390).
//
// The output is the node_exporter *textfile* format: a scheduled task writes it
// into the collector's directory and node_exporter picks it up. That is why
// there is no listener here — a resident HTTP endpoint would be a new network
// surface on a product whose security doc is careful about exactly that, and
// the textfile route needs no port, no ACL and no daemon.
//
// This is not telemetry. Nothing leaves the machine unless its owner points
// their own Prometheus at it; the no-telemetry promise is about what *we*
// collect, not about what an operator may measure on their own hardware.

// metric is one sample: a name, optional labels, and a value. Collected as data
// so the writer stays a formatter and the choices stay testable.
type metric struct {
	name   string
	help   string
	typ    string // "gauge" or "counter"
	labels map[string]string
	value  float64
}

// writePrometheus renders a status reading as Prometheus text.
//
// Every metric carries HELP and TYPE, emitted once per name even when a metric
// has several label sets, because a textfile with bare samples is legal but
// unreadable in a dashboard six months later.
func writePrometheus(w io.Writer, st statusJSON, appVersion string) error {
	ms := collectMetrics(st, appVersion)

	// Group by name, preserving first-seen order, so the HELP/TYPE header is
	// written once and its samples follow it.
	var order []string
	byName := map[string][]metric{}
	for _, m := range ms {
		if _, seen := byName[m.name]; !seen {
			order = append(order, m.name)
		}
		byName[m.name] = append(byName[m.name], m)
	}

	var b strings.Builder
	for _, name := range order {
		group := byName[name]
		fmt.Fprintf(&b, "# HELP %s %s\n", name, group[0].help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, group[0].typ)
		for _, m := range group {
			fmt.Fprintf(&b, "%s%s %s\n", m.name, formatLabels(m.labels), formatValue(m.value))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// formatLabels renders a label set, sorted so the output is byte-stable across
// runs: an unstable textfile churns diffs and makes a change look like news.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// Not %q: escapeLabel has already applied the exposition format's
		// escaping, and %q would escape the backslashes it just added.
		parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel escapes the three characters the exposition format reserves in a
// label value. Values here are versions, pipe names and paths — a Windows path
// is full of backslashes, so this is not theoretical.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}

// formatValue prints a float without a trailing ".0" for whole numbers, since
// nearly every metric here is a count.
func formatValue(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// enumStates writes one sample per possible state with 1 on the live one, the
// convention node_exporter uses for unit states. A single gauge holding an
// opaque integer would force every dashboard to carry the mapping.
//
// label is the dimension's name -- "state" for the engine, "transport" for the
// bridge -- because a label called "state" holding "vsock" reads as a mistake.
func enumStates(name, help, label string, states []string, actual string) []metric {
	out := make([]metric, 0, len(states))
	for _, s := range states {
		out = append(out, metric{
			name: name, help: help, typ: "gauge",
			labels: map[string]string{label: s},
			value:  b2f(s == actual),
		})
	}
	return out
}

// collectMetrics is the whole mapping from a status reading to samples, pure so
// the shape can be tested without a machine.
func collectMetrics(st statusJSON, appVersion string) []metric {
	var ms []metric
	add := func(name, help, typ string, value float64) {
		ms = append(ms, metric{name: name, help: help, typ: typ, value: value})
	}

	// An info metric is how a fleet answers "am I current?" in one query:
	// `skrog upgrade` answers it for one machine, this answers it for fifty.
	info := map[string]string{"version": appVersion}
	if st.Backend != "" {
		info["backend"] = st.Backend
	}
	if st.Distro != "" {
		info["distro"] = st.Distro
	}
	if st.Stats != nil && st.Stats.Engine != nil && st.Stats.Engine.Version != "" {
		info["engine_version"] = st.Stats.Engine.Version
	}
	ms = append(ms, metric{
		name: "skrog_build_info", typ: "gauge",
		help:   "Skrog build and engine identity; always 1, read the labels.",
		labels: info, value: 1,
	})

	add("skrog_installed", "1 when an engine is installed on this host.", "gauge", b2f(st.Installed))
	if !st.Installed {
		// Nothing below is meaningful without an install, and emitting zeros
		// would make "not installed" and "installed but empty" identical.
		return ms
	}

	ms = append(ms, enumStates("skrog_engine_state",
		"Engine state; 1 on the live one. idle means idle-stopped on purpose, not broken.",
		"state", []string{"running", "idle", "stopped"}, st.Engine)...)
	ms = append(ms, enumStates("skrog_desired_state",
		"The engine state the operator last asked for.",
		"state", []string{"running", "stopped"}, st.Desired)...)
	add("skrog_supervisor_running", "1 when a supervisor holds the single-instance lock.", "gauge",
		b2f(st.Supervisor == "running"))
	add("skrog_gpu_enabled", "1 when GPU passthrough is configured.", "gauge", b2f(st.GPU.Enabled))

	s := st.Stats
	if s == nil {
		return ms
	}
	add("skrog_stats_probed",
		"1 when the engine answered, so engine/disk/vm metrics below are present.", "gauge", b2f(s.Probed))

	if sup := s.Supervisor; sup != nil {
		add("skrog_supervisor_reading_fresh",
			"1 when the supervisor's flushed reading is recent; 0 means its numbers are stale.",
			"gauge", b2f(sup.Fresh))
		add("skrog_supervisor_reading_age_seconds",
			"Age of the supervisor's flushed reading.", "gauge", sup.ReadingAgeSecs)
		add("skrog_supervisor_uptime_seconds", "Supervisor uptime.", "gauge", sup.UptimeSecs)
		add("skrog_engine_uptime_seconds", "Engine uptime; 0 when it is not running.", "gauge", sup.EngineUptimeSecs)
		add("skrog_engine_starts_total", "Engine starts since the supervisor started.", "counter", float64(sup.EngineStarts))
		add("skrog_idle_stops_total", "Times the idle timeout stopped the engine.", "counter", float64(sup.IdleStops))
	}

	if br := s.Bridge; br != nil {
		add("skrog_bridge_connections_total", "Connections the pipe has accepted.", "counter", float64(br.Connections))
		ms = append(ms,
			metric{name: "skrog_bridge_bytes_total", typ: "counter",
				help:   "Bytes relayed across the bridge, by direction.",
				labels: map[string]string{"direction": "to_engine"}, value: float64(br.BytesToEngine)},
			metric{name: "skrog_bridge_bytes_total", typ: "counter",
				help:   "Bytes relayed across the bridge, by direction.",
				labels: map[string]string{"direction": "to_client"}, value: float64(br.BytesToClient)},
		)
		add("skrog_bridge_active_connections", "Connections currently open.", "gauge", float64(br.ActiveConns))
		// The one number that explains a slow docker with a healthy engine:
		// fallback is ~165 ms per connection against vsock's ~0.6 ms.
		ms = append(ms, enumStates("skrog_bridge_transport",
			"Live bridge transport; 1 on the active one. fallback is the slow path.",
			"transport", []string{"vsock", "fallback", "direct"}, br.Transport)...)
	}

	if e := s.Engine; e != nil {
		// One series per state rather than three metric names, so a dashboard
		// can sum containers without knowing the state list.
		for _, c := range []struct {
			state string
			n     int
		}{{"running", e.Running}, {"paused", e.Paused}, {"stopped", e.Stopped}} {
			ms = append(ms, metric{
				name: "skrog_containers", typ: "gauge", help: "Containers by state.",
				labels: map[string]string{"state": c.state}, value: float64(c.n),
			})
		}
		add("skrog_images", "Images in the engine's store.", "gauge", float64(e.Images))
		add("skrog_volumes", "Volumes in the engine's store.", "gauge", float64(e.Volumes))
		add("skrog_images_bytes", "Bytes held by images.", "gauge", float64(e.ImagesBytes))
		add("skrog_volumes_bytes", "Bytes held by volumes.", "gauge", float64(e.VolumesBytes))
		add("skrog_build_cache_bytes", "Bytes held by the BuildKit cache.", "gauge", float64(e.BuildCacheBytes))
		add("skrog_engine_reclaimable_bytes",
			"What the engine says `docker system prune` could free.", "gauge", float64(e.ReclaimableBytes))
	}

	if d := s.Disk; d != nil {
		add("skrog_vhdx_size_bytes", "Size the engine's .vhdx occupies on the host volume.", "gauge",
			float64(d.SizeOnDiskBytes))
		add("skrog_vhdx_guest_used_bytes", "Bytes the filesystem inside the .vhdx uses.", "gauge",
			float64(d.GuestUsedBytes))
		add("skrog_vhdx_reclaimable_bytes",
			"Roughly what `skrog compact` could return; an estimate, since compaction works in blocks.",
			"gauge", float64(d.ReclaimableBytes))
		add("skrog_host_free_bytes", "Free space on the volume holding the engine's data.", "gauge",
			float64(d.HostFreeBytes))
	}

	if v := s.VM; v != nil {
		add("skrog_vm_cpus", "CPUs the WSL2 VM sees.", "gauge", float64(v.CPUs))
		add("skrog_vm_memory_total_bytes", "Memory the WSL2 VM sees.", "gauge", float64(v.MemTotalBytes))
		add("skrog_vm_memory_available_bytes", "Memory available inside the WSL2 VM.", "gauge",
			float64(v.MemAvailableBytes))
		add("skrog_vm_swap_total_bytes", "Swap the WSL2 VM sees.", "gauge", float64(v.SwapTotalBytes))
	}

	return ms
}
