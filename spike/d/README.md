# Spike D: talk to wslservice over COM instead of spawning wsl.exe

The spike gating [#356](https://github.com/wslkit/skrog/issues/356). Question:
can a **standard, non-elevated** Windows process drive WSL through
`wslservice`'s COM interface rather than by spawning `wsl.exe` for every call —
and is it actually faster enough to be worth a second `internal/wsl` backend?

## Verdict: GO, with one hard requirement and one open risk

Run on 2026-09-16, Windows 10 Pro 19045, WSL **2.9.11.0** (Store), standard
non-admin user, two distros registered (`Ubuntu` stopped, `skrog-engine`
running).

```
CoInitializeSecurity -> 0x00000000

wsl.exe -l -v: 20 calls in 1.2528744s  (mean 62.64372ms per call)
LxssUserSession (Store)    CoCreateInstance -> OK
  EnumerateDistributions -> OK in 525.5µs, 2 distro(s)
    Ubuntu                   state=Installed  version=2 flags=0x1
    skrog-engine             state=Running    version=2 flags=0x0
  20 calls in 14.8172ms  (mean 740.86µs per call)
LxssUserSessionInBox       CoCreateInstance -> 0x80004002
```

| | `wsl.exe -l -v` | `EnumerateDistributions` |
|---|---|---|
| mean per call, run 1 | 66.9 ms | 0.81 ms |
| mean per call, run 2 | 62.6 ms | 0.74 ms |
| mean per call, run 3 | 66.0 ms | 0.66 ms |

**Roughly 85–90× faster**, repeated three times with the first call discarded.
That is the difference between a status poll costing a process spawn and
costing a function call.

## Findings, in the order #356 needs them

### 1. The interface is readable, not guessed

Everything here comes from
[`src/windows/service/inc/wslservice.idl`](https://github.com/microsoft/WSL/blob/master/src/windows/service/inc/wslservice.idl)
in the open-sourced WSL:

```
CLSID_LxssUserSession       a9b7a1b9-0671-405c-95f1-e0612cb4ce7e
CLSID_LxssUserSessionInBox  4f476546-b412-4579-b64c-123df331e3d6
IID_ILxssUserSession        38541BDC-F54F-4CEB-85D0-37F0F3D2617E
```

`ILxssUserSession` has 24 methods after `IUnknown`. The ones that map onto
`internal/wsl.WSL`:

| `WSL` method | COM method | vtable slot |
|---|---|---|
| `List` | `EnumerateDistributions` | 15 |
| `Terminate` | `TerminateDistribution` | 7 |
| `Unregister` | `UnregisterDistribution` | 8 |
| `Import` | `RegisterDistribution` | 4 |
| `Export` | `ExportDistribution` | 18 |
| `Exec` / `Start` | `CreateLxProcess` | 16 |
| (`wsl --shutdown`) | `Shutdown` | 23 |
| `Status` | **no equivalent** — stays on the CLI | — |

Struct layouts confirmed against the running service:
`sizeof(LXSS_ENUMERATE_INFO) = 544`, `sizeof(LXSS_ERROR_INFO) = 40`.

### 2. No elevation needed — but impersonation is

`CoCreateInstance` succeeds as a standard user. The first call then fails:

```
EnumerateDistributions -> 0x80070542      // Win32 1346, ERROR_BAD_IMPERSONATION_LEVEL
```

The service impersonates the caller to act on that user's distros, so the
client must allow it. `CoInitializeSecurity(..., RPC_C_IMP_LEVEL_IMPERSONATE,
...)` once per process fixes it. Nothing here needs admin rights.

### 3. `runtime.LockOSThread` is mandatory, and its absence is intermittent

Without it, `CoCreateInstance` failed in **2 of 4 runs** with
`CO_E_NOTINITIALIZED (0x800401F0)` — because a COM apartment belongs to an OS
*thread*, and Go moves goroutines between threads freely. The failures clustered
right after the process-spawn benchmark loop, which is exactly when the
scheduler has reason to migrate.

Load-dependent, invisible in a quick test, and it would surface on a user's
machine as an unexplained supervisor error. With the lock: 6 of 6 runs clean.

This is the single most important thing for the implementation to get right.

### 4. The in-box CLSID is a different object

`LxssUserSessionInBox` returned `E_NOINTERFACE (0x80004002)` on this machine,
which runs Store WSL. So the two CLSIDs are not interchangeable and the client
has to pick — consistent with the runtime-flavour split. A machine on in-box WSL
is untested here and is the obvious gap in this spike.

### 5. What is NOT established

- **Only `EnumerateDistributions` was actually called.** `CreateLxProcess` is
  the hard one — handles, pipes, and a process lifecycle across the RPC
  boundary — and it is the path `Exec`/`Start` need. Assume nothing about it
  from this result.
- **One host, one WSL version.** `wslc.idl` explicitly disclaims stability
  (#323) and there is no reason to expect better here. The version gate and the
  `doctor` drift check in #356 are not optional extras; they are the price of
  using this at all.
- **In-box WSL untested**, per finding 4.

## Running it

```powershell
cd spike/d
go run .
```

Standard user. It prints struct sizes, benchmarks `wsl.exe -l -v` against
`EnumerateDistributions`, and tries both CLSIDs.
