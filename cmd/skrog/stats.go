package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wslkit/skrog/internal/enginestats"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/vhdx"
	"github.com/wslkit/skrog/internal/wsl"
	"github.com/wslkit/skrog/internal/wslconfig"
)

// statsDisk adapts internal/vhdx to enginestats.Disk.
type statsDisk struct{}

func (statsDisk) SizeOnDisk(path string) (uint64, error) { return vhdx.SizeOnDisk(path) }
func (statsDisk) Free(path string) (uint64, error)       { return freeSpace(filepath.Dir(path)) }

// gatherStats reads the statistics for `skrog status --stats` (#179).
//
// running is passed in rather than probed here: the caller already knows, and
// status must never boot a stopped distro to find out (#82). A stopped engine
// yields probed=false and no WSL calls at all.
func gatherStats(ctx context.Context, opts provision.Options, distro string, running bool) statsJSON {
	out := statsJSON{}

	// The supervisor's own numbers come from the file it flushes, because it is
	// a different process. The reading is timestamped and reported with its
	// age, so a supervisor that died (#166) shows as stale rather than as
	// silence — and its last numbers are still the most useful thing available.
	if st, ok, err := supervise.ReadStats(opts.StateDir); err == nil && ok {
		out.Supervisor = &supervisorStatsJSON{
			Fresh:           st.Fresh(),
			ReadingAgeSecs:  round1(st.Age().Seconds()),
			StartedAt:       timeOrEmpty(st.Lifecycle.StartedAt),
			UptimeSecs:      sinceSecs(st.Lifecycle.StartedAt),
			EngineStartedAt: timeOrEmpty(st.Lifecycle.EngineStartedAt),
			EngineUptimeSecs: func() float64 {
				if !running {
					return 0
				}
				return sinceSecs(st.Lifecycle.EngineStartedAt)
			}(),
			EngineStarts:   st.Lifecycle.EngineStarts,
			IdleStops:      st.Lifecycle.IdleStops,
			LastIdleStopAt: timeOrEmpty(st.Lifecycle.LastIdleStopAt),
			LastWakeAt:     timeOrEmpty(st.Lifecycle.LastWakeAt),
		}
		out.Bridge = &bridgeStatsJSON{
			Connections:   st.Bridge.Connections,
			BytesToEngine: st.Bridge.BytesToEngine,
			BytesToClient: st.Bridge.BytesToClient,
			ActiveConns:   st.Bridge.ActiveConns,
			Transport:     st.Bridge.Transport,
		}
	}

	if !running {
		return out
	}

	dataDir := opts.DataDir
	p := &provision.Provisioner{Logger: cliLogger(true)}
	if m, err := p.ReadManifest(opts); err == nil && m.DataDir != "" {
		dataDir = m.DataDir
	}
	if dataDir == "" {
		dataDir = filepath.Join(opts.StateDir, "distro")
	}

	configured := map[string]string{}
	if path, err := wslconfig.Path(); err == nil {
		if f, err := wslconfig.Load(path); err == nil {
			configured = f.All()
		}
	}

	// A bound, because `/system/df` walks every layer: on a large engine it is
	// the slow part, and `status` should not hang on it.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	r := &enginestats.Reader{WSL: wsl.NewLocal(), Disk: statsDisk{}, Configured: configured}
	s := r.Read(ctx, distro, filepath.Join(dataDir, "ext4.vhdx"), running)

	out.Probed = s.Probed
	out.Engine = &engineStatsJSON{
		Version:          s.Engine.Version,
		Containers:       s.Engine.Containers,
		Running:          s.Engine.Running,
		Paused:           s.Engine.Paused,
		Stopped:          s.Engine.Stopped,
		Images:           s.Engine.Images,
		Volumes:          s.Engine.Volumes,
		ImagesBytes:      s.Engine.ImagesBytes,
		VolumesBytes:     s.Engine.VolumesBytes,
		BuildCacheBytes:  s.Engine.BuildCacheBytes,
		ReclaimableBytes: s.Engine.ReclaimableBytes,
	}
	out.Disk = &diskStatsJSON{
		Path:             s.Disk.Path,
		SizeOnDiskBytes:  s.Disk.SizeOnDiskBytes,
		GuestUsedBytes:   s.Disk.GuestUsedBytes,
		ReclaimableBytes: s.Disk.ReclaimableBytes,
		HostFreeBytes:    s.Disk.HostFreeBytes,
	}
	out.VM = &vmStatsJSON{
		CPUs:                 s.VM.CPUs,
		MemTotalBytes:        s.VM.MemTotalBytes,
		MemAvailableBytes:    s.VM.MemAvailableBytes,
		SwapTotalBytes:       s.VM.SwapTotalBytes,
		ConfiguredMemory:     s.VM.ConfiguredMemory,
		ConfiguredProcessors: s.VM.ConfiguredProcessors,
	}
	out.Errors = s.Errors
	return out
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func sinceSecs(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return round1(time.Since(t).Seconds())
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

// printStats renders the statistics for a human. Numbers that mean nothing
// without context carry it: a reclaimable figure names the command that would
// act on it, and a degraded transport says what it costs.
func printStats(s statsJSON) {
	if s.Supervisor != nil {
		sup := s.Supervisor
		fmt.Println("\nsupervisor")
		stale := ""
		if !sup.Fresh {
			stale = fmt.Sprintf("  (STALE: last reading %s ago — the supervisor may be gone)",
				time.Duration(sup.ReadingAgeSecs*float64(time.Second)).Round(time.Second))
		}
		fmt.Printf("  uptime            %s%s\n", humanDur(sup.UptimeSecs), stale)
		if sup.EngineUptimeSecs > 0 {
			fmt.Printf("  engine uptime     %s\n", humanDur(sup.EngineUptimeSecs))
		}
		fmt.Printf("  engine starts     %d\n", sup.EngineStarts)
		if sup.IdleStops > 0 {
			fmt.Printf("  idle stops        %d (last %s)\n", sup.IdleStops, ago(sup.LastIdleStopAt))
		} else {
			fmt.Printf("  idle stops        0\n")
		}
	}
	if s.Bridge != nil {
		b := s.Bridge
		fmt.Println("\nbridge")
		fmt.Printf("  connections       %d (%d open)\n", b.Connections, b.ActiveConns)
		fmt.Printf("  relayed           %s in, %s out\n",
			humanBytes(b.BytesToEngine), humanBytes(b.BytesToClient))
		// The one line that explains a slow docker with a healthy engine, so
		// each transport says what it costs rather than just naming itself.
		switch b.Transport {
		case "vsock":
			fmt.Println("  transport         vsock (fast path, ~0.6 ms per connection)")
		case "fallback":
			fmt.Println("  transport         fallback — the socat relay, ~165 ms per connection instead")
			fmt.Println("                    of ~0.6 ms: the vsock agent is unreachable, so docker feels slow")
		case "socat":
			fmt.Println("  transport         socat — the slow path, pinned by SKROG_NO_VSOCK")
		case "":
			fmt.Println("  transport         unknown (no reading from the supervisor yet)")
		default:
			fmt.Printf("  transport         %s\n", b.Transport)
		}
	}

	if !s.Probed {
		fmt.Println("\nengine statistics need a running engine; nothing was started to collect them")
		return
	}
	if e := s.Engine; e != nil {
		fmt.Println("\nengine")
		fmt.Printf("  containers        %d (%d running, %d stopped", e.Containers, e.Running, e.Stopped)
		if e.Paused > 0 {
			fmt.Printf(", %d paused", e.Paused)
		}
		fmt.Println(")")
		fmt.Printf("  images            %d (%s)\n", e.Images, humanBytes(e.ImagesBytes))
		if e.Volumes > 0 {
			fmt.Printf("  volumes           %d (%s)\n", e.Volumes, humanBytes(e.VolumesBytes))
		}
		fmt.Printf("  build cache       %s\n", humanBytes(e.BuildCacheBytes))
		if e.ReclaimableBytes > 0 {
			fmt.Printf("  reclaimable       %s  (`skrog prune`)\n", humanBytes(e.ReclaimableBytes))
		}
	}
	if d := s.Disk; d != nil && d.SizeOnDiskBytes > 0 {
		fmt.Println("\ndisk")
		fmt.Printf("  virtual disk      %s on disk", humanBytes(d.SizeOnDiskBytes))
		if d.GuestUsedBytes > 0 {
			fmt.Printf(", %s used inside", humanBytes(d.GuestUsedBytes))
		}
		fmt.Println()
		if d.ReclaimableBytes > 0 {
			fmt.Printf("  reclaimable       ~%s  (`skrog compact`)\n", humanBytes(d.ReclaimableBytes))
		}
		if d.HostFreeBytes > 0 {
			fmt.Printf("  host free         %s\n", humanBytes(d.HostFreeBytes))
		}
	}
	if v := s.VM; v != nil && v.MemTotalBytes > 0 {
		fmt.Println("\nvm")
		fmt.Printf("  memory            %s total", humanBytes(v.MemTotalBytes))
		if v.MemAvailableBytes > 0 {
			fmt.Printf(", %s available", humanBytes(v.MemAvailableBytes))
		}
		if v.ConfiguredMemory != "" {
			fmt.Printf("  (configured: %s)", v.ConfiguredMemory)
		}
		fmt.Println()
		fmt.Printf("  cpus              %d", v.CPUs)
		if v.ConfiguredProcessors != "" {
			fmt.Printf("  (configured: %s)", v.ConfiguredProcessors)
		}
		fmt.Println()
		if v.ConfiguredMemory == "" && v.ConfiguredProcessors == "" {
			fmt.Println("                    no ~/.wslconfig sizing; WSL's defaults apply (`skrog wsl-config`)")
		}
	}
	for _, e := range s.Errors {
		fmt.Printf("  ! %s\n", e)
	}
}

// humanDur renders a duration in seconds the way uptime is usually read.
func humanDur(secs float64) string {
	d := time.Duration(secs * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// ago renders an RFC3339 timestamp as an interval, which is what a reader
// actually wants from "when did it last idle-stop".
func ago(ts string) string {
	if ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return humanDur(time.Since(t).Seconds()) + " ago"
}
