# Per-workspace engines (targets)

By default every project on the machine shares one engine: one pool of images,
containers and volumes. That is what Docker Desktop does too, and it has three
costs:

- **Trust.** An image pulled while working on project A can be `docker run` from
  project B. A poisoned base image, or a container you forgot, is in reach of
  everything else you do.
- **Hygiene.** Images and volumes pile up across projects, and nothing says which
  project owns which.
- **Reproducibility.** Project A needs dockerd 29.7, project B needs 29.9, and
  there is one engine.

A **named engine** is a second, third or tenth Skrog engine, each completely
independent of the others. Docker Desktop is one global engine and cannot do this.

```powershell
skrog target create proj-a            # a new, isolated engine; created stopped
skrog use proj-a                      # start it and point docker at it
docker run --rm hello-world           # runs on proj-a's dockerd
skrog use local                       # back to the shared engine
```

## What "independent" means

An engine named `proj-a` is a complete Skrog install of its own:

| | the default engine | engine `proj-a` |
|---|---|---|
| WSL distro | `skrog-engine` | `skrog-proj-a` |
| state directory | `%LOCALAPPDATA%\Skrog` | `%LOCALAPPDATA%\Skrog\engines\proj-a` |
| virtual disk (VHDX) | `<state>\distro` | `<state>\engines\proj-a\distro` |
| named pipe | `\\.\pipe\docker_engine` (or Skrog's own) | `\\.\pipe\skrog-proj-a` |
| docker context | `skrog` | `skrog-proj-a` |
| settings, snapshots, audit log, supervisor log | in its state dir | in its state dir |
| supervisor process | one | one per running engine |

Its images, containers, volumes and networks are its own. Nothing is shared between
engines except the verified rootfs download: a second engine imports the tarball the
first one already fetched, rather than downloading it again (the checksum is checked
on every use).

## Targets

A **target** is *which engine docker talks to*. There are three kinds, and `skrog use`
switches between all of them:

| kind | what it is | created by |
|---|---|---|
| `local` | the default shared engine (docker context `skrog`) | `skrog install` |
| `engine` | a named local engine (context `skrog-<name>`) | `skrog target create` |
| `remote` | an engine on another machine, over mutual TLS (context `skrog-<name>`) | `skrog remote add` |

They share one namespace, because they share the `skrog-<name>` context shape: a name
cannot be both an engine and a remote, and `local` is reserved. `engine` is reserved
too, since `skrog-engine` is the default engine's distro.

`skrog engine` and `skrog target` answer different questions. `skrog target` is
*which engine*; `skrog engine list|upgrade|rollback` is *which dockerd version* the
current one runs.

## Commands

### `skrog target create <name>`

Installs a new engine and leaves it **stopped**. It starts the first time you
`skrog use` it and stops itself when idle (the same [idle-timeout](housekeeping.md)
as any engine), so ten engines cost memory only while one is in use.

```powershell
skrog target create proj-a
skrog target create proj-b --engine-version 29.7.2     # pin its dockerd
skrog target create proj-c --locked skrog.lock         # exactly the engine a lock names
skrog target create proj-d --offline skrog-bundle.zip  # from an air-gap bundle
skrog target create proj-e --autostart                 # also start it at logon
```

It takes the same engine sources as `skrog install`: `--engine-version`, `--locked`,
`--offline`, or `--rootfs-url` with `--rootfs-sha256`. A new engine gets **this
build's default engine version**, not whatever the shared engine has been upgraded to,
so the same command on two machines creates the same engine. Pin a version when you
want a different one.

Names are lowercase letters, digits, `-` and `_` (1 to 32 characters), starting and
ending with a letter or digit. Lowercase only, because the name becomes a directory, a
distro, a context and a pipe, and Windows would treat `Proj` and `proj` as one.

### `skrog use <name>|local`

Points docker at a target. For a local engine it starts the engine first, so the next
docker command finds it ready, and a start that fails leaves docker where it was.

`skrog use` switches the docker **context**, and nothing else. It does not export
`DOCKER_HOST`, deliberately: a process cannot set a variable in its parent shell, and
`DOCKER_HOST` overrides the context, which is what VS Code Dev Containers and the VS Code
extension read. Setting it would leave your terminal and your editor on different
engines, which is the confusion this feature exists to remove. For a script that must
name an engine explicitly, use `DOCKER_CONTEXT=skrog-proj-a`.

`skrog remote use <name>` still works and is the same command.

### `skrog target` / `skrog target list`

```
   NAME    KIND    STATE    ENGINE  DISK      WHERE
   local   local   running  29.9.0  1.3 GiB   C:\Users\me\AppData\Local\Skrog
 * proj-a  engine  running  29.9.0  612.0 MiB C:\Users\me\AppData\Local\Skrog\engines\proj-a
   proj-b  engine  stopped  29.7.2  538.0 MiB C:\Users\me\AppData\Local\Skrog\engines\proj-b
   desk    remote                             tcp://desk.corp:2376

total disk across local engines: 2.4 GiB
```

`*` marks the target docker is on now. `STATE` is `running`, `stopped`, `incomplete`
(a create that died halfway, or a distro removed by hand; `target remove` cleans it up)
or `not-installed`. `--json` is the same data, documented in
[cli-json.md](cli-json.md#skrog-target-json).

### `skrog target remove <name>`

Deletes the engine: stops its supervisor, removes its logon entry, unwires any
`wsl-integrate` distros, removes its docker context (moving docker back to the shared
engine first if it was on this one), unregisters the distro, and deletes its state
directory. **Every image, container and volume in it goes.** The default engine and
your remotes are never touched. `--yes` skips the prompt.

## A workspace file: `target:`

`skrog.yaml` ([declarative install](declarative-install.md)) has a `target:` key that
names the engine a workspace uses:

```yaml
target: proj-a
engine-version: 29.7.2
idle-timeout: 15m
engine:
  registry-mirrors: https://mirror.corp.example.com
```

`skrog install --config skrog.yaml` creates the engine if it does not exist, converges
the file's settings **onto that engine**, and switches docker to it. Run again, it
converges. A file without `target:` behaves exactly as before.

The key is `target:`, not `engine:`, because `engine:` already holds `daemon.json`
settings. `distro:` and `data-dir:` are rejected in a file that sets `target:`, since a
named engine's locations come from its name.

## Which commands act on which engine

`skrog engine upgrade|rollback|list`, `skrog config` and `skrog status` act on the
**current target**, the engine docker is on, so they change and report the engine you
are talking to rather than a different one. On a machine with no named engines this
costs nothing and behaves as it always did.

Every other command (`start`, `stop`, `restart`, `snapshot`, `prune`, `compact`,
`doctor`, `logs`, ...) acts on the default engine unless you pass `--state-dir`. The
`WHERE` column of `skrog target list` is the path to give it:

```powershell
skrog stop     --state-dir "$env:LOCALAPPDATA\Skrog\engines\proj-a"
skrog snapshot --state-dir "$env:LOCALAPPDATA\Skrog\engines\proj-a" save before-upgrade
```

`--state-dir` always wins when you give it. Flags go before the verb for `skrog target`.

## Starting at logon

The default engine starts at logon. A named engine does not: it starts when you `use`
it. If you want one up at logon:

```powershell
skrog target create proj-a --autostart
skrog autostart --engine proj-a enable      # or later
skrog autostart --engine proj-a disable
skrog autostart --engine proj-a status
```

Each engine's logon entry is its own value in the per-user Run key (`Skrog-proj-a`,
visible in Task Manager's Startup tab), so enabling or removing one never touches the
default engine's or another engine's.

## Uninstalling

`skrog uninstall` removes the default engine **and every named engine under the same state
directory**, and its prompt lists them, so "removes everything Skrog created" stays true.
To remove one engine and keep the rest, use `skrog target remove <name>`. Running
`skrog uninstall` against a named engine's own state directory is refused for that reason.

## The tray

The tray's dot follows the **current target**, and its tooltip names the engine when it
is a named one. Its Start, Stop and Restart items, and Open logs, act on the engine the
dot is showing. It stays the same seven items; there is no engine list in it. A list of
engines with per-engine state is a control panel, and `skrog target list` is that.

## Disk

RAM is not the cost of a second engine; **disk** is. Each engine is a WSL distro with
its own virtual disk: hundreds of MB before anything is pulled, plus whatever it holds.
There is no cap on the number of engines. Instead the cost is kept visible:

- `skrog target list` shows each engine's disk and the total.
- `skrog doctor` judges the free-space floor ([`disk.warn-below`](housekeeping.md))
  against the fullest volume **any** engine lives on, lists what each engine holds, and
  points at `skrog target remove` when space is short.
- `skrog prune` goes through whatever docker targets, so it prunes the current target;
  `skrog compact --state-dir <engine's dir>` shrinks that engine's virtual disk.

## Limits to know about

- **Emulation is machine-wide.** `emulation.platforms` registers `binfmt_misc` handlers,
  which are kernel state shared by every WSL2 distro. Stopping or removing *any* engine
  removes them, so with several engines running, an idle-stop of one can take the
  handlers away from another until an engine next starts. Foreign-architecture builds
  that need no emulation setting are not affected.
- **One published-port relay per engine.** `network.publish-scope` is a per-engine
  setting like any other; two engines publishing the same host port conflict exactly as
  two containers would.
- **A named engine does not take over the `docker_engine` pipe.** Only the default
  engine ever serves it. Named engines always use `\\.\pipe\skrog-<name>`, so they
  coexist with Docker Desktop with no fallback logic.
- **The VS Code extension and other tools** follow the docker context, so they follow
  `skrog use`. Anything pinned to `DOCKER_HOST` stays on the engine it names.

## Related

- [Declarative install](declarative-install.md): the `target:` key
- [Remote engine over mTLS](remote-engine.md): the third kind of target
- [Engine upgrade and rollback](engine-upgrade.md): per-target dockerd versions
- [Housekeeping](housekeeping.md): prune, compact, and the free-space floor
- [Design: per-workspace engines](design/per-workspace-engine.md): why it is shaped this way
