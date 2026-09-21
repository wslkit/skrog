# Dev Containers & VS Code

The Dev Containers CLI and the VS Code Dev Containers extension work against the
Skrog engine with **no shim** — Skrog serves the standard Docker API, and its
Windows-path rewriting handles the bind mounts these tools generate.

Validated end to end with Dev Containers CLI 0.89.0 — `devcontainer up`
reporting `"outcome":"success"`, the workspace bind mount translated
transparently, `postCreateCommand` run, `containerEnv` applied, `devcontainer
exec` working, and a write from inside the container visible on Windows.
Features, docker-outside-of-docker and `docker run` from inside the dev
container all work.

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
