# Admission control

A local, scriptable guardrail on the docker API. Skrog already sits in the
request path — every `docker` call crosses the named-pipe bridge — so it can
refuse a container this machine's owner has ruled out, before the engine ever
sees it.

```
skrog policy show                       # the rules in effect
skrog policy check                      # validate the file without applying it
skrog policy test create-body.json      # judge one request, and say why
```

## Two layers, and only one of them is yours

Rules come from two files:

| | Where | Who can write it |
| --- | --- | --- |
| **Machine** | `%ProgramData%\skrog\policy.yaml` | whoever the directory's ACL allows — see [what it is not](#what-it-is-not) |
| **You** | `policy.yaml` in the state dir (`skrog policy show` prints the path) | you |

They merge with one rule: **the user layer may only tighten.** You can forbid
more than the machine does. You cannot permit anything it forbids.

```
$ skrog policy show
machine rules: C:\ProgramData\skrog\policy.yaml  (deployed machine-wide; you cannot loosen these)
  deny --privileged
  bind mounts only from: C:\work

your rules: C:\Users\you\AppData\Local\Skrog\policy.yaml
  images must be pinned by digest

in effect (machine rules, tightened by yours):
  deny --privileged
  bind mounts only from: C:\work
  images must be pinned by digest
```

Most machines have no machine file, and on those nothing above changes: one
file, one layer, as before.

### How the merge works

- **Deny rules OR.** A machine `deny-privileged: true` cannot be un-denied.
- **`deny-capabilities` unions.** Both lists are forbidden.
- **Allow-lists intersect**, which is the subtle one. An allow-list is a
  *permission*, so tightening means keeping fewer entries: a user entry
  survives only if the machine layer already permitted it.

That last point is worth an example, because "intersect" is not quite set
intersection. With machine `allow-bind-sources: [C:\work]`:

| Your file says | In effect | Why |
| --- | --- | --- |
| nothing | `C:\work` | the machine's |
| `C:\work\proj` | `C:\work\proj` | it is *under* a machine root, so it was already permitted |
| `D:\other` | `C:\work` | nothing you asked for was permitted, so you get the machine's list |

A tightening step never produces an *empty* allow-list, because empty means
"no restriction" — the one value that grants more than it looks like.

## What it is not

**Not a security boundary against a local ADMINISTRATOR.** They can edit the
machine file, or uninstall Skrog. Nothing here survives someone who owns the
box outright.

**Not a boundary against a standard user either.** This section used to claim
the opposite — that a standard user "cannot write to `%ProgramData%\skrog`,
cannot loosen what is there, and cannot make `skrog policy show` lie about
it." That was wrong on every clause, and it is the kind of wrong that matters,
because it is the sentence a security team would have relied on:

- **The directory is not administrator-only by default.** `C:\ProgramData`
  ships with `BUILTIN\Users:(CI)(WD,AD)` and `CREATOR OWNER:(OI)(CI)(IO)(F)`,
  so on any machine where an administrator has not already created
  `ProgramData\skrog`, a standard user can create it first and own it outright.
  Skrog does not check the owner or the ACL before reading the file.
- **The location can be redirected.** `SKROG_MACHINE_POLICY_DIR` overrides it,
  and the supervisor runs as the ordinary user, who owns their own environment
  block. `setx` is enough.
- **The gate is not the only route to the engine.** The distro is registered in
  the user's own WSL installation, so `wsl -d <distro> -u root` reaches
  `/var/run/docker.sock` with nothing in the way. `skrog proxy
  --no-path-translation` serves the pipe with the HTTP layer, gate included,
  switched off. `skrog wsl-integrate` shares the socket at mode `0666`.

What the machine layer honestly is: **tamper-evident fleet configuration,
enforced at the pipe.** It stops a user from casually loosening the rules by
editing their own `policy.yaml` — which is the realistic accident on a managed
laptop — and `skrog policy show` tells you what is in force. It does not stop
someone who sets out to get around it. Treat it as configuration management,
not as access control. The tracking issue for closing the gaps above is
[#418](https://github.com/wslkit/skrog/issues/418).

**Not a rule language.** No Rego, no expressions. The vocabulary is small and
fixed so a reader can tell at a glance what is forbidden — which is most of
the value of writing a policy down.

## The rules

They live in `policy.yaml` in the state dir (`skrog policy show` prints the
path). A missing file means no rules, which is the default state of a machine
nobody has configured.

```yaml
deny-privileged: true                 # refuse --privileged
deny-added-capabilities: true         # refuse any --cap-add
deny-capabilities: [SYS_ADMIN]        # ...or only these
deny-host-namespaces: true            # refuse --network/--pid/--ipc/--uts=host
allow-bind-sources:                   # bind mounts may only come from here
  - C:\work
allow-registries:                     # images may only come from here
  - registry.example.com
  - "*.internal"
require-digest: true                  # images must be pinned by digest
deny-unattributable-builds: true      # refuse `docker build` while the above is set
```

Changing the file takes effect on the **next container create** — no
restart, nothing to reload. Creating the file for the first time works the
same way, and deleting it removes every rule.

> An earlier draft read the file once when the supervisor started and told you
> to run `skrog restart`. That was wrong twice: `restart` bounces the engine,
> not the supervisor that holds the rules, so the advice did not work even
> when followed — and a policy file written after the supervisor started
> installed no rules at all. Both were found by running a real
> `docker run --privileged` against a machine that had just been told to deny
> it, and watching it succeed.

### Notes that matter in practice

**`deny-capabilities` is spelling-insensitive.** `SYS_ADMIN`, `sys_admin` and
`CAP_SYS_ADMIN` are the same capability; a rule that only caught one spelling
would be trivially bypassed.

**`allow-bind-sources` matches path prefixes at a boundary.** `C:\work` allows
`C:\work\proj` but not `C:\workshop`. Case and separators do not matter, so
`c:/work` is the same root. **Named volumes are not binds** — `-v myvol:/data`
carries only a name at create time, so this rule does not apply to it.

> **But a volume that names a host path is judged as one.** A `local`-driver
> volume *can* point at a directory:
>
> ```
> docker volume create -d local -o type=none -o o=bind -o device=/mnt/c/secrets esc
> ```
>
> `POST /volumes/create` was not judged at all until
> [#419](https://github.com/wslkit/skrog/issues/419), so that volume — and any
> container later mounting it — reached a directory the rule would have
> refused. The container create that follows carries only the volume's *name*,
> and a name is not a path, so nothing downstream could catch it either.
>
> It is judged now. The `device` is a guest path, so it is mapped back from
> `/mnt/<drive>/...` to Windows form before being compared against the allowed
> roots. Two consequences worth knowing:
>
> - **A device that is not under a Windows drive is refused** — `device=/`,
>   `/etc`, `/var/lib/docker`. They are under no allowed root, and refusing is
>   the point of the rule.
> - **A third-party volume driver is refused while this rule is in force**,
>   because its options are its own vocabulary and Skrog cannot tell whether
>   they name a host path. Saying "checked" would be a lie. The `local` driver
>   — the default, and what `docker volume create` uses unless told otherwise —
>   is unaffected.

**`allow-registries` blocks Docker Hub unless you list it.** Docker's own rule
is that the first component of an image reference is a registry only if it
looks like a host, so `ubuntu` and `library/ubuntu` are both `docker.io`. A
bare hostname in the allowlist matches that host on **any port**; write
`host:5000` if you mean only that port. A leading `*.` matches a whole domain.

**`require-digest`** refuses anything without `@sha256:` — including
`ubuntu:24.04`, because a tag can move.

### Which calls each rule judges

The image rules apply at **pull and push**, not only at container create. That
matters: gating only `create` would stop a blocked image *running* while still
letting it be *fetched onto the machine*, which is not what either rule says.

| rule | judged on |
|---|---|
| `deny-privileged`, `deny-added-capabilities`, `deny-capabilities`, `deny-host-namespaces`, `allow-bind-sources` | `create` / `run` |
| `allow-registries` | `create` / `run`, **`pull`**, **`push`** |
| `require-digest` | `create` / `run`, **`pull`** |

`require-digest` does **not** apply to a push. It exists to stop unpinned images
being *consumed*; a push publishes something you just built, and requiring a
digest there would refuse every ordinary `docker push app:v1`.

> **This is a behaviour change if you already use `allow-registries` or
> `require-digest`.** A `docker pull` that worked before is now refused on both
> backends, at the pull rather than at the container create. That is the rule
> doing what it always said, but it will look new.

### What `allow-registries` does not stop

**A build.** A Dockerfile's `FROM` and any `RUN` can reach any registry, and at
the pipe a BuildKit build is an opaque gRPC stream, so a build cannot be
attributed to a registry in advance. `policy.yaml` lets builds through rather
than refusing them.

That is a deliberate default, not an oversight. An administrator's deployed
WSL policy *does* refuse builds while `WSLContainerRegistryAllowlist` is
active — but that policy arrives by GPO against a user who cannot edit it,
where failing closed is the only coherent answer. `policy.yaml` is your own
file: refusing every build on a machine that merely lists its registries would
break working setups to close a hole its author can walk around by editing one
line.

**If you want the strict reading, ask for it:**

```yaml
allow-registries:
  - registry.example.com
deny-unattributable-builds: true
```

`docker build` is then refused outright, with the same reasoning the
administrator's policy applies by default — reached here because you chose it
rather than because we assumed it.

The rule is **inert without `allow-registries`**, and `skrog policy show` says
so rather than letting you believe otherwise:

```
deny unattributable builds — INERT: it needs allow-registries to bite
```

Refusing every build on a machine with no registry restriction would be closing
a hole that is not open.

Worth knowing if you deploy a [machine layer](#two-layers-and-only-one-of-them-is-yours):
the machine can set `deny-unattributable-builds` while a user's own file
supplies the `allow-registries` that makes it bite. That is the intended
outcome — the administrator said "no unattributable builds where images are
restricted", and they are.

**Swarm services.** `POST /services/create` and `/swarm/init` run an image
from a TaskSpec this gate does not parse, so they cannot be attributed to a
registry. They are refused only when `deny-unattributable-builds` is on — which
is off by default. Refusing beats parsing a TaskSpec and getting it subtly
wrong, which is how two earlier bypasses happened.

> **Plugins used to be in this list, and should not have been**
> ([#420](https://github.com/wslkit/skrog/issues/420)). `docker plugin install`
> names its registry in the request, so it *is* attributable — and because it
> was lumped in with swarm it inherited the permissive **build** default, so a
> plain `allow-registries` let `docker plugin install evil.example.com/p`
> through. A plugin gets host device and mount access where an image gets a
> container, which made it a worse hole than the build one the default was
> chosen to tolerate.
>
> **`allow-registries` now applies to plugins**, with no opt-in, exactly as it
> does to `docker pull` — including `require-digest` if you have set it. A
> plugin from a listed registry installs as before. A plugin pull that somehow
> names no registry keeps the old conservative treatment rather than passing
> unjudged.

**A registry mirror.** `skrog cache enable --upstream <url>` wires
`registry-mirrors` into the engine, and the upstream is not checked against
`allow-registries` ([#421](https://github.com/wslkit/skrog/issues/421)). The
image *reference* is unchanged, so the rule passes; the bytes come from
wherever the mirror points.

**The network.** This is admission control at the Docker API. A running
container can reach any registry it likes, and `docker load` plus `docker tag`
will launder an image past a reference-based rule
([#343](https://github.com/wslkit/skrog/issues/343)). The rules are a guardrail
against mistakes, which is the claim this page has always made.

## What a denial looks like

The bridge answers **403** with the reason, and the docker CLI prints it
verbatim:

```
$ docker run --privileged ubuntu
docker: Error response from daemon: policy denies --privileged: it turns off
container isolation wholesale
```

403 rather than 400 is deliberate: the request is well-formed, this machine
simply will not run it. Every denial is also recorded by the
[audit log](audit.md) when it is enabled.

## Testing a rule set before trusting it

`skrog policy test` judges a container-create body without running anything,
and exits **13** when the rules refuse it — so it can gate a script.

```
$ skrog policy test --rules policy.yaml request.json
DENIED by allow-bind-sources
  policy does not allow bind mounts from C:/secrets (allowed: C:\work)

$ skrog policy test --json --rules policy.yaml request.json
{
  "denied": true,
  "rule": "allow-bind-sources",
  "reason": "policy does not allow bind mounts from C:/secrets (allowed: C:\\work)"
}
```

The body is the JSON the docker CLI POSTs to `/containers/create`. The easiest
way to capture a real one is the [audit log](audit.md); otherwise hand-write
the fields the rule cares about.

## Failure direction

A rules file that will not parse is an **error**, not an empty policy. The
supervisor logs it loudly and runs with admission control **off** rather than
silently enforcing nothing while looking enforced — a guardrail that quietly
fails open is worse than none, because it is believed.

Unknown keys are refused for the same reason: a misspelled rule that silently
does nothing looks exactly like one that works.

```
$ skrog policy check
skrog: policy: yaml: unmarshal errors:
  line 1: field deny-priviliged not found in type policy.Rules
```

Run `skrog policy check` after editing, before restarting.

## Deploying the machine layer

It is one file, so anything that puts a file on a machine will do: Intune, an
Ansible task, a Packer provisioner, Group Policy Preferences, or a line in
your golden-image script. `contrib/` already has
[Ansible and Packer](../contrib/README.md) examples to hang it off.

```powershell
# elevated
$dir = "$env:ProgramData\skrog"
New-Item -ItemType Directory -Force $dir | Out-Null

# Set the ACL explicitly. ProgramData's default gives Users (CI)(WD,AD) and
# CREATOR OWNER full control of what they create, so a directory that skrog
# or a user created first is NOT administrator-only -- and skrog does not
# check (#418). Deploy this before anyone runs skrog on the machine.
icacls $dir /inheritance:r `
  /grant "*S-1-5-18:(OI)(CI)F" `
  /grant "*S-1-5-32-544:(OI)(CI)F" `
  /grant "*S-1-5-32-545:(OI)(CI)RX" | Out-Null
$acl = Get-Acl $dir
$acl.SetOwner([System.Security.Principal.SecurityIdentifier]"S-1-5-32-544")
Set-Acl $dir $acl

Set-Content "$dir\policy.yaml" @'
deny-privileged: true
deny-host-namespaces: true
allow-registries:
  - registry.example.com
'@
```

The SIDs are used rather than names so the snippet works on a non-English
Windows: `S-1-5-18` is SYSTEM, `S-1-5-32-544` Administrators, `S-1-5-32-545`
Users (read + execute, which is what every user needs to have the rules
applied to them).

Check it before you ship it to a fleet — `skrog policy check` validates a file,
and `skrog policy test` judges a real request against the *effective* rules:

```powershell
echo '{"Image":"ubuntu","HostConfig":{"Privileged":true}}' | skrog policy test -
# DENIED by deny-privileged
```

A machine file that does not parse is an **error**, not an empty layer: a typo
in a deployment must not quietly turn into an unenforced machine. The
supervisor keeps the rules that were working and says so.

### Not yet: ADMX

There is no Group Policy template, so this is not manageable from `gpedit`
alongside other products yet. The file is the deployable mechanism today, and
the registry/ADMX route is tracked separately — writing an ADMX I cannot test
against a real domain would be worse than not shipping one.

## Scope today

Judged: `POST /containers/create` (where `--privileged`, capabilities,
namespaces, binds and the image reference all arrive), `POST /images/create`
(pull) and `POST /images/{name}/push` — see the rule table above for which
rule applies where. With `deny-unattributable-builds` set, the build endpoints
(`/build`, `/session`, `/grpc`) and the other calls that carry no attributable
image reference are refused too.

Also judged: `POST /volumes/create`, when `allow-bind-sources` is set — a
`local`-driver volume can name a host path through its driver options, and the
container create that follows carries only the volume's name
([#419](https://github.com/wslkit/skrog/issues/419)).

Also judged: `POST /plugins/pull` and `/plugins/{name}/upgrade`, against
`allow-registries`, because a plugin names the registry it comes from
([#420](https://github.com/wslkit/skrog/issues/420)).

Not judged: the swarm and service endpoints, which run an image from a
TaskSpec this gate does not parse and so are refused only when
`deny-unattributable-builds` is on.

Resource caps on an unset container — the one *mutating* rule in the original
proposal — are deliberately not implemented: mutating a user's request
silently deserves its own review, and every rule here refuses rather than
edits.
