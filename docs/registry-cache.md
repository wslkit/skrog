# Pull-through registry cache

A layer you have pulled before comes off your own disk instead of the internet.

```powershell
skrog cache enable
```

That is the whole setup. Pulls go through it from the next one onward; the
first is no faster, and every repeat is.

## Why you would

**Docker Hub rate-limits anonymous pulls per IP.** Behind a corporate NAT, or
on a fleet of runners sharing an address, the limit is reached by the
*organisation* — and the error lands on whichever developer pulled next. A
local cache turns N pulls of the same layer into one.

**A corporate link is slow, and a TLS-inspecting proxy makes it slower.**
[corporate-network.md](corporate-network.md) covers reaching a registry
through one; this is about only doing it once.

**CI runners re-pull everything.** [`skrog prewarm`](ci-runners.md) already
pre-pulls a pinned list, but that list has to be right in advance. A cache
needs no list — it just stops fetching what it already has.

## What it is

The upstream `registry:2` image in pull-through mode, **pinned by digest**
like everything else here, running as a container on the engine with its store
in the `skrog-cache-data` volume.

A volume rather than a path inside the distro, deliberately: `skrog prune`,
[`skrog compact`](housekeeping.md) and [`skrog relocate`](housekeeping.md)
already account for volumes, so the cache's disk shows up in every tool that
answers "where did my space go".

dockerd reaches it on **loopback** (`http://127.0.0.1:5000`), which Docker
treats as insecure-by-default. Nothing is exposed to the network, and no
`insecure-registries` entry is needed.

## Status

```
$ skrog cache status
cache      running
upstream   https://registry-1.docker.io
mirror     http://127.0.0.1:5000
data       1.4 GiB
```

`--json` gives the same as an object. The field worth watching is `wired`: a
cache that is running while `daemon.json` points nowhere looks healthy and does
nothing, so `status` says so in as many words and `skrog cache enable` rewires
it.

## Caching something other than Docker Hub

One cache proxies exactly one upstream — that is a `registry:2` constraint, not
ours:

```powershell
skrog cache enable --upstream https://ghcr.io
```

It must be a registry **root**. `https://ghcr.io/myorg` is refused at the point
you type it, because the alternative is a cache that 404s every manifest and
looks like a network fault.

## Turning it off

```powershell
skrog cache disable                # removes the container, the mirror and the data
skrog cache disable --keep-data    # keeps the layers for next time
```

`disable` unwires `daemon.json` **before** removing the container, so there is
no window where the engine is pointed at a mirror that has stopped existing.

## Three things it deliberately does not do

**It does not hold the engine awake.** The cache is a long-lived container, and
both [idle-stop](../README.md) and [scheduled prune](housekeeping.md) veto on
"are containers running". Counting it would have silently disabled both — your
engine would hold its RAM forever because of a container you never think of as
work. The busy probe skips it by name; it is infrastructure, not work.

**The mirror is judged by [admission control](policy.md) too.** This section
used to say the cache "cannot become a route to a registry your policy
forbids", and that was wrong ([#421](https://github.com/wslkit/skrog/issues/421)).

A mirror changes where bytes come from, not which image was asked for — that
part is true, and `allow-registries` and `require-digest` do judge the
reference in the request. The conclusion did not follow. Where the bytes come
from is exactly what a registry allowlist is for, and dockerd applies
`registry-mirrors` to every unpinned Docker Hub pull without touching the
reference. So a mirror pointed at a forbidden host served that host's content
under an allowed name, and the reference check passed because the reference was
never the thing in question.

Now:

- `skrog cache enable --upstream <url>` refuses an upstream your
  `allow-registries` does not allow.
- An `http://` upstream is refused unless you pass `--insecure`, and warns
  loudly when you do. A plaintext mirror for Docker Hub is a
  content-substitution position on every unpinned pull on the machine.

What is still true: the cache does not let a *reference* through that policy
would refuse, and it binds to loopback, so nothing is exposed off-host.

**It does not make the engine depend on it.** `registry-mirrors` is a hint:
when the cache is down, dockerd goes to the upstream. A broken cache costs you
speed, not pulls.

## What it does not tell you yet

Hit rate. `registry:2` can expose Prometheus metrics behind a debug endpoint,
and wiring that into [`skrog status --prometheus`](monitoring.md) is the
obvious next step — but reporting a number this version does not actually
measure would be worse than reporting none.
