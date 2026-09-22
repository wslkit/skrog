// Package vmtop answers "where did the WSL VM's memory and CPU go?" (#511).
//
// `docker stats` answers it for containers and nothing else. The number people
// actually worry about is Vmmem, and most of Vmmem is usually NOT a container:
// it is page cache from builds and pulls, the engine's own daemons, other
// distros sharing the same VM, and the kernel. This package reads all of those
// from one place -- cgroup v2 and procfs inside the engine distro -- and puts
// them next to what Windows says Vmmem holds.
//
// The cgroup hierarchy the engine distro sees is the VM's: /sys/fs/cgroup has
// docker/ (every container, as its own group) and wsl-user/ (one group per
// running distro, plus non-distro for WSL's own processes). That is what makes
// "other distros" a measurement here rather than a subtraction.
//
// Two rules this package inherits and keeps:
//
//   - it never starts the engine (#82): the caller checks the distro is
//     running before calling Read, and a stopped engine is simply reported;
//   - it never counts as engine activity (#496): everything is read inside the
//     distro, and the one engine API call goes to the unix socket there, never
//     through the pipe the bridge counts.
package vmtop

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/enginestats"
)

// Distro runs commands inside the engine distro.
type Distro interface {
	Exec(ctx context.Context, distro, user string, args ...string) (string, error)
}

// Pressure is PSI "some" avg10, in percent: the share of the last ten seconds
// in which at least one task was stalled waiting on the resource. It is the
// number that says whether a resource is actually short, where usage alone
// only says it is used.
type Pressure struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	IO     float64 `json:"io"`
}

// Group is one cgroup: a container, the engine distro, or an aggregate.
type Group struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// MemoryBytes is memory.current: everything charged to the group,
	// including the page cache its reads and writes pulled in.
	MemoryBytes uint64 `json:"memoryBytes"`
	AnonBytes   uint64 `json:"anonBytes"`
	FileBytes   uint64 `json:"fileBytes"`
	KernelBytes uint64 `json:"kernelBytes"`
	// CPUPercent is of ONE CPU, like `docker stats`, so a group using two
	// full cores reads 200.
	CPUPercent   float64  `json:"cpuPercent"`
	IOReadBytes  uint64   `json:"ioReadBytes"`
	IOWriteBytes uint64   `json:"ioWriteBytes"`
	Pressure     Pressure `json:"pressure"`
}

// VM is the utility VM as its kernel sees it.
type VM struct {
	CPUs int `json:"cpus"`
	// CPUPercent is of one CPU, like the groups: a busy 4-CPU VM reads 400.
	CPUPercent        float64 `json:"cpuPercent"`
	MemTotalBytes     uint64  `json:"memTotalBytes"`
	MemFreeBytes      uint64  `json:"memFreeBytes"`
	MemAvailableBytes uint64  `json:"memAvailableBytes"`
	// MemUsedBytes is total minus free: what the VM holds, cache included.
	// That, not "used by processes", is what Windows has to back.
	MemUsedBytes uint64 `json:"memUsedBytes"`
	// AnonBytes is process memory that is not file-backed (heaps, stacks).
	AnonBytes uint64 `json:"anonBytes"`
	// PageCacheBytes is Buffers + Cached: file data kept in memory, tmpfs
	// included. Given back under pressure inside the VM -- but held, as far as
	// Windows can tell, until something reclaims it.
	PageCacheBytes uint64 `json:"pageCacheBytes"`
	// KernelBytes is Slab + KernelStack + PageTables, an approximation.
	KernelBytes uint64   `json:"kernelBytes"`
	Pressure    Pressure `json:"pressure"`
}

// Snapshot is one reading.
type Snapshot struct {
	TakenAt time.Time `json:"takenAt"`
	// WindowSecs is how long the CPU figures were averaged over.
	WindowSecs float64 `json:"windowSecs"`
	VM         VM      `json:"vm"`
	// Containers are the engine's running containers, heaviest first.
	Containers []Group `json:"containers"`
	// Engine is the engine distro's own group: dockerd, containerd, the
	// shims, buildkit, the agent -- and the page cache they pulled in, which
	// is where image pulls and builds land.
	Engine Group `json:"engine"`
	// OtherDistros is every other running WSL distro, together, and
	// OtherDistroCount how many there are. Their names are not visible from
	// inside the VM.
	OtherDistros     Group `json:"otherDistros"`
	OtherDistroCount int   `json:"otherDistroCount"`
	// WSL is WSL's own processes in the VM (the wsl-user/non-distro group).
	WSL Group `json:"wsl"`
	// OtherContainers is container groups that are not this engine's running
	// containers but still hold memory -- another engine in the same VM.
	OtherContainers Group `json:"otherContainers"`
	// UnchargedBytes is used memory no group accounts for: mostly the
	// kernel's own. Derived (used minus every top-level group), not measured.
	UnchargedBytes uint64 `json:"unchargedBytes"`
	// Vmmem is the Windows side, filled in by the caller (ReadVmmem): this
	// package's Read only sees the guest. Absent when it could not be read.
	Vmmem *Vmmem `json:"vmmem,omitempty"`
	// Errors are the parts that could not be read. A partial answer is still
	// an answer.
	Errors []string `json:"errors,omitempty"`
}

// Reader takes snapshots.
type Reader struct {
	WSL Distro
}

// Read takes two samples Window apart and reports the second, with CPU
// averaged across the gap. The caller must know the distro is running.
func (r *Reader) Read(ctx context.Context, distro string, window time.Duration) (Snapshot, error) {
	a, err := r.sample(ctx, distro)
	if err != nil {
		return Snapshot{}, err
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-time.After(window):
	}
	b, err := r.sample(ctx, distro)
	if err != nil {
		return Snapshot{}, err
	}
	return Compute(a, b), nil
}

// Next is Read for a refreshing view: it samples once and computes against
// the previous sample, so each refresh costs one exec rather than two.
func (r *Reader) Next(ctx context.Context, distro string, prev *Sample) (Snapshot, *Sample, error) {
	s, err := r.sample(ctx, distro)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if prev == nil {
		return Compute(s, s), s, nil
	}
	return Compute(prev, s), s, nil
}

func (r *Reader) sample(ctx context.Context, distro string) (*Sample, error) {
	out, err := r.WSL.Exec(ctx, distro, "root", "sh", "-c", script)
	if err != nil {
		return nil, fmt.Errorf("reading the VM: %w", err)
	}
	s := ParseSample(out)
	s.At = time.Now()
	return s, nil
}

// script prints every source in one exec, as `==name` sections. One exec per
// sample is the cost that matters: a wsl.exe round trip is ~0.2 s (measured on
// the maintainer's machine, 2026-09-22), and the reads themselves are
// microseconds.
//
// Trailing slashes on the globs match directories only; the memory.current
// test skips a glob that matched nothing.
const script = `echo '==meminfo'; cat /proc/meminfo
echo '==stat'; head -n1 /proc/stat
echo '==nproc'; nproc
echo '==self'; cat /proc/self/cgroup
for r in cpu memory io; do echo "==psi $r"; cat "/proc/pressure/$r" 2>/dev/null; done
for d in /sys/fs/cgroup/*/ /sys/fs/cgroup/docker/*/ /sys/fs/cgroup/wsl-user/*/; do
  [ -f "${d}memory.current" ] || continue
  echo "==cg ${d#/sys/fs/cgroup/}"
  echo "memory.current $(cat "${d}memory.current")"
  grep -E '^(anon|file|kernel) ' "${d}memory.stat" 2>/dev/null
  grep -E '^usage_usec ' "${d}cpu.stat" 2>/dev/null
  sed 's/^/io /' "${d}io.stat" 2>/dev/null
  for r in cpu memory io; do sed -n "s/^some /psi.$r /p" "${d}$r.pressure" 2>/dev/null; done
done
echo '==api'
printf 'GET /v1.44/containers/json HTTP/1.0\r\nHost: docker\r\n\r\n' | socat -t 5 - UNIX-CONNECT:/var/run/docker.sock,shut-none`

// Sample is one raw reading. Exported so a refreshing caller can hold the
// previous one; its fields are not a contract.
type Sample struct {
	At       time.Time
	meminfo  map[string]uint64 // bytes
	cpuBusy  uint64            // jiffies
	cpuTotal uint64            // jiffies
	ncpu     int
	self     string // this distro's group, e.g. wsl-user/distro-3271
	psi      Pressure
	groups   map[string]*rawGroup
	running  map[string]string // container ID -> name
	errors   []string
}

type rawGroup struct {
	mem, anon, file, kernel uint64
	cpuUsec                 uint64
	ioR, ioW                uint64
	psi                     Pressure
}

// ParseSample reads script's output.
func ParseSample(out string) *Sample {
	s := &Sample{meminfo: map[string]uint64{}, groups: map[string]*rawGroup{}}
	sections := splitSections(out)

	for _, line := range strings.Split(sections["meminfo"], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			mult := uint64(1)
			if len(f) > 2 && f[2] == "kB" {
				mult = 1024
			}
			s.meminfo[strings.TrimSuffix(f[0], ":")] = n * mult
		}
	}
	if len(s.meminfo) == 0 {
		s.errors = append(s.errors, "could not read /proc/meminfo")
	}

	// cpu user nice system idle iowait irq softirq steal ...
	if f := strings.Fields(sections["stat"]); len(f) > 5 && f[0] == "cpu" {
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			// guest and guest_nice (the 9th and 10th) are already in user.
			if i >= 8 {
				break
			}
			s.cpuTotal += n
			if i != 3 && i != 4 { // idle, iowait
				s.cpuBusy += n
			}
		}
	}
	s.ncpu, _ = strconv.Atoi(strings.TrimSpace(sections["nproc"]))

	for _, line := range strings.Split(sections["self"], "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::/"); ok {
			s.self = rest
		}
	}

	s.psi = Pressure{
		CPU:    someAvg10(sections["psi cpu"]),
		Memory: someAvg10(sections["psi memory"]),
		IO:     someAvg10(sections["psi io"]),
	}

	for name, body := range sections {
		path, ok := strings.CutPrefix(name, "cg ")
		if !ok {
			continue
		}
		s.groups[strings.Trim(path, "/")] = parseGroup(body)
	}

	if body, err := enginestats.HTTPBody(sections["api"]); err != nil {
		s.errors = append(s.errors, "container names: "+err.Error())
	} else {
		var list []struct {
			ID    string   `json:"Id"`
			Names []string `json:"Names"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			s.errors = append(s.errors, "container names: "+err.Error())
		} else {
			s.running = map[string]string{}
			for _, c := range list {
				name := c.ID
				if len(name) > 12 {
					name = name[:12]
				}
				if len(c.Names) > 0 {
					name = strings.TrimPrefix(c.Names[0], "/")
				}
				s.running[c.ID] = name
			}
		}
	}
	return s
}

// splitSections cuts `==name` sections. The api section runs to the end: it
// is an HTTP response, and its body is not line-oriented.
func splitSections(out string) map[string]string {
	res := map[string]string{}
	out = strings.ReplaceAll(out, "\r\n", "\n")
	var name string
	var body strings.Builder
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "=="); ok {
			if name != "" {
				res[name] = body.String()
			}
			name, body = rest, strings.Builder{}
			if name == "api" {
				res[name] = strings.Join(lines[i+1:], "\n")
				return res
			}
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	if name != "" {
		res[name] = body.String()
	}
	return res
}

func parseGroup(body string) *rawGroup {
	g := &rawGroup{}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		num := func() uint64 { n, _ := strconv.ParseUint(f[1], 10, 64); return n }
		switch f[0] {
		case "memory.current":
			g.mem = num()
		case "anon":
			g.anon = num()
		case "file":
			g.file = num()
		case "kernel":
			g.kernel = num()
		case "usage_usec":
			g.cpuUsec = num()
		case "io":
			// io MAJ:MIN rbytes=N wbytes=N ...
			for _, kv := range f[2:] {
				k, v, _ := strings.Cut(kv, "=")
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "rbytes":
					g.ioR += n
				case "wbytes":
					g.ioW += n
				}
			}
		case "psi.cpu":
			g.psi.CPU = avg10(f[1:])
		case "psi.memory":
			g.psi.Memory = avg10(f[1:])
		case "psi.io":
			g.psi.IO = avg10(f[1:])
		}
	}
	return g
}

// someAvg10 reads avg10 from the "some" line of a /proc/pressure file.
func someAvg10(body string) float64 {
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "some" {
			return avg10(f[1:])
		}
	}
	return 0
}

func avg10(fields []string) float64 {
	for _, kv := range fields {
		if v, ok := strings.CutPrefix(kv, "avg10="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			return n
		}
	}
	return 0
}

// Compute turns two samples into a snapshot. With a == b the CPU figures are
// zero, which is the honest answer for a single reading.
func Compute(a, b *Sample) Snapshot {
	// UTC, like every other timestamp in the --json contract.
	snap := Snapshot{TakenAt: b.At.UTC(), Errors: b.errors}
	elapsed := b.At.Sub(a.At)
	snap.WindowSecs = round1(elapsed.Seconds())

	m := b.meminfo
	vm := VM{
		CPUs:              b.ncpu,
		MemTotalBytes:     m["MemTotal"],
		MemFreeBytes:      m["MemFree"],
		MemAvailableBytes: m["MemAvailable"],
		AnonBytes:         m["AnonPages"],
		PageCacheBytes:    m["Buffers"] + m["Cached"],
		KernelBytes:       m["Slab"] + m["KernelStack"] + m["PageTables"],
		Pressure:          b.psi,
	}
	if vm.MemTotalBytes > vm.MemFreeBytes {
		vm.MemUsedBytes = vm.MemTotalBytes - vm.MemFreeBytes
	}
	if dt := b.cpuTotal - a.cpuTotal; b.cpuTotal > a.cpuTotal && b.ncpu > 0 {
		vm.CPUPercent = round1(float64(b.cpuBusy-a.cpuBusy) / float64(dt) * 100 * float64(b.ncpu))
	}
	snap.VM = vm

	group := func(path, id, name string) Group {
		g := b.groups[path]
		if g == nil {
			return Group{ID: id, Name: name}
		}
		out := Group{
			ID: id, Name: name,
			MemoryBytes: g.mem, AnonBytes: g.anon, FileBytes: g.file, KernelBytes: g.kernel,
			IOReadBytes: g.ioR, IOWriteBytes: g.ioW, Pressure: g.psi,
		}
		if prev := a.groups[path]; prev != nil && elapsed > 0 && g.cpuUsec >= prev.cpuUsec {
			out.CPUPercent = round1(float64(g.cpuUsec-prev.cpuUsec) / float64(elapsed.Microseconds()) * 100)
		}
		return out
	}
	add := func(dst *Group, g Group) {
		dst.MemoryBytes += g.MemoryBytes
		dst.AnonBytes += g.AnonBytes
		dst.FileBytes += g.FileBytes
		dst.KernelBytes += g.KernelBytes
		dst.CPUPercent = round1(dst.CPUPercent + g.CPUPercent)
		dst.IOReadBytes += g.IOReadBytes
		dst.IOWriteBytes += g.IOWriteBytes
	}

	snap.Engine = group(b.self, "", "engine")
	snap.WSL = group("wsl-user/non-distro", "", "wsl")
	snap.OtherDistros.Name = "other distros"
	snap.OtherContainers.Name = "other containers"

	var topLevel uint64
	for path := range b.groups {
		parent, child, nested := strings.Cut(path, "/")
		switch {
		case !nested:
			topLevel += b.groups[path].mem
		case parent == "docker":
			if name, ok := b.running[child]; ok {
				snap.Containers = append(snap.Containers, group(path, child, name))
			} else if b.groups[path].mem > 0 {
				// A dead container's group lingers with nothing in it; one
				// that still holds memory belongs to something else.
				add(&snap.OtherContainers, group(path, child, ""))
			}
		case parent == "wsl-user" && path != b.self && child != "non-distro":
			snap.OtherDistroCount++
			add(&snap.OtherDistros, group(path, "", ""))
		}
	}
	if vm.MemUsedBytes > topLevel {
		snap.UnchargedBytes = vm.MemUsedBytes - topLevel
	}
	sort.Slice(snap.Containers, func(i, j int) bool {
		if snap.Containers[i].MemoryBytes != snap.Containers[j].MemoryBytes {
			return snap.Containers[i].MemoryBytes > snap.Containers[j].MemoryBytes
		}
		return snap.Containers[i].Name < snap.Containers[j].Name
	})
	return snap
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
