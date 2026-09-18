# Installing Skrog

Installing Skrog is three separate steps, and knowing they are separate saves
the most common confusion — "I installed it and `docker` says command not
found":

| | what it gives you |
|---|---|
| 1. **Get `skrog.exe`** | the command-line tool, on your PATH. Nothing is provisioned |
| 2. **`skrog install`** | the Docker **engine**, and a bridge serving it on a Windows named pipe |
| 3. **`skrog cli install`** | the **`docker` command** itself — *only if you do not already have one* |

Step 3 is not automatic, and that is deliberate: most people arrive with Docker
Desktop's `docker.exe` already on PATH and would not want a second one. On a
clean machine you do need it, and `skrog install` now says so when it finishes.

## Requirements

- **Windows 11**, or **Windows 10 22H2** (build 19045) — both tested.
- **An x64 machine.** The engine rootfs is built amd64-only, so **Windows on
  ARM cannot run the engine** — see below.
- **WSL 2.x**, from the Microsoft Store or the MSI. `wsl --version` should
  print something; if it errors, run `wsl --update`.
- Virtualization enabled in firmware.

### Windows on ARM

`skrog.exe` has an arm64 build and it runs natively — the CLI, the bridge and
the supervisor are all fine. **The engine is not available.** Every rootfs in
the version manifest is built x86-64, and WSL2 runs the guest on the host CPU,
so an x86-64 `dockerd` cannot start inside an arm64 utility VM. There is no
emulation route.

`skrog install` **refuses on arm64** with exit code `4` rather than
downloading 200 MB and failing obscurely afterwards. If you have built an
arm64 rootfs yourself, `skrog install --rootfs-url <url> --rootfs-sha256 <sha>`
still works — that path is deliberately open.

Until a published arm64 rootfs exists
([#388](https://github.com/wslkit/skrog/issues/388)), use Docker Desktop or
Rancher Desktop on ARM; both ship an arm64 engine.

Nothing else. Skrog does not need Docker Desktop, and coexists with it if you
keep it.

## Step 1 — get `skrog.exe`

### The one-liner

```powershell
irm https://wslkit.github.io/skrog/install.ps1 | iex
```

It resolves the newest release, downloads the zip for your architecture,
**verifies it against the release's `SHA256SUMS`**, unpacks it to
`%LOCALAPPDATA%\Programs\skrog`, and adds that to your user PATH (no
elevation). It provisions nothing — that is step 2.

Options come from the environment, because `irm | iex` cannot take arguments:

```powershell
$env:SKROG_VERSION = '0.6.0'            # default: newest release
$env:SKROG_DIR     = 'C:\tools\skrog'   # default: %LOCALAPPDATA%\Programs\skrog
$env:SKROG_NO_PATH = '1'                # do not touch PATH
```

[Read the script first](https://github.com/wslkit/skrog/blob/main/scripts/install.ps1)
if you would rather not pipe a URL into your shell. It does nothing the manual
steps below do not.

It installs the newest **stable** release. `skrog upgrade --check` deliberately
counts pre-releases too, so it can tell you a preview exists on a machine this
script put on the stable build — first contact should be stable, an explicit
"what is new?" should be complete.

### Or with scoop

```powershell
scoop bucket add skrog https://github.com/wslkit/scoop-skrog
scoop install skrog
```

Then `scoop update skrog` keeps it current. Skrog notices it is managed —
`skrog upgrade --apply` refuses inside a package-manager directory and tells
you to use scoop instead, rather than fighting it for the same files.

A winget package is [#412](https://github.com/wslkit/skrog/issues/412). Neither
is blocked on code signing, which this project believed for longer than it
should have; only the MSI is.

### Or by hand

1. Open the [latest release](https://github.com/wslkit/skrog/releases).
2. Download the zip for your architecture and the `SHA256SUMS` file beside it:

   | you have | download |
   |---|---|
   | 64-bit Intel/AMD (almost everyone) | `skrog_<version>_windows_amd64.zip` |
   | Windows on ARM (Snapdragon, Surface Pro X) | `skrog_<version>_windows_arm64.zip` — **CLI only, [no engine](#windows-on-arm)** |

   Not sure? `$env:PROCESSOR_ARCHITECTURE` prints `AMD64` or `ARM64`.

3. Check it, and compare the line for your zip:

   ```powershell
   Get-FileHash .\skrog_<version>_windows_amd64.zip -Algorithm SHA256
   Get-Content .\SHA256SUMS
   ```

4. Unpack it somewhere on your PATH — `%LOCALAPPDATA%\Programs\skrog` is what
   the script uses:

   ```powershell
   Expand-Archive .\skrog_<version>_windows_amd64.zip -DestinationPath "$env:LOCALAPPDATA\Programs\skrog"
   ```

The zip contains `skrog.exe`, `skrogw.exe` (the windowless logon launcher —
autostart needs it), `skrogtray.exe` (an optional status tray), `skrog-agent`
(a Linux binary used by the [wslc backend](wslc-backend.md)), plus `LICENSE`
and `README.md`. Keep them together.

> **SmartScreen will warn on first run.** The binaries are not
> Authenticode-signed. The [SignPath Foundation](https://signpath.org)'s free
> programme declined for now — it is for projects with an established user base
> — and invited a reapplication as visibility grows; paying for a certificate
> is the other route. Which one, and when, is
> [#77](https://github.com/wslkit/skrog/issues/77), so no date is promised here.
>
> An **MSI** waits on the same answer, because an installer that asks for
> elevation unsigned is a worse experience than a zip, not a better one. A
> **winget** package does not — winget's `portable` type unpacks per-user and
> needs no elevation or signature — and one is on the way; until it lands it is
> download-and-unpack, which is exactly why the checksum step above is written
> out in full rather than waved at.
>
> What every release *does* carry today is **SLSA build provenance** and a
> **cosign-signed `SHA256SUMS`** — two things an Authenticode signature does not
> give you, because they tie the artifact to a workflow run and a commit. See
> [verifying a download](security.md#verifying-a-download).

## Step 2 — install the engine

```powershell
skrog install
```

This downloads the checksum-verified engine rootfs, imports it as the
`skrog-engine` WSL2 distro, starts the engine, wires a `skrog` docker context,
and registers the supervisor to start at logon. It prints the pipe it took.

Then bring the bridge up now (from your next logon it starts itself):

```powershell
skrog start
```

Useful flags:

| flag | why |
|---|---|
| `--no-autostart` | do not start the supervisor at logon |
| `--headless` | never prompt; for CI and unattended installs |
| `--locked skrog.lock` | install the exact engine a `skrog lock` pinned |
| `--offline bundle.zip` | install from an air-gap bundle ([air-gap.md](air-gap.md)) |
| `--engine wslc` | use a WSL container session instead ([wslc-backend.md](wslc-backend.md)) |

## Step 3 — make sure you have a `docker` command

**Skrog does not install one by default.** Check:

```powershell
docker version
```

If that works, you are done — skip to *Verify*. It usually works when Docker
Desktop is installed, because its CLI is already on PATH.

If it says *"command not found"*, install the upstream tools:

```powershell
skrog cli install
```

That fetches and checksum-verifies pinned versions of the Docker CLI, Compose
v2, Buildx and the Windows credential helper, straight from each project's own
release assets — no Docker Desktop and no third-party mirror. Open a **new**
terminal afterwards so PATH is picked up. Details in
[docker-cli.md](docker-cli.md).

## Verify

```powershell
docker run --rm hello-world
skrog status
skrog doctor
```

`skrog status` should show the engine running and name the pipe it bound.
`skrog doctor` checks the host, the engine, PATH, contexts, VPN/DNS/proxy
interference and more, and `--fix` applies the safe remedies.

### Which pipe, and when you need a context

If Docker Desktop is not serving it, Skrog takes `\\.\pipe\docker_engine` — the
pipe `docker` already talks to — and plain `docker` reaches Skrog with no flag
and no context.

If Desktop owns that pipe, Skrog serves its own and you select it:

```powershell
docker context use skrog      # or: docker --context skrog ps
```

The install output names which case you are in, and `docker context inspect
skrog` shows it at any time.

## Tab completion

Optional, and worth the one line: `skrog` has 35 commands and a config surface
of around 25 dotted keys, which is more than anyone should type from memory.

The release zip contains a `completions` folder.

**PowerShell** — dot-source it from your profile:

```powershell
# see where your profile lives, and create it if it does not exist
if (-not (Test-Path $PROFILE)) { New-Item -ItemType File -Path $PROFILE -Force }
Add-Content $PROFILE ". '$env:LOCALAPPDATA\Programs\skrog\completions\skrog.ps1'"
```

Open a new terminal, then `skrog sn<TAB>` completes to `snapshot`, and
`skrog status --<TAB>` offers that command's flags.

**bash** (Git Bash, or a distro you have `wsl-integrate`d):

```bash
source /c/Users/you/AppData/Local/Programs/skrog/completions/skrog.bash
# or, to make it permanent
echo "source .../completions/skrog.bash" >> ~/.bashrc
```

Both scripts complete commands, subcommands and flags from a list generated
from the binary itself — CI fails if they drift from what the CLI accepts.
Where the useful answer is something only your machine knows, they ask it at
the moment you press TAB:

| You type | It offers |
| --- | --- |
| `skrog config set <TAB>` | every setting key, including `engine.*` and `hook.*` |
| `skrog profile switch <TAB>` | your saved profiles |
| `skrog snapshot restore <TAB>` | your snapshots |
| `skrog remote use <TAB>` | your registered remotes, plus `local` |

Those lookups are read-only and never start the engine, so completing a word
never costs you a cold start.

## Upgrading

```powershell
skrog upgrade --apply
```

That replaces `skrog.exe` itself. The engine is a separate, pinned thing:
`skrog engine list`, `skrog engine upgrade`, `skrog engine rollback`. See
[upgrading.md](upgrading.md) and [engine-upgrade.md](engine-upgrade.md).

## Uninstalling

```powershell
skrog uninstall
```

Removes the distro, the supervisor registration and the docker context. It asks
before deleting engine data — images and volumes — because that is not
recoverable. Then delete the folder from step 1.

## If something goes wrong

`skrog doctor` first: it diagnoses most of it and explains the rest.

- **Corporate network, proxy or TLS inspection** —
  [corporate-network.md](corporate-network.md)
- **A VPN breaks networking after connecting** — [vpn.md](vpn.md)
- **No internet on the machine at all** — [air-gap.md](air-gap.md)
- **`docker` resolves to the wrong binary** — `skrog doctor` names it and says
  what will move it
