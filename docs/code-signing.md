# Code signing policy

Anyone installing a binary that claims to be Skrog deserves to know who can
make one. That is what this page answers, and it stands whoever ends up issuing
the certificate.

> **Status: unsigned today. Two routes open, neither closed.**
>
> The application to the SignPath **Foundation** — the free programme for open
> source — was declined for now: it is for projects with an established user
> base, and Skrog does not have one yet. They invited a reapplication once
> visibility grows, so that route is deferred rather than refused.
>
> The other route is simply to **pay** — a SignPath subscription, or another
> certificate provider. That needs no one's approval and could happen at any
> time; it is a cost decision, taken in
> [#77](https://github.com/wslkit/skrog/issues/77).
>
> Until one of those lands, Skrog's binaries are **not Authenticode-signed**
> and SmartScreen warns on first run.
>
> What *is* in effect, from **v0.3.1**: every release artifact carries SLSA
> build provenance, and `SHA256SUMS` is signed with cosign keyless. Those
> answer *"was this built by that workflow, from that commit"* — a question an
> Authenticode signature does not answer. They are not a substitute for
> signing; they are a different, and in some ways stronger, check. See
> [verifying a release](#verifying-a-release-today).
>
> Options and their costs are in
> [#77](https://github.com/wslkit/skrog/issues/77).

## The policy below still applies

The roles, the CI-only signing rule and the threat model were written to meet
the Foundation's requirements, and they are good practice independent of who
issues a certificate. They are kept so that the day one is obtained, the
process is already settled rather than invented in a hurry.

Where the text says "the Foundation", read "whoever issues the certificate".

## Project roles

SignPath requires three roles, separated so that no single unreviewed action
produces a signed binary.

| role | who | what they can do |
|---|---|---|
| **Authors** | [@zcsizmadia](https://github.com/zcsizmadia) | commit to the repository |
| **Reviewers** | [@zcsizmadia](https://github.com/zcsizmadia) | review and merge external contributions |
| **Approvers** | [@zcsizmadia](https://github.com/zcsizmadia) | approve an individual signing request |

Skrog has one maintainer, so all three are currently the same person. Stating
that plainly is the point: the separation is a real control only when the roles
are held by different people, and claiming otherwise would misrepresent what
the signature attests to. As the project gains maintainers, the Reviewer and
Approver roles are the first to be handed to someone else, and this table is
updated in the same change.

What the arrangement does still guarantee, with one maintainer or ten: **no
signature is produced without a deliberate act**. Signing is not automatic on a
tag — every release waits for an approval that a person has to give.

Every contribution from outside the Author list is reviewed before it is
merged.

Accounts holding any of these roles use multi-factor authentication for both
GitHub and SignPath.

## What is signed

The Windows executables in a release: `skrog.exe`, `skrogw.exe` and
`skrogtray.exe`.

The engine rootfs is **not** covered by this certificate. It is a Linux tarball
and is protected differently, and more strongly: pinned by SHA-256 in the
manifest compiled into the binary, verified before import, and separately
signed with [Sigstore/cosign](security.md). `skrog config set
install.verify-signature on` enforces that signature at install time.

## How a signed build is produced

Signing happens in CI, never on a developer machine — a private key that has
touched a laptop is a key whose custody cannot be described. Concretely:

1. A tag triggers `.github/workflows/release.yml`
2. The binaries are built from that tag's source with `-trimpath`, on a GitHub-hosted runner
3. [SLSA build provenance](https://slsa.dev) is attested for every artifact
4. The signing request is submitted to SignPath and waits for **manual approval**
5. Signed artifacts are published, with `SHA256SUMS` alongside

The consequence worth stating: a signed Skrog binary is reproducible only
through that pipeline, not from a local `go build`. The source is identical;
the signature is not something a local build can produce.

That is not a change SignPath introduces. The release already signs
`SHA256SUMS` with **cosign keyless**, whose identity is a GitHub Actions OIDC
token — an identity that exists nowhere but inside a workflow run. CI has
therefore always been the only thing able to produce an official Skrog
release. SignPath adds an Authenticode signature to a pipeline that was
already the sole source of signed artifacts.

## Data handling

Skrog collects nothing. There is no telemetry, no analytics, no crash
reporting, no phone-home, and no identifier of any kind is transmitted at any
point — including by the signing process, which operates on artifacts in CI and
never on a user's machine.

The single outbound request the product makes is `skrog upgrade`'s query to
the GitHub releases API, which happens only when a user runs that command and
sends nothing but the request itself. `--offline` skips it. See
[staying current](upgrading.md) and the [security model](security.md).

SignPath receives the build artifacts and the repository metadata needed to
verify them. It receives nothing about anyone who installs or runs Skrog,
because nothing about them exists to send.

## Free versus paid, and why it is not only about money

Recorded because it is the actual trade in
[#77](https://github.com/wslkit/skrog/issues/77), and the cheaper option is not
automatically the better one.

**The Foundation's programme** issues the certificate *to the project* and
requires an OSI-approved licence with **no commercial dual-licensing, for any
component**. A future paid tier would end eligibility. The Foundation can also
pause or revoke, immediately or retroactively, over a Code of Conduct
violation. It is free, and it is somebody else's to withdraw.

**A paid subscription** costs money and needs an identity to verify, but nobody
can take it away over a licence change or a disagreement, and it does not wait
on how popular the project is.

So the free route keeps a door open that the paid route closes — and vice
versa. Worth deciding deliberately rather than by default.

## Verifying a release today

Until Authenticode signing is in effect, the checksum is the first check —
and it works on every release, including the ones that predate provenance:

```powershell
# Download the zip and SHA256SUMS from the release page, then:
(Get-FileHash .\skrog_0.3.1_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()
# compare against the matching line in SHA256SUMS
```

Releases from v0.3.1 also carry SLSA provenance, verifiable with the
[GitHub CLI](https://cli.github.com):

```powershell
gh attestation verify .\skrog_0.3.1_windows_amd64.zip --owner wslkit
```

That attestation is the stronger claim of the two: it says *this artifact was
built by this workflow from this commit*, which a signature alone does not.

v0.3.0 and earlier do not have one: provenance and cosign signing landed two
days after v0.3.0 was tagged, so the command above returns 404 against it. A
404 there means "older than the feature", not "inauthentic".

## Reporting a problem

A binary that claims to be Skrog and does not verify, or a signature you
cannot account for, is worth reporting immediately —
[open an issue](https://github.com/wslkit/skrog/issues/new) or, if it looks
like a compromise rather than a mistake, mark it as such and do not include a
working reproduction in public.
