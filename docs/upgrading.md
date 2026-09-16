# Staying current

Three things have to stay current, and they are not independent:

```
$ skrog upgrade
app     0.3.0   -> 0.4.0 available
engine  29.8.0  (current)
cli     29.8.0  (current)

app:     download from https://github.com/wslkit/skrog/releases

note: the engines `skrog engine upgrade` can reach are pinned in this build's
manifest, so skrog 0.4.0 may offer newer engines than the 29.8.0 listed here —
upgrade skrog first, then re-check
```

## The note is the point

The convenience of one answer is the small part. The reason this command
exists is in that last paragraph: **the engines `skrog engine upgrade` can
install are compiled into the skrog binary you are running.**

So "engine: current" is only ever true *of this build*. A newer engine can
require a newer skrog first — and without being told, someone on an older
skrog runs `engine upgrade`, is told they are already current, and is wrong.
The note appears only when the app is actually behind, because a warning
printed every time is a warning nobody reads.

Same verb at two scopes:

| | |
|---|---|
| `skrog upgrade` | everything — app, engine, bundled docker CLI |
| `skrog engine upgrade` | just the engine ([details](engine-upgrade.md)) |

## What it applies, and what it only reports

| stream | |
|---|---|
| app | **reported by default, applied with `--apply`** — replacing the running binary restarts the supervisor, so it is opt-in |
| engine | applied — `skrog engine upgrade`, reversible with `skrog engine rollback` |
| bundled CLI | applied — `skrog cli install` |

`skrog upgrade --apply` brings the app forward too:

```
== app: 0.4.1 -> 0.4.2
  downloading skrog_0.4.2_windows_amd64.zip
  verified against the release's SHA256SUMS
  replaced skrog.exe, skrogw.exe, skrogtray.exe in ...\Programs\skrog
  restarting the supervisor onto the new binary
```

### If a package manager installed Skrog, let it do the upgrade

`--apply` refuses when the binary lives inside a package manager's directory:

```
$ skrog upgrade --apply
skrog: this install is managed by winget, so `skrog upgrade --apply` would
overwrite files it owns and leave it reporting a version you no longer have.

  upgrade with:  winget upgrade wslkit.skrog

Pass --force to replace the binary anyway.
```

Replacing the files behind the manager's back leaves it believing something
untrue: `winget list` keeps reporting the version it installed, a later
`winget upgrade` reinstalls over the newer binary and silently downgrades it,
and `winget uninstall` removes a package whose contents no longer match its
manifest. scoop and Chocolatey are recognised the same way.

A zip you unpacked yourself — including the one the install script places in
`%LOCALAPPDATA%\Programs\skrog` — is owned by nobody and upgrades itself
normally. Only a recognised package-manager location is refused, so an
unfamiliar install directory is always treated as unmanaged.

The order is the safety argument: the release zip is downloaded and checked
against **that release's `SHA256SUMS`** before anything on disk is touched, and
the binaries are moved aside rather than overwritten, so a failure at any point
leaves the install exactly as it was. It is not signed yet (#77) — but the
alternative it replaces is `irm https://… | iex`, a script from the internet run
with no check at all, so this is the tighter loop of the two.

`skrog.exe` takes effect immediately, because the supervisor is recycled onto
it. `skrogw.exe` and `skrogtray.exe` take effect when those processes next start
— the tray on relaunch, the watchdog at your next logon. The previous binaries
stay as `.old` beside them until the next run clears them, because Windows will
not delete an image a process is still executing.

> This used to say *"a running `.exe` cannot cleanly replace itself on Windows"*.
> That is not true: you cannot **overwrite** a running image, but you can
> **rename** one, which is how every Windows self-updater works
> ([#309](https://github.com/wslkit/skrog/issues/309)).

It always shows you the plan and asks first:

```
will run:
  skrog cli install      29.7.2 -> 29.8.0
  skrog engine upgrade   29.7.2 -> 29.8.0

proceed? [y/N]
```

`--dry-run` prints that and stops. `--check` reports without even planning.
`--yes` skips the question, for runners. `--json` implies `--check`, because
there is no way to ask a question in JSON.

The CLI goes first: it is a file copy costing no downtime, where the engine
upgrade stops and restarts the engine and takes minutes. So a failed engine
upgrade leaves a machine with the CLI already current rather than nothing done,
and the two are independent — nothing is ever half-applied.

## Nothing checks on its own

There is no auto-update, no background poll, and no "latest" resolved at
install time. This runs when you run it.

One outbound request is made, to the GitHub releases API, for the app version.
It sends nothing but the request: no machine identifier, no version, no
telemetry. The server learns an IP asked, which is what downloading anything
would tell it anyway.

## Coming from Hawser (v0.3.1 or earlier)

The project was called **Hawser** through v0.3.1 and is **Skrog** from v0.4.0
([#1](https://github.com/wslkit/skrog/issues/1)). That release renames every
contract a machine can hold: the binaries (`skrog.exe`, `skrogw.exe`,
`skrogtray.exe`), the WSL distro (`skrog-engine`), the docker context
(`skrog`), the state directory (`%LOCALAPPDATA%\Skrog`), the config and lock
files (`skrog.yaml`, `skrog.lock`), the named pipes, and the `SKROG_*`
environment variables.

**There is no in-place upgrade.** `skrog upgrade` reports the new version and
points at the releases page, as it does for any app upgrade — but the thing you
download is a different program with a different name, not a newer copy of the
one you have. Nothing inherits a Hawser install's state.

The one thing worth knowing before you uninstall: **your images and volumes live
inside the `hawser-engine` distro**, and removing it removes them.

Because every name differs, the two can be installed at the same time — which
gives you a way to keep them:

```powershell
# With Hawser still installed and its engine running:
skrog install
skrog start
skrog migrate --from-context hawser --dry-run   # what would move, and how big
skrog migrate --from-context hawser
```

`skrog migrate` is documented for Docker Desktop, but the mechanism is
engine-to-engine — `docker save` piped to `docker load` for images, a streamed
tar for volumes — and `--from-context` takes any docker context. It never
writes to the source, so an interrupted run leaves the Hawser side untouched.

This path has not been exercised against a Hawser install specifically, so run
the `--dry-run` first and keep Hawser until you have checked what arrived. Once
you are satisfied, uninstall Hawser to reclaim its distro.

If you have nothing in the engine worth keeping, uninstall Hawser first and
install Skrog into the space — there is nothing to preserve.

## Air-gapped machines

`--offline` skips that request entirely:

```
skrog upgrade --offline
```

The engine and CLI answers are still real, because both manifests are compiled
into the binary — there is nothing to fetch. The app line reports **unknown**,
not "current": a check that did not happen must never read as a check that
passed. See [air-gapped installs](air-gap.md).

## Scripting it

```
skrog upgrade --json
```

Exit codes follow `skrog cli status`, so either can gate a script the same
way: **0** nothing to do (or everything applied), **3** something can be
upgraded, **1** error, **2** usage. The 3 is reported by `--check` and
`--dry-run`; a plain run that applies successfully exits **0**.

Each stream carries a `status` of `current`, `available`, `unknown` or
`not-installed`. `unknown` and `not-installed` are distinct from `current` on
purpose, and neither counts as an upgrade — a component you never installed is
not out of date, and a component that could not be checked is not up to date.

## In the tray

"Check for updates" runs the same command and puts the answer in its tooltip.
It opens the releases page only when something is actually available; being
told "everything is up to date" without a browser window is the common case,
and the better one.
