# Security policy

## Reporting a vulnerability

**Use [GitHub's private vulnerability reporting](https://github.com/wslkit/skrog/security/advisories/new).**
It is the only private channel here, it is free, and it gives us a place to
work on a fix and coordinate a disclosure without anything being public until
you and the maintainer agree it should be.

If that page will not open for you, open a normal issue saying only *"I have a
security report, how should I send it?"* — no details — and you will get a
channel back. Do not put the details in a public issue.

`docs/security.md` previously said "contact the maintainer privately" and gave
no way to do it, while private reporting was switched off in the repository
settings. That was a closed door with a sign on it. This file, and the setting
behind it, are the fix.

### What to include

Whatever you have. These help most:

- the version (`skrog version`) and Windows build
- what an attacker gets, and what they need to start with — a standard user on
  the same machine is a very different report from a remote one
- a reproduction, even a rough one

`skrog doctor --report` produces a Markdown summary of the machine's state
that is useful to paste in. **Read it before you send it** — it names your
distro, paths and versions.

### What to expect

One maintainer, so: an acknowledgement within a week, and an honest estimate
rather than an SLA. If a fix will take time, you will be told that instead of
being left waiting. Credit in the advisory and the release notes unless you
would rather not be named.

## Scope

### In scope

- Local privilege escalation, or one user reaching another's engine
- Anything that lets an unprivileged process on the machine reach the docker
  API through Skrog when it should not — the named pipe or the vsock transport
- Supply-chain issues: the version manifest, rootfs verification, the upgrade
  path, the release workflows
- A bypass of something the docs claim is enforced. **A false claim in the
  documentation is in scope on its own** — if `docs/security.md` says
  something is prevented and it is not, that is a bug worth reporting even
  when the underlying behaviour is intentional.

### Known and documented, not vulnerabilities

These are design decisions, written down in
[docs/security.md](docs/security.md) and
[docs/policy.md](docs/policy.md). A report that rediscovers one is welcome as
an issue, but it is not a vulnerability report:

- **A local administrator can do anything.** They can edit any config, replace
  the binaries, or uninstall Skrog.
- **The admission-control policy is not an access-control boundary**, including
  its machine-wide layer. It is tamper-evident fleet configuration enforced at
  the pipe; a determined local user can reach the engine directly
  (`wsl -d <distro> -u root`, `skrog proxy --no-path-translation`,
  `skrog wsl-integrate`). See
  [#418](https://github.com/wslkit/skrog/issues/418).
- **`skrog wsl-integrate` shares the engine socket at mode `0666`** into the
  distro you name. That is root-equivalent access for anything in it, by
  design and with a warning.
- **Binaries are not Authenticode-signed**, so SmartScreen warns
  ([#77](https://github.com/wslkit/skrog/issues/77)). Every release carries
  SLSA provenance and a cosign-signed `SHA256SUMS`; see
  [verifying a download](docs/security.md#verifying-a-download).

## Supported versions

Pre-1.0: **the latest release only.** There are no backported security fixes
to older tags. `skrog upgrade` tells you whether you are current.
