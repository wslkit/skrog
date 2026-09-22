# Published ports

`docker run -p 8080:80` works. Then you try it from your phone, and it does
not.

That gap is the whole subject of this page: under WSL2's default networking a
published port answers on the machine that started it and **nowhere else**, and
nothing in `docker ps`, the container logs or the engine says so.

## What actually happens

Measured on Windows 10 22H2 (19045), WSL 2.9.12, default NAT networking:

```powershell
docker run -d --rm -p 18080:80 nginx:alpine

curl http://localhost:18080        # 200 OK
curl http://192.168.1.121:18080    # connection refused
```

The reason is one line of `netstat`:

```
TCP    127.0.0.1:18080    0.0.0.0:0    LISTENING
```

Nothing is bound on any other address. The chain is:

1. `dockerd` publishes `0.0.0.0:18080` **inside the engine distro**.
2. WSL's own `localhostForwarding` relay, on the Windows side, binds
   **`127.0.0.1` only**.
3. So the port exists on Windows loopback and on no other interface.

This is not a Skrog behaviour and Skrog cannot change it from inside the
distro — it is where WSL chooses to bind.

## Who this bites

- **Testing a web app from your phone**, which is the most common reason anyone
  publishes a port at all
- A colleague opening your dev server
- Testcontainers, an agent or a browser on another host
- Anything that resolves your machine's hostname instead of `localhost`

It is also a change from Docker Desktop, whose port proxy binds all interfaces
— so this reads as "Skrog broke my ports" when what changed is which program is
doing the forwarding. *(Desktop's behaviour here is reasoned from how its proxy
works, not measured on the reference host; see the note in the
[README](https://github.com/wslkit/skrog#what-it-does-today) about not making
unmeasured Desktop comparisons.)*

## `skrog doctor` says so

```
[warn] published ports beyond this machine: port 18080 reachable from this machine only
       dockerd publishes inside the distro and WSL's own forwarder binds 127.0.0.1
       on the Windows side, so nothing else on your network can reach the container.
       Testing a web app from your phone is the usual way people meet this.
       fix: `skrog config set network.publish-scope lan` relays published ports to
            every interface (opt-in: it puts your containers on the network). Or set
            networkingMode=mirrored in ~/.wslconfig, which changes every WSL distro
            on the machine -- see `skrog wsl-config`. Neither is needed if localhost
            is all you use.
```

Silent unless a container is actually publishing something. A warning that
fires on every machine is a warning people learn to skip, and most of the time
`localhost` is all anyone wanted.

## Making the port reachable

### `network.publish-scope`

```powershell
skrog config set network.publish-scope lan
```

The supervisor then watches what the engine publishes and opens a matching
listener on every interface, forwarding into the distro. Listeners appear and
disappear with the containers that published them, so nothing outlives the
thing it was pointing at.

```powershell
docker run -d --rm -p 18080:80 nginx:alpine
curl http://192.168.1.121:18080    # 200 OK
```

Measured on the same machine: the relay binds `0.0.0.0:18080` **and**
`[::]:18080`, so the port answers over IPv6 as well — `http://[::1]:18080`
works too.

Back to the default with:

```powershell
skrog config set network.publish-scope loopback
```

Both take effect within a few seconds, without restarting anything: the
supervisor re-reads the setting on each pass, so turning the scope **off** also
tears the listeners down.

> **This puts your containers on the network.** That is the point, and it is
> why it is opt-in rather than a default. A development database with no
> password, published on a coffee-shop wifi, is reachable by everyone on that
> wifi. Turn it on when you want it, and prefer turning it off again to leaving
> it on because it was convenient once.

**Windows Firewall** prompts the first time a port is bound, or silently
refuses on a managed machine where the policy does not allow it. If the port
binds but nothing outside can reach it, that is where to look first.

**TCP only.** A relay carries streams; a UDP published port is untouched and
stays loopback-only. Publishing one and finding it silently drops every packet
would be worse than not relaying it, so it is not relayed.

**Ports below 1024** need elevation to bind on Windows and the supervisor runs
as you, so those are skipped with a line in `supervisor.log`.

### The other route: mirrored networking

```ini
# ~/.wslconfig
[wsl2]
networkingMode=mirrored
```

WSL then gives the distro the host's own interfaces, and published ports land
on them with no relay involved.

It is a **machine-wide** change: every WSL2 distro you have gets it, along with
different DNS and firewall behaviour. That is the same bar
[`skrog wsl-config`](vm-sizing.md) applies to VM sizing — Skrog will show you
the change and apply it if you ask, and will not do it on your behalf.

If you already run mirrored networking, you need none of this and `skrog
doctor` will say so.

## Which one should you use

| you want | use |
|---|---|
| `localhost` only, which is most of the time | nothing; this is the default |
| one machine's containers reachable on your network | `network.publish-scope lan` |
| every WSL distro to share the host's network | `networkingMode=mirrored` |

`publish-scope` is the narrower tool: it touches Skrog's engine and nothing
else on the machine.

## What this is not

It is not a general port forwarder. It relays exactly what the engine reports
as published, and there is no way to ask it for an arbitrary mapping — a
listener that does not correspond to a running container is a port answering
from somewhere surprising, and the point of tying it to container lifetime is
that it cannot happen.

It also does not change what `docker ps` reports, or how the container sees its
own address. The container is unaware of any of this; only the Windows side of
the mapping changes.

## Related

- **[#507](https://github.com/wslkit/skrog/issues/507)** — the doctor check
- **[#508](https://github.com/wslkit/skrog/issues/508)** — the relay design
- **[#163](https://github.com/wslkit/skrog/issues/163)** — a different port
  failure, worth not confusing with this one: under *mirrored* networking a
  published port used to be unreachable **even from `localhost`**, because
  dockerd's `userland-proxy` broke the return path. Fixed by defaulting
  `engine.userland-proxy` to `false`. There, loopback was broken; here loopback
  is the only thing that works.
