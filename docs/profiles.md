# Network profiles

The same laptop needs different engine config on the corporate VPN than at home
— registry mirrors, DNS, logging, the idle timeout, lifecycle hooks. A profile
bundles those into a named set you switch in one command, instead of
hand-toggling each one.

```
skrog profile create work     # save the current settings as "work"
skrog profile create home
skrog profile switch home     # apply the "home" settings
skrog profile list            # * marks the active profile
skrog profile show work
skrog profile delete work
```

`skrog status` names the active profile.

## What a profile holds

A profile is a named set of the settings `skrog config` manages — the idle
timeout, engine `daemon.json` keys (registry mirrors, insecure registries, DNS,
logging, concurrency), and lifecycle hooks. It does **not** hold install-time
identity (distro, data dir, engine version) or autostart: those are properties
of the machine, not the network.

Profiles use the same YAML shape as [`skrog.yaml`](declarative-install.md), so
`skrog profile show` and `skrog config export` read alike.

## Switching is exact, not additive

`switch` makes the live config **match** the profile: any engine key, hook, or
idle timeout the profile does not set is cleared. That is what makes "switch to
home" actually undo the work-network mirrors, rather than leaving them layered
underneath. Engine changes are applied in one batch, so the engine bounces once
per switch (and not at all if the config already matches).

## Auto-switching

Switching on a network/VPN change — detect the adapter, apply the mapped profile
automatically, with consent configured up front and a log line every time — is
not implemented. [#63](https://github.com/wslkit/skrog/issues/63) is closed --
it shipped the VPN fingerprinting this would build on, not the switching -- so
do not read that link as "it exists now". `skrog profile switch` is manual: a
one-liner for a login script or a shortcut.
