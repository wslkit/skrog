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
| **Machine** | `%ProgramData%\skrog\policy.yaml` | administrators |
| **You** | `policy.yaml` in the state dir (`skrog policy show` prints the path) | you |

They merge with one rule: **the user layer may only tighten.** You can forbid
more than the machine does. You cannot permit anything it forbids.

```
$ skrog policy show
machine rules: C:\ProgramData\skrog\policy.yaml  (administrator-writable; you cannot loosen these)
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

**It IS a boundary against a standard user**, which is the actual
configuration of a managed corporate laptop. A standard user cannot write to
`%ProgramData%\skrog`, cannot loosen what is there, and cannot make `skrog
policy show` lie about it. On a machine with no machine-wide file — a personal
laptop — the original caveat stands in full: the rules are yours, you can edit
them, and this catches mistakes rather than adversaries.

**Not a rule language.** No Rego, no expressions. The vocabulary is small and
fixed so a reader can tell at a glance what is forbidden — which is most of
the value of writing a policy down.

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
has no host path to restrict and is never denied by this rule.

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

That is a deliberate choice, not an oversight. Skrog's
[wslc backend](wslc-backend.md) *does* refuse builds while the administrator's
`WSLContainerRegistryAllowlist` is active — but that policy is deployed by GPO
against a user who cannot edit it, where failing closed is the only coherent
answer. `policy.yaml` is your own file: refusing every build on a machine that
merely lists its registries would break working setups to close a hole its
author can walk around by editing one line. Making it opt-in is
[#376](https://github.com/wslkit/skrog/issues/376).

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
New-Item -ItemType Directory -Force "$env:ProgramData\skrog" | Out-Null
Set-Content "$env:ProgramData\skrog\policy.yaml" @'
deny-privileged: true
deny-host-namespaces: true
allow-registries:
  - registry.example.com
'@
```

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

Only `POST /containers/create` is judged, which is where `--privileged`,
capabilities, namespaces, binds and the image reference all arrive. Resource
caps on an unset container — the one *mutating* rule in the original
proposal — are deliberately not implemented yet: mutating a user's request
silently deserves its own review, and every rule here refuses rather than
edits.
