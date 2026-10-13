# Design: per-workspace engines

Status: **implemented** ([#539](https://github.com/wslkit/skrog/issues/539)),
following the design settled for
[#140](https://github.com/wslkit/skrog/issues/140). The user guide is
[docs/targets.md](../targets.md); this page keeps the reasoning, and the
[implementation notes](#implementation-notes) at the end record where the
shipped version differs from the draft.

The `target` namespace below is **approved and binding** — settled early
because a rename is cheap before `engine` and `target` appear in anyone's
scripts and expensive after.

## The capability

Today Skrog runs one engine (`skrog-engine`) that every project shares. The
ask is "this workspace uses *its own* engine": isolated images, containers and
volumes per project — for security (a poisoned image in project A cannot be
`docker run` from project B), for hygiene (no cross-project image sprawl), and
for reproducibility (project A pins engine 29.7, project B needs 29.9). Docker
Desktop is structurally one global engine; this is a capability it cannot offer.

Skrog is already most of the way there: `install --distro X --state-dir Y`
creates a fully independent engine, and the acceptance suite runs one beside a
real install on every run. What is missing is making that first-class:
naming, switching, discovery, and a workspace file that selects it.

## Decision 1 — one supervisor + pipe per engine (not a multiplexer)

Two models were on the table:

- **A. One supervisor per engine** (today's model, N times). Each engine has its
  own state dir, pipe, supervisor process, docker context. Isolation is total
  and everything that exists today keeps working unchanged.
- **B. One supervisor multiplexing N engines.** One process, one pipe with a
  routing layer, shared idle logic.

**A.** Model B saves one small process per engine and buys a routing layer,
coupled lifecycles (one crash takes down all pipes), and a second implementation
of idle-stop, hooks, audit and health that must stay in step with the first.
Model A reuses the supervisor, reconciler, idle-stop, hooks, audit log, doctor
and the entire `--json` contract per engine for free. The cost — a few MB of RAM
per idle supervisor — is noise next to the engine VMs themselves.

## Decision 2 — engines and remotes share one namespace: *targets*

`skrog remote add desk` already creates docker context `skrog-desk`. A local
per-workspace engine `proj-a` would naturally be `skrog-proj-a`. Rather than two
prefixes and two switch commands, both are **targets**, switched by one verb:

```
skrog use desk          # a remote (registered with `skrog remote add`)
skrog use proj-a        # a local per-workspace engine
skrog use local         # the default shared engine (context: skrog)
```

Names are unique across remotes and engines; `local` is reserved (it already is
for remotes). `skrog remote use` stays as an alias so nothing documented
breaks. The VS Code extension's status bar shows the current target either way.

## Layout

An engine named `<name>` is a complete, independent Skrog install:

| Piece | Default engine | Engine `<name>` |
| --- | --- | --- |
| state dir | `%LOCALAPPDATA%\Skrog` | `%LOCALAPPDATA%\Skrog\engines\<name>` |
| distro | `skrog-engine` | `skrog-<name>` |
| data (VHDX) | `<state>\distro` | `<state>\engines\<name>\distro` |
| pipe | `\\.\pipe\docker_engine` (or Skrog's own) | `\\.\pipe\skrog-<name>` |
| docker context | `skrog` | `skrog-<name>` |
| config, snapshots, audit, logs | per engine, in its state dir | same |

Nothing is shared between engines except the rootfs download cache (the same
pinned tarball, verified once, imported N times).

The per-engine **docker context** in that table is not cosmetic. One shared
`skrog` context across several installs is exactly the collision that
[#217](https://github.com/wslkit/skrog/issues/217) turned out to be: a
second install silently took the context, and uninstalling it deleted the
first install's. That is fixed for the two-install case, but `skrog-<name>`
per engine is what makes the problem structurally impossible rather than
handled.

## CLI shape

The instance verbs live under `target`, not `engine`. `skrog engine` already
ships as the **version** dimension — `engine list` is "which dockerd releases
can this build install", and `engine upgrade|rollback` move between them — so
`engine list` cannot also mean "which engines do I have". Decision 2 already
named the right noun; these are targets.

```
skrog target create <name> [--engine-version <v>] [--locked skrog.lock]
skrog target list [--json]              # remotes + local engines + `local`; state, disk, current
skrog target remove <name> [--yes]      # unregister distro, delete state, drop context
skrog use <name>|local                  # switch docker (and Dev Containers) to it
skrog status --state-dir …              # everything else already works per engine

skrog engine list|upgrade|rollback      # unchanged: the VERSION of the current target
```

Reading the two nouns together: a *target* is which engine you are talking to;
the *engine* commands are which dockerd version that one runs. `target list` is
also the only sensible home for Decision 2's shared namespace, since remotes
are not engines anyone can create.

**Consequence:** `engine upgrade`, `engine rollback` and `config set engine.*`
become scoped to the current target. That keeps `skrog use` the single switch
anybody has to learn, and it is the least surprising rule when more than one
engine exists.

`target create` is `install` into the engine's state dir with the engine's
distro name and pipe, honoring the same pins (`--engine-version`, `--locked`,
`--offline` bundle). `target remove` is `uninstall` scoped to that engine. Both
are thin: the work is in `provision`, which already takes these as options.

## Workspace file

`skrog.yaml` (declarative install, #69) gains one key:

```yaml
target: proj-a          # this workspace uses the engine named proj-a
engine-version: 29.7.2  # …pinned, as today
```

(Drafted as `engine:`; shipped as `target:`, see the implementation notes.)

`skrog install --config skrog.yaml` in that directory creates the engine if
missing and switches to it; the VS Code extension (skrog-vscode #8) reads the
key on workspace open and runs `skrog use`. A workspace without the key uses
`local`, so nothing changes for anyone who does not opt in.

## Lifecycle

- **Start on demand, not at logon.** The default engine keeps its logon
  autostart. A per-workspace engine's supervisor starts when you `skrog use` it
  (or run `skrog start --state-dir …`), and idle-stops like any engine — so ten
  project engines cost RAM only while one is in use. Autostart for a named
  engine is opt-in (`skrog autostart enable --engine <name>`), which means the
  Run key holds one entry per autostarted engine, each pointing at its state dir.
- **Idle-stop per engine** is already how the supervisor works; nothing to add.
- **Docker Desktop coexistence** is unchanged: the default engine's pipe logic
  handles it; named engines always use their own pipe.

## Cost, honestly

Each engine is a WSL distro with its own VHDX: hundreds of MB of base plus
whatever it pulls, per engine. RAM is fine (idle-stop); **disk adds up**, which
is why `skrog prune` and the sparse-VHDX default (#145) matter more once this
lands. `target list` shows per-engine **and total** disk so the sprawl is visible.

Doctor's free-space floor (`disk.warn-below`, #145) needs to follow. It checks
the data volume for *the* engine today; with N engines on one drive the honest
question is the sum, and disk sprawl is precisely the failure this feature
invites. Left as-is, the first symptom would be a full drive.


## Sharing

Two workspaces that name the same engine share it — that is simply the default
engine's behavior with a name. There is no per-engine ACL; isolation is between
engines, not between users of one engine (the pipe ACL is per user as today).

## The tray

The tray is one status dot with a six-item cap (PLAN §03) and today it shows
*the* engine. With N engines it has to mean something specific: the dot follows
the **current target**, and the tooltip names it. That needs no new menu item
and no new decision surface. Anything richer — a list of engines with per-engine
state — is a control panel, which is the scope tripwire.

It also reads the supervisor's published state from one state dir (#192), so
per-engine state dirs mean reading the current target's.

## Phasing

1. **`target create|list|remove` + `use`** — the model, on top of what
   `provision` already supports. Ships with `--json` and e2e (the suite already
   proves a second engine beside a real one; it gains a `skrog use` round trip
   that switches back, since it must not leave the machine's context changed).
2. **`skrog.yaml` `engine:`** and the extension picking it up (skrog-vscode #8).
3. **Per-engine autostart policy**, `target list` disk accounting, and
   doctor's free-space floor across every engine.

## Settled decisions

The three questions this doc opened with are decided. Kept here with their
reasoning, because a decision without its argument gets re-litigated.

**A new target gets the build's default engine version**, exactly as `install`
does — not the shared engine's current version. Inheriting would make the
result depend on invisible local history: the same `target create` on two
machines, or on one machine before and after an engine upgrade, would produce
different engines. Determinism is the brand. Anyone who wants a specific
version pins it, with `--engine-version` or `engine-version:` in
`skrog.yaml`.

**`skrog use` does not export `DOCKER_HOST`.** Two reasons, and the second is
the decisive one. A process cannot set a variable in its parent shell, so this
would have to become something you `eval` — a different and worse UX than a
command that just works. And `DOCKER_HOST` overrides the docker context, which
is what VS Code Dev Containers and the skrog-vscode extension read: setting
it would leave the CLI and the editor disagreeing about which engine you are
on, which is the exact confusion per-workspace engines exist to remove.
Contexts are the contract; `DOCKER_CONTEXT` covers scripts that need to be
explicit.

**No cap on the number of engines — visibility instead**, on the condition
that visibility is real: `target list` shows per-engine *and total* disk, and
doctor's free-space floor (`disk.warn-below`, #145) accounts for every engine
rather than one. A cap would be an arbitrary number a fleet eventually needs
to exceed; sprawl you can see is a problem you can act on. Both conditions are
in the phasing below.

## Sequencing: why it waited

*Historical: this is the reasoning that held the build back until #539.* The
design was ready, and building it was held for two reasons that were about the
rest of the project rather than this feature.

**The CLI surface is not settled yet.** This adds a second top-level noun
(`target`, plus `use`). The naming collisions found in a single day of work —
`install` versus `engine upgrade`, `update` versus `upgrade`, `engine list`
meaning two different things in the first draft of this very document — say
that the vocabulary needs to stop moving before another noun joins it.
Introducing `target` before then is how the next collision happens.

Narrower than it was, but still standing. Settled since: `upgrade` is the verb
at both scopes ([#191](https://github.com/wslkit/skrog/issues/191), shipped),
the `target` rename above is approved, and
[#209](https://github.com/wslkit/skrog/issues/209) now generates the command
reference from the binary, so a future collision is visible in a diff. Still
open: #77 (package IDs) and #1 (the product name), which are what finally fix
the vocabulary.

**~~Per-engine supervisors multiply an existing hole.~~ Cleared.** This blocker
is gone: [#202](https://github.com/wslkit/skrog/issues/202) landed with both
halves — settings are re-read live wherever they are consumed, and `skrog
restart --supervisor` replaces the supervisor process. One supervisor per
engine no longer multiplies a gap, because the gap is closed.

The original reasoning, kept because it is why the order mattered: Decision 1
means one supervisor per engine, and #202 recorded that *nothing restarted even
one*: `skrog restart` bounces the engine, not the supervisor. Whatever was
decided there — a supervisor-restart command, live-reloading settings, or
both — had to land first, or this feature would have turned one awkward gap
into N of them. It was both.

**The piece worth building early**, if movement is wanted before either
clears, is `target list` on its own: read-only, showing remotes, local engines
and `local` with disk and the current target marked. It makes Decision 2
visible, breaks nothing, and would show whether the `target` vocabulary reads
well before anything depends on it.

## Implementation notes

What shipped in [#539](https://github.com/wslkit/skrog/issues/539), and the few
places it differs from the draft above.

- **The workspace key is `target:`, not `engine:`.** `skrog.yaml` already uses
  `engine:` for a map of `daemon.json` settings, so `engine: proj-a` would have
  been a string and a map under one key. `target:` is also the noun the CLI settled
  on. `distro:` and `data-dir:` are rejected beside it, since an engine's locations
  derive from its name.
- **Names are lowercase only**: letters, digits, `-` and `_`, 1 to 32 characters,
  starting and ending with a letter or digit. The name is a
  directory, a distro, a docker context and a pipe, and Windows is case-insensitive
  for three of those. `local` and `engine` are reserved (`skrog-engine` is the
  default engine's distro).
- **The supervisor derives its pipe and context from its state dir**
  (`<base>\engines\<name>` means pipe `\.\pipe\skrog-<name>` and context
  `skrog-<name>`, via `internal/target`), rather than being told. `skrog start`, the
  logon entry and the watchdog's restart all launch it with only `--state-dir`, so
  nothing else has to carry the pipe and nothing can disagree after a crash. An
  explicit `--pipe` still wins. A named engine's supervisor never touches the shared
  `skrog` context (#217).
- **"Current target" is the docker context**, per "contexts are the contract".
  `engine`, `config` and `status` resolve a missing `--state-dir` to the current
  target; the lookup is skipped entirely on a machine with no named engines, so the
  common case pays nothing. The design scoped only `engine` and `config`; `status` was
  added because the tray reads it to learn which engine the dot describes.
- **The rootfs download cache is shared** (`provision.Options.CacheDir`), which the
  design listed as the one thing engines share. `engine upgrade` still caches under the
  engine's own directory.
- **Created stopped.** `target create` stops the engine it just installed: with no
  supervisor yet, nothing would ever idle-stop it. `skrog use` starts it.
- **Per-engine logon entries** are separate Run-key values (`Skrog-<name>`, opt-in), so
  the default engine's single `Skrog` entry keeps its meaning.
- **Disk accounting** is `target list`'s per-engine and total columns and doctor's
  free-space check across every engine's volume, as the design required in place of a
  cap.
- **Known limit, not fixed:** emulation (`binfmt_misc`) is kernel state shared by every
  WSL2 distro, and stopping or removing any engine deregisters the handlers for all of
  them. With several engines running at once, an idle-stop of one can take foreign
  architecture support away from another until an engine next starts.

### Sequencing

The draft held this back until the CLI vocabulary settled (#77 package IDs, #1 the
product name). It was built with the `target` namespace as approved; those two remain
open and do not touch it.
