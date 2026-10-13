# Declarative install (`skrog.yaml`)

Describe a whole install in one YAML file, check it into your provisioning repo,
and install a fleet of runners from it instead of a pile of flags:

```
skrog install --config skrog.yaml
```

Re-running against an existing install **converges** it (idempotent) — it skips
provisioning and re-applies the settings — so the same file drives both the
first install and every later change. To capture an existing machine as a
starting point:

```
skrog config export > skrog.yaml
```

> **It does not capture everything.** The schema carries `distro`, `data-dir`,
> `engine-version`, `idle-timeout`, `engine.*`, `hooks`, `integrations` and
> `autostart`. It does **not** carry `network.proxy`, `network.no-proxy`,
> `network.import-host-cas`, `emulation.platforms`, `gpu`, `gpu.vendor`,
> `audit`, `disk.warn-below`, `install.verify-signature`, `prune.*` or the
> `wsl.*` keys — so an export from a machine configured per
> [corporate-network.md](corporate-network.md) loses the proxy and the CA
> import, with no error at either end. Set those separately (`skrog config
> set`) until the schema covers them.

> Docker Desktop paywalls Settings Management behind Docker Business. This is
> free, and it is just a file.

## Schema

Every field maps to an existing flag or config key; nothing new is invented, and
unknown fields fail loudly rather than being silently ignored.

```yaml
# Which engine (omit for the default shared engine; see targets.md):
target: proj-a                 # a named engine, created if missing and selected

# Install-time (used only on a fresh install):
distro: skrog-engine          # --distro
data-dir: D:\skrog            # --data-dir
engine-version: 29.7.2         # --engine-version

# Settings (applied on install and on every converge):
idle-timeout: 30m              # or "off"
autostart: true               # start the supervisor at logon

engine:                        # engine daemon.json keys (see `skrog config`)
  registry-mirrors: https://mirror.corp.example.com
  insecure-registries: registry.internal:5000
  log-opts: max-size=10m,max-file=3

hooks:                         # lifecycle scripts (see docs/hooks.md)
  post-start: C:\ProgramData\skrog\login.cmd

integrations:                  # distros to wsl-integrate
  - Ubuntu
```

All fields are optional: an absent field is left at its default or untouched, so
a partial file is valid.

### `target:`, a named engine per workspace

With `target: <name>`, the file describes **that engine** rather than the default one
([targets.md](targets.md)). `skrog install --config` creates the engine if it does not
exist, converges every setting below onto it (its own `idle-timeout`, `engine.*` keys,
hooks, integrations), and switches docker to it. Re-running converges.

- `engine-version:` pins the engine's dockerd when it is created; an existing engine is
  not re-created.
- `distro:` and `data-dir:` are rejected beside `target:`: a named engine's locations
  come from its name.
- `--distro`, `--data-dir` and `--state-dir` do not apply to such a file, for the same
  reason.
- `autostart:` is not defaulted on for a named engine, as it is for the default one: an
  engine starts at logon only with an explicit `autostart: true`, and `--no-autostart`
  still wins.
- The key is `target:` and not `engine:` because `engine:` already holds `daemon.json`
  settings.

## CI / fleet example

A GitHub Actions self-hosted runner image, provisioned in one step:

```powershell
# In the runner's setup script:
skrog install --config C:\provisioning\skrog.yaml --headless --no-autostart
skrog start
docker run --rm hello-world
```

- `--headless` never prompts.
- `--no-autostart` suits a runner started by a service manager rather than at
  logon; drop it (or set `autostart: true`) for interactive machines.
- Flags win over the file, so one shared `skrog.yaml` can be specialized per
  host without editing it.

## Pinning the engine (`skrog.lock`)

`skrog.yaml` describes *what to configure*; `skrog.lock` pins *exactly which
engine* — version, rootfs URL, and SHA-256 — so every machine runs a verified,
identical engine:

```
skrog lock > skrog.lock            # capture this build's pinned engine
skrog install --locked skrog.lock  # reproduce it, refusing on any checksum mismatch
```

Check both into your provisioning repo. `--locked` sets the rootfs the same way
the embedded manifest does, so the install stays checksum-verified end to end.

