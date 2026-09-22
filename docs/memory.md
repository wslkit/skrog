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

## What it adds to `docker stats`

Both, on the reference host, at the same moment — three containers, one of
them (`cache`) started with `-m 256m`, a few seconds after pulling its image:

```
> docker stats --no-stream
NAME      CPU %     MEM USAGE / LIMIT     MEM %     NET I/O         BLOCK I/O         PIDS
cache     0.33%     5.719MiB / 256MiB     2.23%     712B / 126B     0B / 0B           6
web       0.00%     4.578MiB / 7.611GiB   0.06%     1.45kB / 126B   8.48MB / 12.3kB   5
u1        0.00%     1.523MiB / 7.611GiB   0.02%     1.8kB / 126B    11.3MB / 0B       1
```

```
> skrog top --once
VM        4 CPUs, CPU 9.1%   memory 776.4 MiB used of 7.6 GiB (7.0 GiB available)
          used is 111.7 MiB processes, 333.9 MiB page cache, 84.1 MiB kernel, 246.6 MiB not itemised by the kernel
          stalled (PSI, last 10s): cpu 0.0%  memory 0.0%  io 0.3%
Windows   vmmem 879.7 MiB (Task Manager's figure), working set 879.7 MiB, committed 1.0 GiB

                                              MEMORY     LIMIT      ANON       FILE       KERNEL     CPU%  PIDS  MEM PSI  IO PSI
web                                           8.5 MiB    -          3.8 MiB    3.9 MiB    832.0 KiB  0.0   5     0.0      0.0
u1                                            5.8 MiB    -          1.2 MiB    4.3 MiB    336.0 KiB  0.0   1     0.0      0.0
cache                                         5.7 MiB    256.0 MiB  4.6 MiB    0 B        608.0 KiB  0.3   6     0.0      0.0
engine (dockerd, containerd, build)           412.5 MiB  -          101.0 MiB  296.1 MiB  15.2 MiB   8.0   101   0.0      0.2
other WSL distros (0)                         0 B        -          0 B        0 B        0 B        0.0   0     0.0      0.0
WSL itself                                    392.0 KiB  -          104.0 KiB  0 B        100.0 KiB  0.3   1     0.0      0.0
kernel and drivers, not charged to any group  340.7 MiB
```

`docker stats` accounts for **11.8 MiB**. The VM is using **776.4 MiB**, and
Windows is holding **879.7 MiB** for it. `docker stats` explains 1.5% of what
the VM uses and has nothing to say about the rest; `top` accounts for all of
it, row by row, and puts Windows' figure beside it.

What that adds, concretely:

- **Everything that is not a container.** The engine's own daemons and the
  page cache *they* pulled in — 412.5 MiB here, 296.1 MiB of it the image
  layers from pulling `redis:alpine` — are charged to the engine, not to any
  container, so `docker stats` never shows them. Neither does the 340.7 MiB of
  kernel and driver memory, or another WSL distro running in the same VM.
- **Windows' side.** Vmmem is the number people arrive worried about, and it
  is not the VM's own "used": here Windows holds 103 MiB more than the VM uses.
  When that surplus is large, `top` says so and what to do about it (below).
- **Where each container's memory actually is.** `docker stats` subtracts
  inactive file cache from what it reports; `top` shows the whole charge and
  splits it. `web` is 8.5 MiB in `top` and 4.578 MiB in `docker stats`; the
  difference is its 3.9 MiB of FILE. Checked against the cgroup counters
  directly for `web` and `u1`: `memory.current − inactive_file` came to
  5,431,296 and 1,957,888 bytes, exactly the 5.18 MiB and 1.867 MiB
  `docker stats` printed at that moment.
- **Whether anything is short.** PSI says how much of the last ten seconds
  work spent *stalled* waiting on memory, CPU or I/O. Usage cannot tell you
  that; a container at 90% of its limit with 0.0 memory stall is fine, and one
  at 40% with a high stall is not. A moment earlier, while the `redis:alpine`
  pull was being unpacked, the VM showed 15.6% I/O stall and the engine row
  13.4% — the pull, visible as what it cost.
- **A limit only where there is one.** `docker stats` prints the VM total
  (7.611 GiB) as the "limit" of every unconstrained container, which no single
  container can reach on its own. `top` shows `cache`'s real 256 MiB and a dash
  for the others.
- **It leaves the engine alone.** A streaming `docker stats` is a docker
  connection like any other: measured on the reference host, the supervisor
  counted 3 open pipe connections while it ran and 0 before and after, and
  idle-stop is vetoed while any are open. `top` reads inside the distro and
  counts as nothing — with it refreshing every 2 s and a 1 minute idle
  timeout, the engine still idle-stopped at 66 s. Against an idle-stopped
  engine, `docker stats` wakes it; `top` reports it and waits.

| | `docker stats` | `skrog top` |
|---|---|---|
| memory outside any container: engine daemons, their page cache, kernel and drivers | not shown | separate rows |
| what Windows says Vmmem holds | not shown | shown, with a hint when it is well above the VM's use |
| a container's page cache | inactive file cache subtracted (measured) | shown as FILE, inside MEMORY |
| other distros in the same VM | not shown | shown, as a count and a total |
| stall time (PSI) | not shown | per group and for the VM |
| memory limit | the VM total when none is set | the container's own, or `-` |
| PIDs | yes | yes |
| effect on idle-stop | holds pipe connections open (3, measured), so the engine cannot idle | none |
| engine idle-stopped | wakes it | reports it, and waits |
| network bytes per container | yes | no |
| block I/O per container | yes | in `--json` (`ioReadBytes`, `ioWriteBytes`) |
| a remote engine (`skrog remote use`) | yes | no: it reads the local engine's VM |

**Where `docker stats` is still the tool:** per-container network traffic,
and any engine that is not this machine's — `top` measures a VM, and a remote
engine's VM is on another machine. For "which of my containers is busy",
either will do; for "why is Vmmem this big", only `top` has the answer.

## Reading the output

**The VM lines** are the kernel's own view. *Used* is total minus free —
everything the VM is holding, cache included, because that is what Windows has
to back. It splits into four parts that add up to it: process memory, **page
cache** (file data kept in memory: image layers, build contexts, anything read
or written recently), what `/proc/meminfo` itemises as the **kernel**'s own
(slab, stacks, page tables, vmalloc, per-CPU), and what it **does not itemise
at all**. In the reading above: 111.7 + 333.9 + 84.1 + 246.6 = 776.3 MiB, the
used figure to rounding.

The last part is not small on WSL — about 245 MiB on the reference host,
steady across readings. Pages a driver allocates directly are counted by no
meminfo field, and WSL's VM runs several (`dxgkrnl`, `hv_netvsc`, the
balloon); that they are most of it is an inference, not a measurement. It is
shown so the line adds up rather than leaving a gap that reads like an
accounting error.

**The Windows line** is the Vmmem process as Windows sees it. The first figure
is the one in Task Manager's *Memory* column (the private working set); the
working set and the committed total follow. All three come from the system
process list, which needs no elevation.

**The rows** are cgroups, which is how the kernel itself keeps count:

| row | what it is |
|---|---|
| each container | one of this engine's running containers |
| engine | the `skrog-engine` distro's own processes — dockerd, containerd, BuildKit, the shims — and the page cache *they* pulled in. Image pulls and builds land here, not in a container |
| another engine's containers | container groups this engine did not start that still hold memory — Docker Desktop, if it shares the VM. Hidden when there are none |
| other WSL distros | every other running distro, together. Their names are not visible from inside the VM, so they are counted, not named. Measured: a running Ubuntu showed up as its own group |
| WSL itself | WSL's own processes in the VM |
| kernel and drivers, not charged to any group | used memory no group accounts for: mostly the part the kernel does not itemise, plus cache nothing is charged for. **Derived**, not measured: used minus every top-level group |

In the reading above the rows sum to 773.6 MiB of the 776.4 used; the rest is
groups `top` does not list on their own, such as those left by exited
containers.

**The columns:**

| column | what it is |
|---|---|
| MEMORY | everything charged to the group (`memory.current`), page cache included |
| LIMIT | the group's `memory.max`, or `-` when it has none |
| ANON | heaps and stacks: memory that is not file-backed |
| FILE | page cache charged to the group — files it read or wrote |
| KERNEL | kernel memory spent on the group's behalf |
| CPU% | of **one** CPU, like `docker stats`: two busy cores read 200. The first frame shows a dash — there is nothing to average over yet |
| PIDS | processes and threads in the group |
| MEM PSI, IO PSI | share of the last ten seconds the group spent stalled on memory, or on I/O |

## The two usual answers

### Windows has not been given the memory back

When Windows' Vmmem figure is well above what the VM is using — by at least
256 MiB and a quarter of *used* — `top` says so. On the reference host, an
earlier reading showed 288 MiB of guest memory free inside the VM and still
held on the host:

```
Windows holds 288.3 MiB more for the VM than the VM is using: memory freed inside
the VM that has not been handed back yet.
~/.wslconfig does not set autoMemoryReclaim; `skrog config set wsl.auto-memory-reclaim gradual`
asks WSL to hand idle memory back. See docs/vm-sizing.md.
```

The two figures are different measures (Windows' private working set, the
guest's total minus free), which is why a small difference either way — the
103 MiB in the reading above — is not reported. The engine's kernel log shows
the balloon returning free memory in 2 MiB chunks (`page_reporting_order 9`),
which is one reason a fragmented free list stays with Windows; that is read
from the log, not measured.

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
The thresholds for both hints are a judgement about where each stops being
noise, set before looking at this machine, not a measurement.

## When something looks off

What each reading means, and what to do about it. Only the two hints above
are printed by `top` itself; the rest is how to read the rows. The "above 0"
and "near the limit" readings are rules of thumb, not thresholds tuned on
failing machines.

| what you see | what it means | what to do |
|---|---|---|
| the *Windows holds … more* hint | memory free inside the VM that Windows has not taken back | `skrog config set wsl.auto-memory-reclaim gradual`, then `skrog wsl-config apply` ([vm-sizing.md](vm-sizing.md)); applies from the next VM start. For now: drop the cache, below |
| the *page cache* hint, or a large FILE on the engine row after pulls and builds | file data kept in memory | nothing, usually: Linux drops it the moment something needs the room. If the host is short, drop it (below) or set `autoMemoryReclaim` |
| a container's MEMORY close to its LIMIT, MEM PSI 0.0 | the cache has filled up to the limit | nothing: that is what a limit with spare cache looks like |
| a container's MEMORY close to its LIMIT, MEM PSI above 0 | the container is being slowed by reclaim at its limit | raise the limit, or find what the app is holding (below) |
| VM memory PSI above 0, or unlimited containers with MEM PSI | the VM itself is short of memory | `skrog config set wsl.memory 8GB`, then `skrog wsl-config apply`; or stop what the rows show is using it |
| VM CPU PSI above 0, VM CPU% near CPUs × 100 | not enough CPUs | `skrog config set wsl.processors 8`, then `skrog wsl-config apply` |
| high IO PSI on a container that bind-mounts from `C:` | reads and writes crossing to Windows over 9p | `skrog config set wsl.virtiofs true` ([vm-sizing.md](vm-sizing.md)); `skrog doctor` also warns about this |
| high IO PSI on the engine row during a pull or build | the engine unpacking layers | nothing: that is the cost of the pull. 13–16% during a `redis:alpine` pull on the reference host |
| a large *other WSL distros* row | another distro sharing the VM | `wsl -l --running` names them (`top` cannot, from inside the VM); close or `wsl --terminate <name>` the one you do not need. They are not Skrog's |
| an *another engine's containers* row | another Docker engine running containers in the same VM, usually Docker Desktop | stop those containers, or quit that engine if you do not need it |
| *kernel and drivers* climbing across readings | not normal: it has held steady at about 340 MiB on the reference host | `wsl --shutdown` resets it; open an issue with `skrog top --json` output attached. Not yet seen happen |
| engine CPU% with nothing running | partly `top`'s own reads, which run in the engine distro | check with `--interval 10s` before concluding dockerd is busy |
| engine ANON staying high after builds | BuildKit or dockerd holding memory | `skrog restart` restarts the daemons. Images and volumes stay, but **running containers stop** unless they have a restart policy — do it between jobs |

### Dropping the page cache now

When the host needs the memory back immediately, dropping the VM's page cache
hands it back without stopping anything:

```powershell
wsl -d skrog-engine -u root -- sh -c "sync; echo 1 > /proc/sys/vm/drop_caches"
```

Measured on the reference host, after filling the cache by reading the image
store:

| | used | page cache | Vmmem (Windows) |
|---|---|---|---|
| after filling the cache | 6,589 MiB | 5,907 MiB | 6,424 MiB |
| right after the drop | 833 MiB | 181 MiB | 6,430 MiB |
| 15 s later | 827 MiB | 181 MiB | **954 MiB** |

The cache emptied at once, and Windows had about 5.5 GiB back within
15 seconds. It is safe: only clean cache is dropped, and nothing running loses
data. What it costs is speed afterwards, because the next reads of those files
come from disk again. It is the whole VM's cache, other distros' included,
not only the engine's.

### Raising a container's limit

```powershell
docker update --memory 1g --memory-swap 2g <container>
```

Pass `--memory-swap` too. A container started with `-m 256m` has a swap limit
of 512 MiB, and raising the memory limit past it on its own is refused —
checked on the reference host:

```
Error response from daemon: Cannot update container …: Memory limit should be
smaller than already set memoryswap limit, update the memoryswap at the same time
```

The new limit applies to the running container; nothing restarts.

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
