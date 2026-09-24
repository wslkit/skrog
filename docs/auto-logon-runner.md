# Running Skrog unattended (CI runners, build agents)

Skrog's supervisor keeps the engine alive across crashes, and wakes it on the
next docker command after a `wsl --shutdown` — but it needs a **logged-on
interactive session** to do it.
This page documents how to give a headless machine one. It is documentation on
purpose: Skrog will not set up auto-logon for you, because doing so means
writing an account password into the machine's LSA secrets, and a tool that
does that silently on your behalf is a liability a security review would
rightly reject. You set it up, with eyes open, or you do not.

## Why a session is required

WSL2 will not create its utility VM from session 0 (a Windows service). This
is a WSL platform constraint, not a Skrog one — it binds Docker Desktop and
Rancher Desktop identically:

- `LocalSystem` is refused outright (`WSL_E_LOCAL_SYSTEM_NOT_SUPPORTED`).
- A dedicated service account fails inside the Host Compute Service with
  `ERROR_LOGON_TYPE_NOT_GRANTED` even holding Service + Batch + Interactive
  logon rights and local administrator.

So an unattended machine needs a real user session. In practice that means
auto-logon: the machine boots, logs a chosen user in, and Skrog's supervisor
starts with that session (via the per-user autostart entry, below).

> The session-0 findings above are marked provisional in the plan (Spike B,
> issue #3) pending a re-test on a clean VM. Until that lands, treat auto-logon
> as the supported path for unattended machines.

## The playbook

### 1. Create a dedicated local account

Use a purpose-made low-privilege local account for the runner, not a personal
or domain admin account. Auto-logon stores this account's password on the
machine; scope the blast radius accordingly.

```powershell
# Elevated PowerShell
$pw = Read-Host -AsSecureString "Password for the runner account"
New-LocalUser -Name "skrog-runner" -Password $pw -PasswordNeverExpires
Add-LocalGroupMember -Group "Users" -Member "skrog-runner"
```

Local administrator is **not** required for Skrog itself (install runs
non-admin). Add it only if your CI jobs need it.

### 2. Install Skrog as that user

Log in as `skrog-runner` once, interactively, and install. This registers the
per-user autostart entry so the supervisor starts at every logon:

```powershell
skrog install --headless
skrog autostart status   # should report enabled
```

`skrog autostart` uses the per-user `Run` key and launches the windowless
`skrogw.exe`, which starts `skrog supervise` — an interactive-session
process, which is exactly what WSL2 needs. (A scheduled task set to "run
whether logged on or not" performs a batch logon into session 0, where WSL
cannot start; that is why Skrog uses the Run key, not a task.)

`skrogw.exe` then stays resident as the supervisor's **watchdog**. If the
supervisor dies — a crash, a stray `Stop-Process`, an out-of-memory kill — the
pipe would otherwise stay gone until someone logged in and ran `skrog start`,
which on an unattended runner can mean every job failing overnight. Instead it
comes back in about a second:

```
type %LOCALAPPDATA%\Skrog\watchdog.log
2026-09-09T22:15:23-05:00 supervisor exited 0xFFFFFFFF after 20s; restarting in 1s
```

The policy refuses to make things worse: a clean exit is final, a failure that
happens in milliseconds (a held single-instance lock, a bad flag, no install)
is not treated as a crash and does not loop, restarts back off from 1s to 30s,
and after 10 restarts in an hour the watchdog stops and says so. Whatever the
supervisor wrote to stderr — including the goroutine dump of a Go fatal error,
which is the only evidence such a crash leaves — is captured in
`supervisor-stderr.log` beside it.

Set `SKROG_NO_WATCHDOG=1` in the runner's environment to go back to
launch-and-forget.

### If the supervisor keeps dying

Check `watchdog.log` for how often, then run `skrog doctor` and read the
**injected modules** check. Endpoint-security agents (EDR/DLP) load a DLL into
every process and rewrite function prologues to route through their own
trampolines; those trampolines assume a C thread stack, and Go's goroutine
stacks — small, movable, their own calling convention — do not survive a hook
that restores the wrong frame. The result is a Go runtime fatal error
(`unexpected return pc`, `found pointer to free object`, an access violation at
an image-base-shaped address) with **no Skrog frame at the top of the dump**
and nothing wrong in the program that died.

Doctor names the module because the dump cannot. The actual fix is an
**exclusion for `skrog.exe` and `skrogw.exe`** from whoever manages the
agent — a policy change, not a code change. `SKROG_NO_VSOCK=1` narrows the
window in the meantime, at the cost of the slower transport. See
[#166](https://github.com/wslkit/skrog/issues/166).

### 3. Enable auto-logon

Use Sysinternals **Autologon** (recommended: it stores the password in an LSA
secret rather than plain text in the registry):

```
autologon.exe skrog-runner <domain-or-machine> <password>
```

The registry alternative (`DefaultUserName` / `DefaultPassword` under
`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`) stores the
password **in clear text** and should be avoided.

### 4. (Optional) Lock the session after logon

If the machine sits in the open, auto-logon then immediately lock keeps the
desktop from being usable while the session — and therefore the engine — stays
alive. Add a per-user startup entry:

```
%SystemRoot%\System32\rundll32.exe user32.dll,LockWorkStation
```

Locking does not stop the supervisor; the session remains logged on.

### 5. Verify

Reboot. With no interactive login, from an SSH session or a remote build step:

```powershell
docker version          # the engine answers
skrog status --json    # "supervisor":"running","engine":"running"
skrog runner check     # one verdict: auto-logon, autostart, supervisor, engine
```

`skrog runner check` verifies every step above without elevation and names the
missing one — including whether the auto-logon account is the one Skrog was
installed for, and whether the password sits in clear text in the registry
(step 3's warning). It compares the account name but never prints it. `--json`
for fleet health scripts; `skrog doctor` includes the same check on any machine
where auto-logon is configured.

## Security notes

- Auto-logon means the machine boots into a usable (or lockable) session with a
  stored credential. Treat the machine as holding that credential: restrict
  physical and RDP access, and prefer a dedicated account with only the rights
  the runner needs.
- Combine with an idle timeout (`skrog config set idle-timeout 30m`) so an
  unattended machine reclaims the engine's RAM between jobs; the next `docker`
  command wakes it.
- Nothing here is Skrog-specific plumbing — it is standard Windows unattended
  configuration. Skrog only asks for the session it documents here; it never
  creates it for you.
