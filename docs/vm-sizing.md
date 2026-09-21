# Right-sizing the engine's VM

A runner with 8 GB should not let the engine take four of them. WSL2's sizing —
memory, processors, swap, and how idle memory is given back — lives in
`%USERPROFILE%\.wslconfig`, and that file is **global**: every WSL2 distro on
the machine reads it, Docker Desktop's and Rancher's included.

So Skrog splits it in two. You record what you want in Skrog's own settings;
one command writes it to the shared file, after showing you the diff.

```powershell
skrog config set wsl.memory 4GB
skrog config set wsl.processors 2
skrog wsl-config apply          # shows the diff, asks, then writes
```

## The keys

| Skrog setting | `.wslconfig` key | what it does |
| --- | --- | --- |
| `wsl.memory` | `memory` | cap on the VM's RAM (`4GB`, `512MB`) |
| `wsl.processors` | `processors` | how many CPUs the VM sees |
| `wsl.swap` | `swap` | swap file size; `0` disables it |
| `wsl.auto-memory-reclaim` | `autoMemoryReclaim` | `gradual`, `dropcache` or `disabled` — hands idle memory back to Windows |
| `wsl.virtiofs` | `virtiofs` | `true` mounts Windows drives over virtiofs instead of 9p — see below |

Values are validated when you set them, in the way WSL reads them, so a typo
fails at `skrog config set` rather than silently sizing the VM as something
else. `memory=4` in `.wslconfig` means *four bytes*; `skrog config set
wsl.memory 4` refuses and tells you to write `4GB`.

Networking keys (`networkingMode`, `dnsTunneling`, `autoProxy`) are **not**
managed here. `skrog doctor` recommends those for VPNs ([vpn.md](vpn.md)) and
leaves them to you, because they change how every distro on the machine reaches
the network.

## `wsl.virtiofs`: a faster `/mnt/c`, and the one key here that is not about size

WSL2 has historically mounted Windows drives into distros over **9p**. WSL 2.9
can use **virtiofs** instead, and for a source tree on `C:` that is the single
largest performance difference available to the distro backend.

```powershell
skrog config set wsl.virtiofs true
skrog wsl-config apply
wsl --shutdown          # required, and it stops every distro on the machine
```

`skrog doctor` warns when the engine is still on 9p, with those three commands
as its remedy. It reads `/proc/mounts` in the running engine rather than this
file, because the two disagree exactly when it matters: WSL below 2.9 ignores
the key silently, and setting it without the shutdown changes nothing. Both
look like success in `~/.wslconfig`.

### Measured

A Windows folder bind-mounted into a container through Skrog, timed inside the
container, on WSL 2.9.11 / Windows 10 22H2. First write after a VM boot
discarded and the write repeated three times, because a cold VM's first write
is not representative — it came in at 34 MB/s and the next three at 103, 239
and 155.

| | 9p | virtiofs | |
|---|---|---|---|
| write 256 MB (`conv=fsync`) | 122 MB/s | 155 MB/s | ~1.3×, noisy on both |
| read 256 MB (warm) | 205 MB/s | **811 MB/s** | **4.0×** |
| create 1000 files | 2.17 s | 2.02 s | ~1.1× |
| `ls -l` 1000 files | 0.68 s | **0.22 s** | **3.1×** |
| read 1000 files | 2.49 s | 1.52 s | 1.6× |
| delete 1000 files | 1.52 s | 0.92 s | 1.7× |

Reads and directory listings are where it pays, which is the shape of `npm
install`, a gradle build, or a large `docker build` context. Writes are a
modest gain with enough variance that it is fair to call them a wash.

### Side effects, checked rather than assumed

| | virtiofs |
|---|---|
| `chmod` | **works** — `chmod 600` sticks, same as 9p's `metadata` option |
| symlinks | work |
| case sensitivity | insensitive, unchanged |
| `inotify` | events fire, so file watchers keep working |

That first row is worth stating because
[microsoft/WSL#40719](https://github.com/microsoft/WSL/issues/40719) reads as
though virtiofs loses permission metadata in general. It does not here: WSL's
distro mount path passes the `metadata` option, so `chmod` sticks.

### Caveats

- **WSL 2.9 or newer.** On older WSL the key is ignored silently and you stay
  on 9p. Check with `wsl --version`.
- **Machine-wide**, like every key on this page. Every distro's `/mnt/*`
  changes, Docker Desktop's included.
- **Needs `wsl --shutdown`** to take effect, which stops everything.
- Confirm it took: `wsl -d skrog-engine -u root --exec sh -c "grep ' /mnt/c ' /proc/mounts"`
  should say `virtiofs`, not `9p`.

## Nothing is written without consent

```
$ skrog wsl-config apply
C:\Users\me\.wslconfig is shared by every WSL2 distro on this machine.

Skrog would change:
  + memory=4GB
  + processors=2
  + autoMemoryReclaim=gradual

apply these changes? [y/N]
```

Answer anything but yes and nothing is written. On a runner, where the owner
has already decided, `--yes` skips the prompt — and re-running it changes
nothing, so a converge run is safe to repeat:

```
$ skrog wsl-config apply --yes
wrote 3 change(s) to C:\Users\me\.wslconfig
$ skrog wsl-config apply --yes
C:\Users\me\.wslconfig already matches Skrog's settings; nothing to do
```

**Everything else in the file survives** — other sections, keys Skrog does not
manage, comments (including trailing ones on a line it edits), blank lines and
your ordering. The file belongs to you; Skrog edits the lines it was asked to
and nothing more.

`skrog wsl-config show` reports the effective sizing and anything pending, and
`--json` gives `effective`, `desired`, `pending` and `applied` for a converge
script.

## It takes effect on the next VM start, and Skrog will not force that

New sizing applies when the WSL2 VM next starts. The only way to force it is
`wsl --shutdown`, which stops **every** distro on the machine — someone else's
containers included — so Skrog tells you rather than doing it:

```
  the new sizing takes effect when the WSL VM next starts.
  `skrog stop` does not do that (it stops only Skrog's distro); a reboot,
  or `wsl --shutdown` when you are sure nothing else is running, does.
```

`skrog stop` is not enough on purpose: it stops Skrog's distro, and the
utility VM stays up for as long as any distro is running.

## What doctor says about it

`skrog doctor` always reports the effective sizing, and judges it in exactly
two cases:

- **Recorded but not applied.** You ran `skrog config set wsl.memory 4GB` and
  never applied it, so the cap you believe is in force is not. Warn, with
  `skrog wsl-config apply` as the remedy — this is the mistake the two-step
  design makes possible, so the tool watches for it.
- **A small host with no explicit sizing.** On ≤8 GB, WSL's default of half the
  host's RAM plus a build's own memory can push the machine into swap, and the
  symptom is a slow laptop rather than anything that looks like an engine
  problem. Warn, with a concrete starting point.

On a large machine with no sizing set it says so and stays `ok`: WSL's default
is a reasonable answer there, and a check that warned on every untuned machine
would be noise.

## See also

- [ci-runners.md](ci-runners.md) — running jobs against the engine
- [housekeeping.md](housekeeping.md) — `skrog prune` and `skrog compact` for
  the disk side of the same problem
- [vpn.md](vpn.md) — the networking keys, which stay advisory
