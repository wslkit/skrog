# Dev Containers & VS Code

The Dev Containers CLI and the VS Code Dev Containers extension work against the
Skrog engine with **no shim** — Skrog serves the standard Docker API, and its
Windows-path rewriting handles the bind mounts these tools generate.

Validated end to end on **both backends** with Dev Containers CLI 0.89.0 —
`devcontainer up` reporting `"outcome":"success"`, the workspace bind mount
translated transparently, `postCreateCommand` run, `containerEnv` applied,
`devcontainer exec` working, and a write from inside the container visible on
Windows:

| | engine distro | wslc session |
|---|---|---|
| `devcontainer up` | ok | ok |
| workspace mount transport | 9p | **virtiofs** |
| `exec`, `postCreateCommand`, `containerEnv` | ok | ok |
| container write reaches Windows | ok | ok |
| **Features** (anything needing the network at build time) | ok | **no — see below** |
| docker-outside-of-docker, and `docker run` from inside the dev container | ok | blocked by the row above |

### Features on the wslc backend: fixed in WSL 2.9, broken in 2.7

This page used to say Features do not install on the wslc backend. **On WSL
2.9.11 they do**, and the section is kept because the failure is real on older
WSL and the symptom is baffling if you meet it.

**What went wrong on WSL 2.7.x.** Containers in a session were handed the
Windows host's LAN router as their nameserver, and it answered `SERVFAIL` from
inside the session VM — so anything reaching the network during a **build**
failed:

```
curl: (6) Could not resolve host: packages.microsoft.com
```

Routing was fine and the daemon resolved normally; it was specifically the
resolver propagated into containers. `--dns=1.1.1.1` fixed run time and could
not fix build time, because Features install during the build and `runArgs`
applies afterwards.

**Re-tested on WSL 2.9.11** — the same LAN-router resolver, now answering:

```
$ wslc run --rm alpine cat /etc/resolv.conf
nameserver 192.168.1.1
$ wslc build .          # RUN apk add --no-cache curl && curl -sI https://example.com
HTTP/2 200
```

So a Dockerfile or a Feature that reaches the network during a build works.

**If you hit it anyway, check your WSL version first** (`wsl --version`). Below
2.9 the workaround is unchanged: use a base image that already contains what
you need and keep Features for the distro backend. See
[#351](https://github.com/wslkit/skrog/issues/351) for the measurements on both
versions.

## Point them at Skrog

Both use the docker CLI, so anything that selects the Skrog engine works:

**Without Docker Desktop you need neither.** Skrog serves
`\\.\pipe\docker_engine`, the pipe `docker` already talks to, so Dev Containers
finds the engine with no configuration.

Alongside Docker Desktop, Desktop keeps that pipe and Skrog serves its own, so
say which engine you mean:

- **Docker context** (simplest): `docker context use skrog` makes it the
  default for every tool, the CLI and Dev Containers included.
- **`DOCKER_HOST`**: for one shell, asked of Skrog so it is right whichever pipe
  it took — `(skrog status --json | ConvertFrom-Json).endpoint.dockerHost`.
  Plain `skrog status` prints the same endpoint, and so does the install output.

### Dev Containers CLI

```
docker context use skrog
devcontainer up   --workspace-folder .
devcontainer exec --workspace-folder . bash
```

### VS Code

VS Code's Dev Containers extension uses whatever docker context / `DOCKER_HOST`
your environment selects, so without Docker Desktop "Reopen in Container" builds
against the Skrog engine with nothing configured.

Alongside Docker Desktop, select the `skrog` context — or pin it per-workspace
with `"docker.environment": { "DOCKER_HOST": "…" }` in your VS Code settings,
using the host `skrog status` reports rather than a pipe name typed by hand.

## Notes

- Bind mounts with Windows paths (`.` → the workspace) are rewritten by the
  bridge, so nothing special is needed for source-tree mounts.
- The engine is the upstream Docker Engine, so Dev Container **features**,
  Docker Compose-based dev containers, and `docker-in-docker` behave exactly as
  they do on any Linux engine.
