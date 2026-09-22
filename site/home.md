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

Images built for the *other* architecture run too — the common case on Windows
on ARM, where much of Docker Hub is still amd64-only. One opt-in key, and no
emulator to download:

```
skrog config set emulation.platforms linux/amd64   # or linux/arm64
skrog restart
docker run --rm --platform linux/amd64 alpine uname -m   # x86_64
```

It is opt-in because `binfmt_misc` is kernel state shared by every WSL2 distro
on the machine — [the detail is in the docker CLI
page](https://wslkit.github.io/skrog/docker-cli/#running-a-foreign-architecture-container).

New here? The [README](https://github.com/wslkit/skrog#install) has the
install steps and the current status, and
[how it compares](https://github.com/wslkit/skrog#how-it-compares) says where
Docker Desktop, Rancher Desktop, Podman Desktop and Microsoft's own WSL
container tooling each win — including when to use one of them instead. These
pages are the detail behind them —
each one is a markdown file in
[`docs/`](https://github.com/wslkit/skrog/tree/main/docs), so anything wrong
on this site is a pull request away from being right.

Binaries are not Authenticode-signed yet, so SmartScreen warns on first run. The
[SignPath Foundation](https://signpath.org)'s free programme declined for now — it
is for projects with an established user base — and invited a reapplication as
visibility grows; paying for a certificate is the other route and needs nobody's
approval. Which one, and when, is
[#358](https://github.com/wslkit/skrog/issues/358). The
[code signing policy](https://wslkit.github.io/skrog/code-signing/) says who could produce a signed binary and
how. Every release does carry SLSA build provenance and a cosign-signed
`SHA256SUMS` today — see [verifying a download](https://wslkit.github.io/skrog/security/#verifying-a-download).
