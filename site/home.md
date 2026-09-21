---
title: "Documentation"
---

Skrog runs the **upstream open source Docker Engine** on Windows, inside WSL2.
No licence fees, no Electron, no Kubernetes: install once and `docker ps` works
forever, on laptops and CI runners alike.

```
skrog install     # engine, docker context, autostart at logon
skrog start       # the always-on bridge, now (later logons start it for you)
docker run --rm hello-world
```

New here? The [README](https://github.com/wslkit/skrog#install) has the
install steps and the current status, and
[how it compares](https://github.com/wslkit/skrog#how-it-compares) says where
Docker Desktop, Rancher Desktop, Podman Desktop and Microsoft's own WSL
container tooling each win — including when to use one of them instead. These
pages are the detail behind them —
each one is a markdown file in
[`docs/`](https://github.com/wslkit/skrog/tree/main/docs), so anything wrong
on this site is a pull request away from being right.

Binaries are not Authenticode-signed yet, so SmartScreen warns on first run. Code
signing is applied for through the [SignPath Foundation](https://signpath.org),
with signing by [SignPath.io](https://about.signpath.io); the
[code signing policy](https://wslkit.github.io/skrog/code-signing/) says who can produce a signed binary and
how. Every release does carry SLSA build provenance and a cosign-signed
`SHA256SUMS` today — see [verifying a download](https://wslkit.github.io/skrog/security/#verifying-a-download).
