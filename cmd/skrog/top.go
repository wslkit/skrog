package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/supervise"
	"github.com/wslkit/skrog/internal/vmtop"
	"github.com/wslkit/skrog/internal/wsl"
	"github.com/wslkit/skrog/internal/wslconfig"
)

// runTop is `skrog top`: where the WSL VM's memory and CPU went (#511).
//
// `docker stats` covers the containers. This covers the VM they run in -- page
// cache, the engine's own daemons, other distros in the same VM, the kernel --
// next to what Windows says Vmmem holds, because Vmmem is the number people
// arrive worried about and a container list cannot explain it.
func runTop(args []string) int {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	asJSON := fs.Bool("json", false, "print one reading as JSON and exit")
	once := fs.Bool("once", false, "print one reading and exit, instead of refreshing")
	noStream := fs.Bool("no-stream", false, "the same as --once, spelled the way docker stats spells it")
	stream := fs.Bool("stream", false, "with --json, keep printing: one JSON object per line, every --interval")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval, and the window CPU is averaged over")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog top [--once | --no-stream] [--json [--stream]] [--interval 2s]

Shows where the WSL VM's memory and CPU go: each running container, the
engine's own daemons, other WSL distros sharing the VM, page cache and the
kernel -- next to what Windows says the Vmmem process holds.

docker stats shows the containers. This shows the VM they run in, which is
what the Vmmem figure in Task Manager is. The usual answer to "why is Vmmem
so big" is page cache from builds and pulls, and only this view can show it.

The table refreshes until Ctrl-C, like docker stats; --once (or --no-stream)
prints one reading. --json prints one reading too, because a script reading it
expects one document; --json --stream prints one object per line, every
--interval, for jq or a log shipper. While the engine is down each line
carries its state and no reading.

CPU is in percent of one CPU, like docker stats. PSI is the share of the last
ten seconds that work spent stalled waiting on that resource.

Never starts the engine: a stopped or idle-stopped engine is reported, and a
refreshing view waits for it. Reads inside the distro, never through the
docker pipe, so leaving it open does not keep the engine from idling.

Exit codes: 0 engine running or idle, %d engine stopped or unreadable, %d usage,
%d not installed. The JSON shape is in docs/cli-json.md.
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *noStream {
		*once = true
	}
	if *stream && *once {
		fmt.Fprintln(os.Stderr, "skrog: --stream keeps printing and --once stops after one; choose one")
		return exitUsage
	}
	if *interval < 500*time.Millisecond {
		fmt.Fprintln(os.Stderr, "skrog: --interval must be at least 500ms")
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	p := &provision.Provisioner{Logger: cliLogger(true)}
	target, ok := resolveEngineTarget(p, opts)
	if !ok {
		if *asJSON {
			printTopJSON(topJSON{Engine: "stopped"})
		} else {
			fmt.Fprintln(os.Stderr, "skrog: no engine is installed; run `skrog install`")
		}
		return exitNotFound
	}

	w := wsl.NewFast()
	defer w.Close()
	engineState := func(ctx context.Context) string {
		// A listing, never an exec: exec boots a stopped distro (#82).
		if ds, err := w.List(ctx); err == nil {
			for _, d := range ds {
				if d.Name == target.Distro && d.Running() {
					return "running"
				}
			}
		}
		if supervise.ReadEngineState(opts.StateDir) == supervise.EngineIdle {
			return "idle"
		}
		return "stopped"
	}
	reader := &vmtop.Reader{WSL: w}
	reclaim := autoMemoryReclaim()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *asJSON && *stream {
		return streamTopJSON(ctx, os.Stdout, *interval, target.Distro, engineState, reader, reclaim)
	}

	if *asJSON || *once {
		out := topJSON{Engine: engineState(ctx), Distro: target.Distro}
		if out.Engine != "running" {
			if *asJSON {
				printTopJSON(out)
			} else {
				fmt.Printf("engine is %s; nothing to measure (skrog top never starts it)\n", out.Engine)
			}
			return engineDownCode(out.Engine)
		}
		snap, err := reader.Read(ctx, target.Distro, *interval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		attachVmmem(&snap)
		out.Reading = &snap
		out.AutoMemoryReclaim = reclaim
		if *asJSON {
			printTopJSON(out)
		} else {
			renderTop(os.Stdout, snap, reclaim)
		}
		return exitOK
	}

	enableVT()
	var prev *vmtop.Sample
	for {
		var frame strings.Builder
		if st := engineState(ctx); st != "running" {
			prev = nil
			fmt.Fprintf(&frame, "engine is %s; waiting for it (skrog top never starts it). Ctrl-C to quit.\n", st)
		} else {
			// The first sample has nothing to average CPU against, so it shows
			// memory now and a dash for CPU until the next refresh.
			snap, s, err := reader.Next(ctx, target.Distro, prev)
			prev = s
			if err != nil {
				fmt.Fprintf(&frame, "could not read the VM: %v\n", err)
			} else {
				attachVmmem(&snap)
				renderTop(&frame, snap, reclaim)
			}
		}
		// Home and clear, then the whole frame in one write, so a slow
		// terminal never shows half of one reading over the last.
		fmt.Print("\x1b[H\x1b[2J" + frame.String())
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(*interval):
		}
	}
}

// streamTopJSON prints one compact JSON object per line, every interval,
// until the context ends (#511).
//
// Each line is a whole topJSON, so a consumer never has to track state across
// lines: while the engine is down a line carries only its state, and the first
// line after it comes back has windowSecs 0 -- no window to average CPU over,
// which is said rather than papered over with a made-up figure.
func streamTopJSON(ctx context.Context, w io.Writer, interval time.Duration, distro string,
	engineState func(context.Context) string, reader *vmtop.Reader, reclaim string) int {
	enc := json.NewEncoder(w)
	var prev *vmtop.Sample
	for {
		out := topJSON{Engine: engineState(ctx), Distro: distro, AutoMemoryReclaim: reclaim}
		if out.Engine != "running" {
			prev = nil
		} else if snap, s, err := reader.Next(ctx, distro, prev); err != nil {
			prev = nil
			if ctx.Err() != nil {
				return exitOK
			}
			out.Error = err.Error()
		} else {
			prev = s
			attachVmmem(&snap)
			out.Reading = &snap
		}
		if err := enc.Encode(out); err != nil {
			// The reader went away (a closed pipe into jq); that is the end.
			return exitOK
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(interval):
		}
	}
}

func engineDownCode(state string) int {
	if state == "idle" {
		return exitOK
	}
	return exitError
}

func attachVmmem(snap *vmtop.Snapshot) {
	if v, err := vmtop.ReadVmmem(); err == nil {
		snap.Vmmem = v
	} else {
		snap.Errors = append(snap.Errors, "vmmem: "+err.Error())
	}
}

// autoMemoryReclaim is what ~/.wslconfig asks WSL to do with idle guest
// memory, or "" when it does not say. Reported rather than a default assumed:
// WSL's own default has changed between releases.
func autoMemoryReclaim() string {
	path, err := wslconfig.Path()
	if err != nil {
		return ""
	}
	f, err := wslconfig.Load(path)
	if err != nil {
		return ""
	}
	v, _ := f.Get("autoMemoryReclaim")
	return strings.TrimSpace(v)
}

func printTopJSON(v topJSON) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// renderTop prints one reading for a human.
func renderTop(w io.Writer, s vmtop.Snapshot, reclaim string) {
	vm := s.VM
	fmt.Fprintf(w, "VM        %d CPUs, CPU %s%%   memory %s used of %s (%s available)\n",
		vm.CPUs, pct(vm.CPUPercent, s.WindowSecs), humanBytes(vm.MemUsedBytes),
		humanBytes(vm.MemTotalBytes), humanBytes(vm.MemAvailableBytes))
	// All four, so the parts add up to "used" on screen too. The last is the
	// share /proc/meminfo does not name, and saying so beats a silent gap.
	fmt.Fprintf(w, "          used is %s processes, %s page cache, %s kernel, %s not itemised by the kernel\n",
		humanBytes(vm.AnonBytes), humanBytes(vm.PageCacheBytes), humanBytes(vm.KernelBytes),
		humanBytes(vm.UnitemisedBytes))
	fmt.Fprintf(w, "          stalled (PSI, last 10s): cpu %.1f%%  memory %.1f%%  io %.1f%%\n",
		vm.Pressure.CPU, vm.Pressure.Memory, vm.Pressure.IO)
	if v := s.Vmmem; v != nil {
		fmt.Fprintf(w, "Windows   %s %s (Task Manager's figure), working set %s, committed %s\n",
			v.Process, humanBytes(v.PrivateWorkingSetBytes), humanBytes(v.WorkingSetBytes), humanBytes(v.PrivateBytes))
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tMEMORY\tLIMIT\tANON\tFILE\tKERNEL\tCPU%\tPIDS\tMEM PSI\tIO PSI\t")
	row := func(name string, g vmtop.Group) {
		// A dash, not the VM total, for no limit: docker stats prints the VM
		// total as every container's "limit", which no one container can hit.
		limit := "-"
		if g.LimitBytes > 0 {
			limit = humanBytes(g.LimitBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%.1f\t%.1f\t\n", name,
			humanBytes(g.MemoryBytes), limit, humanBytes(g.AnonBytes), humanBytes(g.FileBytes),
			humanBytes(g.KernelBytes), pct(g.CPUPercent, s.WindowSecs), g.PIDs, g.Pressure.Memory, g.Pressure.IO)
	}
	for _, c := range s.Containers {
		row(truncName(c.Name, 40), c)
	}
	if len(s.Containers) == 0 {
		fmt.Fprintln(tw, "(no running containers)\t\t\t\t\t\t\t\t\t\t")
	}
	row("engine (dockerd, containerd, build)", s.Engine)
	if s.OtherContainers.MemoryBytes > 0 {
		row("another engine's containers", s.OtherContainers)
	}
	row(fmt.Sprintf("other WSL distros (%d)", s.OtherDistroCount), s.OtherDistros)
	row("WSL itself", s.WSL)
	fmt.Fprintf(tw, "kernel and drivers, not charged to any group\t%s\t\t\t\t\t\t\t\t\t\n", humanBytes(s.UnchargedBytes))
	tw.Flush()

	// The two pieces of advice this view exists to give. Thresholds are a
	// judgement, not a measurement: at least a quarter of what the VM holds,
	// and an absolute floor, is where each stops being noise.
	advised := false
	if vm.MemUsedBytes > 0 && vm.PageCacheBytes >= 1<<30 && vm.PageCacheBytes*4 >= vm.MemUsedBytes {
		fmt.Fprintf(w, "\n%s of the VM's memory is page cache: file data Linux will drop when it needs\n"+
			"the room, but which Windows counts as Vmmem until it is reclaimed.\n", humanBytes(vm.PageCacheBytes))
		advised = true
	}
	if gap := vmmemGap(s); gap > 0 {
		fmt.Fprintf(w, "\nWindows holds %s more for the VM than the VM is using: memory freed inside\n"+
			"the VM that has not been handed back yet.\n", humanBytes(gap))
		advised = true
	}
	if advised {
		if reclaim == "" {
			fmt.Fprintln(w, "~/.wslconfig does not set autoMemoryReclaim; `skrog config set wsl.auto-memory-reclaim gradual`\n"+
				"asks WSL to hand idle memory back. See docs/vm-sizing.md.")
		} else {
			fmt.Fprintf(w, "~/.wslconfig sets autoMemoryReclaim=%s. See docs/vm-sizing.md.\n", reclaim)
		}
	}
	for _, e := range s.Errors {
		fmt.Fprintf(w, "\nnot measured: %s", e)
	}
	if len(s.Errors) > 0 {
		fmt.Fprintln(w)
	}
}

// vmmemGap is how much more Windows' figure for Vmmem is than the VM's own
// "used", when that is worth a line: at least 256 MiB and a quarter of used.
// Zero otherwise, and when Vmmem could not be read.
//
// The two are not the same measure -- one is Windows' private working set,
// the other the guest's total minus free -- so a small difference either way
// is not a finding. A large surplus is: guest pages that are free inside the
// VM and still resident on the host. Hyper-V's balloon reports free pages
// back in 2 MiB chunks (page_reporting_order 9 in the engine's dmesg), which
// is one reason a fragmented free list is not returned; that part is read
// from the log, not measured.
func vmmemGap(s vmtop.Snapshot) uint64 {
	if s.Vmmem == nil || s.VM.MemUsedBytes == 0 {
		return 0
	}
	host, guest := s.Vmmem.PrivateWorkingSetBytes, s.VM.MemUsedBytes
	if host <= guest {
		return 0
	}
	gap := host - guest
	if gap < 256<<20 || gap*4 < guest {
		return 0
	}
	return gap
}

// pct renders a CPU figure, or a dash when there was no window to average
// over: 0% would claim a measurement that was not taken.
func pct(v, window float64) string {
	if window <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

func truncName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}
