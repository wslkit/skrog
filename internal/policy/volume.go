package policy

import (
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/apibody"
)

// Volume-create admission control (#419).
//
// docs/policy.md said, for a long time: "Named volumes are not binds --
// `-v myvol:/data` has no host path to restrict and is never denied by this
// rule." True of the common case, and false in general.
//
// A local-driver volume CAN name a host path:
//
//	docker volume create -d local -o type=none -o o=bind -o device=/mnt/c/secrets esc
//	docker run -v esc:/out ubuntu cat /out/...
//
// POST /volumes/create was judged by nothing, and the container create that
// follows carries only the volume's NAME -- which bindSources deliberately
// skips, because a name is not a path. So with allow-bind-sources: [C:\work]
// in force, those two commands read C:\secrets. The rule was not weak here; it
// was absent.
//
// device=/ is the same trick against the whole guest filesystem.

// DenyVolumeCreate judges a POST /volumes/create body.
//
// Only allow-bind-sources applies: a volume carries no image, no capabilities
// and no namespaces, so the other rules have nothing to look at.
func (r Rules) DenyVolumeCreate(body map[string]any) (reason string, denied bool) {
	if len(r.AllowBindSources) == 0 {
		return "", false
	}
	opts, ok := apibody.Map(body, "DriverOpts")
	if !ok {
		return "", false
	}
	device := strings.TrimSpace(apibody.String(opts, "device"))
	if device == "" {
		// No host path named, so nothing for this rule to restrict -- which is
		// the ordinary named volume the docs describe, living in the engine's
		// own storage.
		return "", false
	}

	// The driver is what decides whether "device" means a host path at all.
	// Empty means local, which is dockerd's own default.
	if d := strings.ToLower(strings.TrimSpace(apibody.String(body, "Driver"))); d != "" && d != "local" {
		// A third-party driver's options are its own vocabulary and this rule
		// cannot read them. Say so rather than pretend to have checked.
		return fmt.Sprintf(
			"policy does not allow volume driver %q while allow-bind-sources is in force: "+
				"its options cannot be checked against the allowed roots (allowed: %s)",
			d, strings.Join(r.AllowBindSources, ", ")), true
	}

	win, ok := guestPathToWindows(device)
	if !ok {
		// A guest path that is not under /mnt/<drive> is not under any
		// Windows root either, so it cannot be allowed by a Windows-path
		// allowlist. This is the device=/ case, and refusing is the whole
		// point of the rule.
		return fmt.Sprintf(
			"policy does not allow a volume on the guest path %s "+
				"(bind sources allowed: %s)",
			device, strings.Join(r.AllowBindSources, ", ")), true
	}
	if !underAny(win, r.AllowBindSources) {
		return fmt.Sprintf(
			"policy does not allow bind mounts from %s (allowed: %s)",
			device, strings.Join(r.AllowBindSources, ", ")), true
	}
	return "", false
}

// guestPathToWindows maps /mnt/<drive>/... back to <drive>:/..., so a device
// can be compared against an allowlist written in Windows terms.
//
// This is the inverse of winpath.ToWSL, which is what the bridge already
// applies to bind sources on their way to the engine. It is done here rather
// than by importing winpath because only the /mnt form matters: anything else
// is a guest-only path, and reporting THAT as untranslatable is a result the
// caller needs, not a failure.
//
// ok is false for a path that names no Windows drive.
func guestPathToWindows(p string) (string, bool) {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
	// Already in Windows form (c:/x). Accept it: a caller may write it that
	// way even though dockerd would not.
	if len(s) >= 2 && s[1] == ':' {
		return s, true
	}
	rest, found := strings.CutPrefix(s, "/mnt/")
	if !found || rest == "" {
		return "", false
	}
	// /mnt/c  -> c:/        /mnt/c/x -> c:/x
	drive := rest[:1]
	if drive < "a" || drive > "z" {
		return "", false
	}
	switch {
	case len(rest) == 1:
		return drive + ":/", true
	case rest[1] == '/':
		return drive + ":/" + rest[2:], true
	default:
		// /mnt/wsl/... and friends: a real guest path, not a drive.
		return "", false
	}
}

// DenyVolumeCreate on the Watcher, which is what the bridge installs as its
// gate -- Rules is not.
//
// Spelled out because this release already shipped a rule that existed only on
// Rules: deny-unattributable-builds was documented, reported active by `policy
// show`, and a no-op on every backend, because Watcher.DenyBuild returned a
// hardcoded allow and every test called Rules.DenyBuild. The compile-time
// interface guard did not catch it either, since the method existed.
//
// Carries the same unreadable-file refusal as DenyCreate (#254): a rule file
// the operator wrote and we cannot parse must not silently allow the requests
// it was written to judge.
func (w *Watcher) DenyVolumeCreate(body map[string]any) (string, bool) {
	if err := w.Unavailable(); err != nil {
		return unreadableRules(err), true
	}
	return w.Rules().DenyVolumeCreate(body)
}
