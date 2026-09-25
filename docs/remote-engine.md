# Remote engine over mutual TLS

By default Skrog's engine is reachable only from the host it runs on, through a
Windows named pipe — nothing listens on the network. `skrog serve` opens a
second door: a TCP listener protected by **mutual TLS**, so a teammate, a second
machine, or a CI runner can point a stock `docker` client at your engine.

Mutual means both sides prove themselves. The server presents a certificate the
client checks, and the client presents one the server checks. Only a client
holding a certificate signed by *this machine's* CA can connect — the engine is
never open to the network at large, even while the port is listening.

```
skrog serve cert --host my-desktop.corp        # once: mint the certificates
skrog serve --tcp 0.0.0.0:2376                 # run the server (foreground)
```

## Minting the certificates

`skrog serve cert` writes three things into `tls/` under the state dir:

- **`ca.pem` / `ca-key.pem`** — a private certificate authority. Generated once
  and **reused** on later runs, so certificates you have already handed out keep
  working. Rotating the CA (delete `ca*.pem` and re-run) invalidates every
  client.
- **`server.pem` / `server-key.pem`** — the engine's certificate, valid for
  `localhost`, `127.0.0.1`, this machine's hostname, and its local IPs. Add every
  other name or address a client will dial with `--host` (repeatable):

  ```
  skrog serve cert --host my-desktop.corp --host 10.1.2.3
  ```
- **`client.pem` / `client-key.pem`** — the certificate you give to whoever
  connects. Name it with `--client <name>` (the certificate's CN) to tell clients
  apart; run the command again with a new name to mint another.

Re-running `serve cert` regenerates the server and client certs against the
existing CA — safe and idempotent. Keep every `*-key.pem` private: anyone with a
signed client certificate can reach the engine.

## Running the server

```
skrog serve --tcp 0.0.0.0:2376
```

Runs in the foreground and relays to the engine through the same bind-path
rewriting the local pipe uses, so remote `docker run -v` behaves as it does
locally. Stop it with Ctrl-C.

The supervisor must be running (`skrog start`). The idle timeout (5 minutes by
default) and remote serving work together
([#520](https://github.com/wslkit/skrog/issues/520)):

- **While a remote client is connected, the engine is never idle-stopped.**
  `skrog serve` tells the supervisor about its own connections, because they
  never cross the supervisor's pipe. That includes a remote `docker build`,
  which runs no containers and so would not keep the engine up on its own.
- **Once the last remote connection closes, the engine idles one full timeout
  later**, as it does for local clients.
- **A remote client connecting to an idle engine wakes it** and waits for it,
  about the length of a cold start, 4–8 s.
- **An engine stopped with `skrog stop` is not woken**: the client gets an error
  saying so.

If remote clients should never wait for a cold start, turn idle stops off:

```
skrog config set idle-timeout off
```

`2376` is the IANA port for the Docker TLS endpoint. Bind `0.0.0.0` to accept
from any interface, or a specific address (e.g. `192.168.1.5:2376`) to limit it.

## Connecting a client

Copy `ca.pem`, `client.pem`, and `client-key.pem` to the client machine, into a
directory of their own, then register the remote — one command:

```
skrog remote --host tcp://my-desktop.corp:2376 --certs C:\path\to\certs add desktop
skrog remote test desktop          # engine version + round-trip time
skrog remote use desktop           # plain `docker` now targets the remote
skrog remote use local             # ...and back to the local engine
```

`add` validates the certificates (a wrong file fails here, not on first
connect), copies them under Skrog's state dir with the key at 0600, and creates
a docker context named **`skrog-desktop`** carrying the TLS material. Because
it is a real docker context, **anything that follows the docker context follows
the remote** — `docker compose`, and VS Code Dev Containers: "Reopen in
Container" builds and runs on the remote engine. `skrog remote` lists remotes
and which one docker is on; `skrog doctor` reports `remote:desktop` and warns
two weeks before the client certificate expires. `skrog remote remove desktop`
removes the context and the copied material.

Both certificate layouts are accepted: what `skrog serve cert` writes
(`ca.pem`, `client.pem`, `client-key.pem`) and docker's own
(`ca.pem`, `cert.pem`, `key.pem`).

### Without `skrog remote`

Any docker client can reach the engine with the standard TLS variables, if you
prefer to wire them yourself. `DOCKER_CERT_PATH` expects **`ca.pem`, `cert.pem`,
`key.pem`**:

```powershell
$env:DOCKER_HOST       = "tcp://my-desktop.corp:2376"
$env:DOCKER_TLS_VERIFY = "1"
$env:DOCKER_CERT_PATH  = "C:\path\to\certs"
docker version
```

or pass the files explicitly:

```
docker --tlsverify --tlscacert ca.pem --tlscert client.pem --tlskey client-key.pem \
       -H tcp://my-desktop.corp:2376 version
```

## Security notes

- **Off by default.** Nothing listens on TCP until you run `skrog serve`, and it
  runs only while that command is in the foreground.
- **No elevation.** Certificates are generated with Go's `crypto/x509`; the
  listener binds an unprivileged high port. Opening the port through the Windows
  firewall to other machines is the one step that may prompt for elevation, and
  that is Windows' firewall, not Skrog.
- **Mutual TLS, not a password.** There are no plaintext secrets. Access is
  revoked by rotating the CA (invalidating all clients) — per-client revocation
  lists are not implemented; mint clients narrowly and rotate when someone leaves.
- **The engine is root-equivalent.** A client that can reach the engine can mount
  any host path the engine can see. Hand out client certificates as carefully as
  you would ssh access.
