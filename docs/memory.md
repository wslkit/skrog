# Where the VM's memory goes: `skrog top`

Task Manager says Vmmem is using 6 GB. `docker stats` says your containers are
using 800 MB. Neither is lying, and neither explains the other.
`skrog top` does: it puts every part of the WSL VM's memory and CPU on one
screen, next to what Windows says Vmmem holds.

```
skrog top                 # refreshes every 2 s; Ctrl-C to quit
skrog top --once          # one reading, then exit
skrog top --no-stream     # the same, as docker stats spells it
skrog top --json          # one reading as JSON (docs/cli-json.md)
skrog top --json --stream # one JSON object per line, every --interval
skrog top --interval 5s   # slower refresh; also the CPU averaging window
```

The table refreshes like `docker stats`. `--json` is one reading unless you
add `--stream`, because a script reading it expects one document; with
`--stream` each line is a complete object, ready for `jq` or a log shipper.

## What it shows

From the reference host, with two small containers running:

```
VM        4 CPUs, CPU 8.7%   memory 558.4 MiB used of 7.6 GiB (7.1 GiB available)
          used is 76.0 MiB processes, 161.6 MiB page cache, 76.2 MiB kernel, 244.6 MiB not itemised by the kernel
          stalled (PSI, last 10s): cpu 0.0%  memory 0.0%  io 0.0%
Windows   vmmem 846.8 MiB (Task Manager's figure), working set 846.8 MiB, committed 848.6 MiB

                                              MEMORY     ANON       FILE       KERNEL     CPU%  MEM PSI  IO PSI
web                                           9.1 MiB    3.8 MiB    4.0 MiB    836.0 KiB  0.0   0.0      0.0
u1                                            6.2 MiB    1.2 MiB    4.4 MiB    336.0 KiB  0.0   0.0      0.0
engine (dockerd, containerd, build)           203.2 MiB  70.0 MiB   124.5 MiB  7.8 MiB    7.3   0.0      0.0
other WSL distros (0)                         0 B        0 B        0 B        0 B        0.0   0.0      0.0
WSL itself                                    652.0 KiB  104.0 KiB  0 B        92.0 KiB   0.2   0.0      0.0
kernel and drivers, not charged to any group  337.3 MiB

Windows holds 288.3 MiB more for the VM than the VM is using: memory freed inside
the VM that has not been handed back yet.
~/.wslconfig does not set autoMemoryReclaim; `skrog config set wsl.auto-memory-reclaim gradual`
asks WSL to hand idle memory back. See docs/vm-sizing.md.
```

**The VM lines** are the kernel's own view. *Used* is total minus free —
everything the VM is holding, cache included, because that is what Windows has
to back. It splits into four parts that add up to it: process memory, **page
cache** (file data kept in memory: image layers, build contexts, anything read
or written recently), what `/proc/meminfo` itemises as the **kernel**'s own
(slab, stacks, page tables, vmalloc, per-CPU), and what it **does not itemise
at all**. The last one is not small on WSL — about 245 MiB on the reference
host. Pages a driver allocates directly are counted by no meminfo field, and
WSL's VM runs several (`dxgkrnl`, `hv_netvsc`, the balloon); that they are
most of it is an inference, not a measurement. It is shown so the line adds up
rather than leaving a gap that reads like an accounting error.

**The Windows line** is the Vmmem process as Windows sees it. The first figure
is the one in Task Manager's *Memory* column (the private working set). It is
read from the system process list, which needs no elevation.

**The rows** are cgroups, which is how the kernel itself keeps count:

| row | what it is |
|---|---|
| each container | one of this engine's running containers |
| engine | the `skrog-engine` distro's own processes — dockerd, containerd, BuildKit, the shims — and the page cache *they* pulled in. Image pulls and builds land here, not in a container |
| another engine's containers | container groups this engine did not start that still hold memory — Docker Desktop, if it shares the VM. Hidden when there are none |
| other WSL distros | every other running distro, together. Their names are not visible from inside the VM, so they are counted, not named |
| WSL itself | WSL's own processes in the VM |
| kernel and drivers, not charged to any group | used memory no group accounts for: mostly the part the kernel does not itemise, plus cache nothing is charged for. **Derived**, not measured: used minus every top-level group |

**MEMORY** is everything charged to the group; **ANON** is its heaps and stacks,
**FILE** its page cache, **KERNEL** kernel memory spent on its behalf.

**CPU%** is of one CPU, the same convention as `docker stats`, so two busy
cores read 200. The first frame shows a dash: there is nothing to average over
yet.

**PSI** (pressure stall information) is the share of the last ten seconds in
which work was *stalled* waiting on memory or I/O. Usage says a resource is
used; pressure says it is short. A container at 0.0 is not being slowed down,
however much it holds.

## The two usual answers

### Windows has not been given the memory back

When Windows' Vmmem figure is well above what the VM is using — by at least
256 MiB and a quarter of *used* — `top` says so, as in the reading above:
288 MiB of guest memory that is free inside the VM and still held on the host.
The two figures are different measures (Windows' private working set, the
guest's total minus free), which is why a small difference either way is not
reported. The engine's kernel log shows the balloon returning free memory in
2 MiB chunks (`page_reporting_order 9`), which is one reason a fragmented free
list stays with Windows; that is read from the log, not measured.

### Page cache

When the page cache is large — at least 1 GiB and a quarter of what the VM
holds — `top` says so:

```
3.0 GiB of the VM's memory is page cache: file data Linux will drop when it needs
the room, but which Windows counts as Vmmem until it is reclaimed.
~/.wslconfig does not set autoMemoryReclaim; `skrog config set wsl.auto-memory-reclaim gradual`
asks WSL to hand idle memory back. See docs/vm-sizing.md.
```

That cache is not a leak and nothing needs killing: Linux gives it up the
moment a process needs the room. What it costs is the host's memory in the
meantime, which is what [`wsl.auto-memory-reclaim`](vm-sizing.md) is for.
Both lines end with the same pointer. The thresholds are a judgement about
where each stops being noise, set before looking at this machine, not a
measurement.

## How it differs from `docker stats`

`docker stats` covers the containers. `skrog top` covers the whole VM,
including everything that runs outside them, and does it without keeping the
engine awake. If containers are all you care about, `docker stats` is enough.

| | `docker stats` | `skrog top` |
|---|---|---|
| memory outside any container: page cache, kernel, the engine's daemons | not shown | separate rows |
| what Windows says Vmmem holds | not shown | shown |
| page cache inside a container | mostly hidden¹ | shown as FILE |
| other distros in the same VM | not shown | shown, as a count and a total |
| stall time (PSI) | not shown | per group and for the VM |
| effect on idle-stop | holds a docker connection open, so the engine cannot idle² | none: reads inside the distro |
| engine idle-stopped | goes through the bridge, which wakes it | reports it, and waits |
| network bytes per container | yes | no |

¹ Reasoned from how the upstream docker CLI computes memory on cgroup v2
(it subtracts inactive file cache), not checked against a Skrog install.
² Reasoned from the code: idle-stop is vetoed while any client connection to
the pipe is open (`internal/supervise`), and a streaming `docker stats` is
one. Not driven live.

## What it will not do

- **Start the engine.** A stopped or idle-stopped engine is reported, and a
  refreshing `top` waits for it to come back. Checked on the reference host:
  after `skrog stop`, `top --once` and `top --json` both reported `stopped`
  and the distro stayed stopped.
- **Keep the engine from idling.** Everything is read inside the distro, and
  the one engine call it makes (container names) goes to the socket there,
  never through the pipe idle-stop counts. Checked on the reference host: with
  `top` refreshing every 2 s and a 1 minute idle timeout, the engine
  idle-stopped at 66 s and stayed down.

## What it costs

One `wsl.exe` round trip per refresh — about 0.2 s on the reference host,
measured — which reads every number at once. Those reads run in the engine
distro, so they show up in the *engine* row's CPU; use a longer `--interval`
when you want that row to reflect the engine alone.

## Related

- **[#511](https://github.com/wslkit/skrog/issues/511)** — the design and the
  `docker stats` comparison
- [Right-sizing the engine's VM](vm-sizing.md) — memory, processors, swap and
  reclaim
- [Monitoring a fleet](monitoring.md) — `skrog status --prometheus`, for the
  numbers over time
