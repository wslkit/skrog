# The `--json` contract

Every Skrog command that reports state can emit machine-readable JSON with
`--json`. This is **the contract** that tools build on — the
[VS Code extension](https://github.com/wslkit/skrog-vscode), CI scripts,
fleet health checks — so it is governed by three rules:

1. **Additive only.** Fields are added, never renamed or removed. A consumer
   that ignores unknown fields keeps working across Skrog versions.
2. **Exit codes mean the same thing as the human output** — `0` ok, `1` error,
   `2` usage, `3` not installed / not found, `4` unsupported platform (this
   machine cannot run what was asked for and no retry will change that: the
   manifest has no engine rootfs built for this CPU architecture. amd64 and
   arm64 both do, so in practice this means neither,
   [#388](https://github.com/wslkit/skrog/issues/388)) — and a non-zero exit still emits
   the JSON when there is something to say (e.g. `version --json` exits 3 with
   no engine, `cli status --json` exits 3 with tools missing).
3. **Arrays are arrays.** An empty list is `[]`, never `null`. Where `null` is
   used it is deliberate and documented (`config --json` → `engine`).

Shapes are pinned by `cmd/skrog/jsonshapes_test.go`.

## `skrog status --json`

```json
{
  "installed": true,
  "distro": "skrog-engine",
  "stateDir": "C:\\Users\\me\\AppData\\Local\\Skrog",
  "supervisor": "running",
  "engine": "running",
  "desired": "running",
  "profile": "work",
  "gpu": { "enabled": true, "probed": true, "visible": true, "specInstalled": true },
  "endpoint": {
    "pipe": "\\\\.\\pipe\\docker_engine",
    "dockerHost": "npipe:////./pipe/docker_engine",
    "reason": "default pipe is free"
  }
}
```

- `supervisor`: `running` | `stopped`.
- `engine`: `running` | `idle` | `stopped`. **`idle`** is the engine stopped by the
  idle timeout — healthy, it wakes on the next `docker` call. Scripts can tell
  it from broken.
- `desired`: `running` | `stopped` (what `skrog start`/`stop` last asked for).
- `profile`: omitted when no profile is active.
- `gpu`: `visible` and `specInstalled` are **only probed while the engine is
  running and `enabled` is true** — status never boots a stopped distro (#82).
  `probed: false` means they are not authoritative.
- `endpoint`: where the engine is answering, and **absent unless a supervisor is
  running** — nothing is being served then, so there is no endpoint to name.
  - `pipe` is the named pipe; `dockerHost` is the same thing spelled the way the
    docker CLI wants it, so a consumer setting `DOCKER_HOST` does not have to
    convert between the two.
  - `reason` says why that pipe: the default one was free, or something else
    (usually Docker Desktop) already had it.
  - It reports **what the running supervisor bound**, read back from its own
    record — never recomputed. Docker Desktop can start or stop after Skrog
    chose, so asking the selector again could name a pipe nothing is serving
    (#273).

Exit `0` always (an uninstalled machine is `installed: false`, not an error).

## `skrog version --json`

`{ "app": "0.3.0", ... }` — the full component picture; see `skrog version`.
Exits `3` when no engine is installed (still emits JSON).

## `skrog doctor --json`

```json
{
  "app": "0.4.0",
  "worst": "warn",
  "results": [
    {
      "name": "wsl-version",
      "title": "WSL default version",
      "status": "ok",
      "summary": "default version is 2",
      "detail": ["..."],
      "remedy": "wsl --set-default-version 2",
      "fixed": "set the default WSL version to 2"
    }
  ]
}
```

An **object**, not an array: `worst` saves a consumer from folding the
statuses itself, and `app` identifies the build that ran the checks. The
results are under `results`. `status` is one of `ok` | `skip` | `warn` |
`fail`; `detail`, `remedy` and `fixed` are omitted when empty, and `fixed`
records what `--fix` did on this run.

Exit code is **`0` when every check passed or only warned, `1` when one or
more failed, `2` usage** — the same three `doctor --help` states. A warning is
not a failure: `doctor` warns about things worth knowing that do not stop the
engine working, so gating a fleet script on a non-zero exit would report
healthy machines as broken.

> Both of these were documented wrongly here until v0.4.0
> ([#238](https://github.com/wslkit/skrog/issues/238)) — as an array, and with
> `1` meaning warn. A consumer written against the old text iterated object
> keys and treated a *failing* doctor as a usage error.

## Not JSON: `skrog status --prometheus`

The same numbers `status --stats --json` reports are also available as
Prometheus text for node_exporter's textfile collector — see
[monitoring.md](monitoring.md). It is the one machine-readable output here that
is not JSON, and its exit code deliberately differs (always `0` when metrics
could be written, because the engine's state is *in* the metrics).

## `skrog config --json`

```json
{
  "settings": { "idle-timeout": "off", "audit": "on", "network.proxy": "", "gpu": "off", "...": "..." },
  "engine":   { "engine.registry-mirrors": "https://mirror.corp", "...": "..." }
}
```

- `settings`: every known key with its stored value **or default** — a stable
  key set.
- `engine`: the engine's `daemon.json` keys. **`null` when no engine is
  installed; `{}` when installed with nothing set.** The two are different
  answers.

## `skrog cli status --json`

```json
{
  "arch": "amd64",
  "binDir": "C:\\...\\Skrog\\bin",
  "onPath": true,
  "activeDocker": "C:\\...\\Skrog\\bin\\docker.exe",
  "tools": [
    { "name": "docker",  "version": "29.8.0", "role": "cli",    "path": "...", "installed": true, "available": true },
    { "name": "compose", "version": "5.5.1",  "role": "plugin", "path": "...", "installed": true, "available": true }
  ]
}
```

- `available`: whether the manifest publishes the tool for this arch at all.
  Every tool is available on both architectures today; the field exists
  because that has not always been true and need not stay true — a component
  with no build for the host reports `false` rather than failing at install.
- `activeDocker`: the `docker` that resolves on PATH; omitted if none.
- Exits `3` when an available tool is not installed.

## `skrog snapshot … --json`

- `snapshot list --json` → array (always) of
  `{ "name", "created", "engineVersion", "distro", "sha256", "sizeBytes" }`.
- `snapshot save <name> --json` → one such object.
- `snapshot restore <name> --yes --json` → `{ "restored": "<name>" }`.
- `snapshot delete <name> --json` → `{ "deleted": "<name>" }`.

Flags come **before** the verb: `skrog snapshot --json list`.

## `skrog profile … --json`

- `profile --json` (list) →
  `{ "active": "work", "profiles": [ { "name": "work", "active": true }, … ] }`
  (`active` omitted when none; `profiles` always an array).
- `profile show <name> --json` → the profile as the same document its YAML
  holds: `distro`, `data-dir`, `engine-version`, `idle-timeout`, `autostart`,
  `engine`, `hooks`, `integrations` (unset fields omitted).

## `skrog audit tail`

The audit log **is already JSON lines** — one `audit.Event` per line:

```json
{"time":"2026-09-09T16:46:39.226Z","action":"image-pull","method":"POST","path":"/v1.44/images/create","image":"nvidia/cuda:12.4.1-base-ubuntu22.04","status":200,"ms":1840}
```

Fields: `time`, `action`, `method`, `path`, `image`, `name`, `container`,
`status`, `ms`, `error` (optional ones omitted when empty). Records are derived
from the request line only, never the body.

- Default output: JSON lines (one parse per line; friendly to `tail -f`).
- `audit tail --json`: the same records as **one JSON array**, for a single
  parse. `[]` when the log does not exist yet.

## `skrog install --json`

The resulting install manifest: `{ "distro", "dataDir", "rootfsUrl",
"rootfsSha256", "engineVersion", "installedAt", "wslVersion" }`.

## `skrog remote … --json`

- `remote --json` (list) →

  ```json
  {
    "current": "desktop",
    "remotes": [
      { "name": "desktop", "host": "tcp://my-desktop.corp:2376", "added": "…",
        "certNotAfter": "2028-09-09T16:00:00Z", "dir": "C:\\...\\remotes\\desktop", "current": true }
    ]
  }
  ```

  `current` is `"local"` when docker is on the `skrog` context, a remote's name
  when on `skrog-<name>`, and `""` when docker is on some other context
  entirely. `remotes` is always an array.
- `remote test <name> --json` → `{ "name", "serverVersion", "ms" }`. Exits `1`
  when the remote does not answer, `3` when no such remote.

## `skrog audit trace --json -- <cmd> [args]`

Runs the command, then reports what it did to the engine from the audit records
appended while it ran:

```json
{
  "command": ["act", "-j", "build"],
  "exitCode": 0,
  "ms": 41200,
  "events": 9,
  "actions": { "image-pull": 2, "container-create": 3, "container-start": 3, "exec-start": 1 },
  "images": ["catthehacker/ubuntu:act-latest", "node:20"],
  "containers": ["act-build-1a2b", "db"],
  "note": "audit log rotated during the run; the summary covers the current file"
}
```

- `exitCode` is the traced command's own; **the process exits with it too**, so
  `audit trace -- make test` fails exactly when `make test` does.
- `images` / `containers` are the distinct ones touched — always arrays.
- `note` is omitted unless something qualifies the summary (rotation mid-run, or
  no records written at all).
- Attribution is by log position (appended after the command started), so
  concurrent docker use during the run is included.
- Requires `audit` to be on; otherwise exits `1` with the recipe.
- `--raw` instead prints the matching records as JSON lines.

## `skrog healthcheck --json`

A readiness probe for runner warm-ups and orchestrators:

```json
{ "installed": true, "supervisor": "running", "engine": "idle", "ready": true,
  "reason": "engine idle; wakes on the next docker command" }
```

- `ready` is the verdict the exit code carries: **`0` ready, `1` not ready,
  `3` not installed**. `reason` is never omitted.
- Ready means a docker command would succeed now: the supervisor is serving the
  pipe **and** the engine is `running` or `idle` (idle wakes on demand).
- `--wait <duration>` keeps probing until ready or the deadline; nothing is
  started by the probe itself — pair it with `skrog start`.

## `skrog logs --json`

One object per line, the **same envelope for every source** so a log shipper
needs one pipeline:

```json
{"source":"dockerd","line":"time=\"2026-09-09T16:44:18Z\" level=info msg=\"Daemon has completed initialization\""}
{"source":"supervisor","line":"time=... level=INFO msg=\"engine socket is up\" distro=skrog-engine"}
{"source":"audit","line":"{\"time\":\"...\",\"action\":\"image-pull\",...}"}
```

- `--source supervisor|dockerd|audit` (default supervisor); `-n <lines>` (default
  200, `0` = all); `--follow` streams new lines and survives the 5 MB rotation.
- `line` is the raw record; parse it further if you want dockerd's logfmt fields
  or the audit event's JSON.
- Exits `3` for `--source dockerd` with no engine installed.

## `skrog prewarm --json <images.txt>`

Pulls a pinned image list ahead of need (runner warm-up, golden-image bake,
post-start hook), through whatever docker currently targets:

```json
{
  "file": "images.txt",
  "concurrency": 3,
  "pulled": 2,
  "failed": 1,
  "ms": 8420,
  "images": [
    { "ref": "alpine:3.20", "ok": true, "ms": 1210 },
    { "ref": "node:20@sha256:…", "ok": true, "ms": 8390 },
    { "ref": "ghcr.io/x/missing:1", "ok": false, "ms": 640, "error": "manifest unknown" }
  ]
}
```

- `images` is in list order and always an array; `error` is omitted on success.
- Exit `0` only when `failed` is `0`; a failed pull never stops the others.
- The list file: one reference per line, `#` comments and blank lines ignored,
  duplicates dropped. Digest pins encouraged.

## `skrog runner check --json`

One verdict on whether an unattended host will bring the engine back after a
reboot (see [auto-logon-runner.md](auto-logon-runner.md)):

```json
{
  "ready": false,
  "findings": [
    { "name": "autologon", "status": "ok", "summary": "auto-logon is configured" },
    { "name": "autologon-account", "status": "ok", "summary": "auto-logon uses this account" },
    { "name": "autologon-password", "status": "warn",
      "summary": "the auto-logon password is stored in clear text in the registry (Winlogon\\DefaultPassword)",
      "remedy": "use Sysinternals Autologon, which stores it as an LSA secret, then delete the DefaultPassword registry value (docs/auto-logon-runner.md §3)." },
    { "name": "autostart", "status": "fail", "summary": "no logon autostart; the session will start but the supervisor will not",
      "remedy": "run `skrog autostart enable` as the auto-logon account (needs skrogw.exe beside skrog.exe)." },
    { "name": "supervisor", "status": "ok", "summary": "supervisor is running" },
    { "name": "power", "status": "warn",
      "summary": "the machine sleeps on mains power (sleeps after 1h0m0s), which suspends a job mid-run",
      "remedy": "powercfg /change standby-timeout-ac 0 && powercfg /change hibernate-timeout-ac 0" },
    { "name": "engine", "status": "ok", "summary": "engine is running" }
  ]
}
```

- `ready` is the exit code's verdict: **`0` ready (warnings allowed), `1` not
  ready, `3` not installed**. `findings` is always an array; `status` is `ok` |
  `warn` | `fail`; `remedy` is omitted when `ok`.
- `power` reads the active scheme's **AC** timeouts only: a laptop on battery
  having short DC timeouts is correct, not a misconfiguration. It **warns and
  never fails** — many runners are desktops that will never sleep, and a check
  that fails a healthy host is one people learn to ignore. A reading that could
  not be taken warns as unknown rather than passing.
- Read-only and unelevated. The auto-logon account is **compared, never
  printed**, and the password value is probed for existence only.

## `skrog reset --to <snapshot> --json`

The runner's clean slate — `snapshot restore` with the interactive guards
implied (no `--yes`, no running-container check):

```json
{ "snapshot": "golden", "engineVersion": "29.7.2", "ms": 6840 }
```

- `ms` is the whole cycle — verify the archive, unregister, import, engine back
  — the number a clean-slate budget is measured against.
- `engineVersion` is the snapshot's recorded engine, omitted when unknown.
- Exit `3` when the snapshot does not exist or nothing is installed; `1` when
  the restore failed (the engine is brought back best-effort either way).

## `skrog prune --json`

Reclaims disk on whatever docker currently targets:

```json
{
  "reclaimedBytes": 1234000000,
  "failed": 0,
  "steps": [
    { "name": "containers",  "reclaimedBytes": 100000000 },
    { "name": "images",      "reclaimedBytes": 1134000000 },
    { "name": "build-cache", "reclaimedBytes": 0, "error": "..." }
  ]
}
```

- `steps` is in plan order (containers → images → volumes → build-cache) and
  always an array; `error` is omitted on success. A failed step never stops the
  others.
- Exit `0` only when `failed` is `0`.
- `--all` removes every unused image (default: dangling only); `--until 168h`
  keeps anything newer; `--build-cache` and `--volumes` widen the sweep
  (volumes hold data, so off by default).

## `skrog compact --json`

Shrinks the engine's virtual disk (fstrim + CompactVirtualDisk):

```json
{
  "distro": "skrog-engine",
  "path": "C:\\Users\\me\\AppData\\Local\\Skrog\\distro\\ext4.vhdx",
  "trimmed": true,
  "offeredBytes": 1078939029504,
  "beforeBytes": 15032385536,
  "afterBytes": 9663676416,
  "reclaimedBytes": 5368709120,
  "waitedSeconds": 66.4,
  "restarted": true,
  "dryRun": false
}
```

- `reclaimedBytes` is the difference in the file's size **on disk** and the only
  honest measure of what happened.
- `offeredBytes` is what `fstrim` printed: the free extent of the whole virtual
  disk, **not** space reclaimed. Named "offered" so nothing mistakes it for a
  result; omitted when `--no-trim` was used.
- `held` is present when other distros are keeping WSL from releasing the disk,
  and pairs with exit code **11**. Under `--dry-run` it appears without an
  error: the dry run reports that a real run would refuse, and names who.
- `waitedSeconds` is how long WSL took to let go (about a minute after the last
  distro stops).
- Exit codes: `0` ok, `1` error, `2` usage, `3` not installed, `11` the disk is
  held.

## `skrog relocate --json`

Moves the engine's data directory to another drive:

```json
{
  "distro": "skrog-engine",
  "from": "C:\Users\me\AppData\Local\Skrog\distro",
  "to": "D:\skrog",
  "movedBytes": 439422976,
  "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "needBytes": 1098907648,
  "freeBytes": 181070462976,
  "restarted": true,
  "dryRun": false
}
```

- `movedBytes` and `sha256` describe the transfer archive: the engine is
  exported, checksummed, and only then is the old distro unregistered, so the
  archive is the recovery point if the import fails.
- `needBytes` is the **peak** requirement on the target, roughly twice the
  current disk, because the archive and the imported disk exist at the same
  time. Checked before anything is touched; short space pairs with exit
  code **12** and changes nothing.
- Under `--dry-run` the payload is `steps` plus `needBytes`/`freeBytes` — what
  it would do and whether it would fit.
- `archiveKept` appears with `--keep-archive`, or when the archive could not be
  removed, and names the file to delete by hand.
- Exit codes: `0` ok, `1` error, `2` usage, `3` not installed, `12` not enough
  space.

## `skrog policy show --json` / `skrog policy test --json`

Admission control (#120). `show` reports the rules in effect:

```json
{
  "path": "C:\Users\me\AppData\Local\Skrog\policy.yaml",
  "exists": true,
  "active": true,
  "enforced": true,
  "rules": { "denyPrivileged": true, "allowBindSources": ["C:\work"] }
}
```

`test` is the verdict on one container-create body, for gating a script:

```json
{
  "denied": true,
  "rule": "allow-bind-sources",
  "reason": "policy does not allow bind mounts from C:/secrets (allowed: C:\work)"
}
```

- `rule` names the key that refused, so a script can branch on it without
  parsing prose; `reason` is the same sentence the user would see.
- `active`/`enforced` are false when the file is absent or sets no rules —
  both mean every request is allowed.
- Exit codes: `0` allowed, `1` error, `2` usage, `3` not installed, **`13`**
  the rules deny it. 13 is separate so "refused" is distinguishable from
  "the command went wrong".

## `skrog engine list --json`

What this build can install, what is installed, and where a rollback goes:

```json
{
  "installed": "29.7.2-4",
  "previous": "29.7.2-3",
  "available": [
    { "ref": "29.7.2-4", "version": "29.7.2", "default": true, "published": true }
  ]
}
```

- `ref` is the **revisioned** label and the unit an upgrade moves between;
  `version` is only the dockerd version, and two revisions can share one.
- `published` is `false` for a manifest entry with no checksum yet — a
  placeholder that cannot be installed.
- `installed` and `previous` are omitted when unknown; `available` is always an
  array.

## `skrog engine upgrade --json` / `skrog engine rollback --json`

```json
{
  "from": "29.7.2-3",
  "to": "29.7.2-4",
  "replaced": ["dockerd", "containerd", "runc", "..."],
  "engineVersion": "29.7.2",
  "rolledBack": false,
  "dryRun": false
}
```

- `engineVersion` is what the new dockerd reports about **itself** — evidence
  the swap took, not an assumption that it did.
- `rolledBack: true` with a **non-zero exit** is the interesting case: the
  upgrade failed and the previous engine was restored, so the engine is up.
- `replaced` and `engineVersion` are absent on a dry run.
## `skrog wsl-config show|apply --json`

The WSL2 VM''s sizing, from the global `~/.wslconfig` (#148):

```json
{
  "path": "C:\\Users\\me\\.wslconfig",
  "exists": true,
  "effective": { "memory": "4GB", "processors": "2", "autoMemoryReclaim": "gradual" },
  "desired":   { "memory": "4GB", "processors": "2", "autoMemoryReclaim": "gradual" },
  "pending": [ { "key": "memory", "old": "8GB", "new": "4GB", "added": false } ],
  "applied": false
}
```

- `effective` is what the file says now; `desired` is what Skrog's own
  settings ask for; `pending` is the difference — so a converge script can tell
  "already right" from "would change something" without parsing prose.
- `pending` is omitted when there is nothing to do, which is the signal that a
  repeated `apply --yes` is a no-op.
- `applied` is `true` only when this invocation wrote the file.
- `apply --json` requires `--yes`: there is no way to ask a question in JSON, so
  it exits `2` rather than appearing to hang.
## `skrog status --stats --json`

`--stats` **adds** a `stats` object; the rest of the shape above is unchanged,
because it is a readiness-probe contract. Statistics are opt-in for two
reasons: collecting them costs WSL calls a probe should not pay, and they are
only meaningful when the engine is already running.

```json
{
  "installed": true, "distro": "skrog-engine", "engine": "running",
  "stats": {
    "probed": true,
    "supervisor": {
      "fresh": true, "readingAgeSeconds": 0.2,
      "uptimeSeconds": 3600, "engineUptimeSeconds": 3600,
      "engineStarts": 1, "idleStops": 2,
      "lastIdleStopAt": "2026-09-10T14:00:00Z", "lastWakeAt": "2026-09-10T14:31:00Z"
    },
    "bridge": {
      "connections": 512, "bytesToEngine": 3281, "bytesToClient": 12883,
      "activeConns": 0, "transport": "vsock"
    },
    "engine": {
      "version": "29.7.2", "containers": 1, "containersRunning": 1,
      "containersPaused": 0, "containersStopped": 0,
      "images": 9, "volumes": 2,
      "imagesBytes": 12191537, "volumesBytes": 0,
      "buildCacheBytes": 0, "reclaimableBytes": 490
    },
    "disk": {
      "path": "C:\\Users\\me\\AppData\\Local\\Skrog\\distro\\ext4.vhdx",
      "sizeOnDiskBytes": 415236096, "guestUsedBytes": 305328128,
      "reclaimableBytes": 109907968, "hostFreeBytes": 428330541056
    },
    "vm": {
      "cpus": 20, "memTotalBytes": 33481715712,
      "memAvailableBytes": 31422464000, "swapTotalBytes": 8589934592,
      "configuredMemory": "4GB", "configuredProcessors": "2"
    }
  }
}
```

- **`probed`** says whether the engine was up. Statistics never start it (#82),
  so `probed: false` omits `engine`, `disk` and `vm` entirely rather than
  reporting zeroes — "no data" and "zero containers" must not look alike.
- **`supervisor`** and **`bridge`** come from a file the supervisor flushes, so
  they are present even with the engine down. `fresh` and `readingAgeSeconds`
  say whether they still describe the present: a supervisor that died leaves
  its last numbers behind, and `fresh: false` is how a reader knows not to
  trust them as current.
- **`bridge.transport`** is `vsock` (fast path, ~0.6 ms per connection),
  `fallback` (the socat relay, ~165 ms — the vsock agent is unreachable),
  `socat` (that path pinned by `SKROG_NO_VSOCK`) or `unknown`. This is the
  field that explains a slow `docker` with a perfectly healthy engine.
- **`engine.reclaimableBytes`** is what `skrog prune` could free;
  **`disk.reclaimableBytes`** is size-on-disk minus guest-used, roughly what
  `skrog compact` could return — an estimate, since compaction works in
  blocks.
- **`vm.configured*`** is what `~/.wslconfig` asks for, omitted when it asks
  for nothing. Comparing it with `memTotalBytes` and `cpus` catches the trap
  `skrog wsl-config` closes: a limit recorded and never applied.
- **`errors`** names anything that could not be read, so a partial reading is
  honest rather than silently short.
## The rule for new commands

Anything that gains state reporting must gain `--json` in the same change and
be added here; its shape goes in `cmd/skrog/jsonshapes.go` with a test.
