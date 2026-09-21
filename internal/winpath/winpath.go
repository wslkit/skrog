// Package winpath translates Windows paths into the paths a container engine
// running inside WSL2 understands.
//
// Docker's engine receives bind specs verbatim: Spike A (issue #2) confirmed the
// daemon rejects "C:\src:/app" and "C:/src:/app" with `invalid mode: /app`, and
// accepts "/mnt/c/src:/app". The CLI does not rewrite them, so the pipe proxy
// must — which is why this lives in its own package with no I/O: the translation
// rules are subtle enough to deserve exhaustive tests that run on any platform.
package winpath

import (
	"fmt"
	"regexp"
	"strings"
)

// A named volume is an identifier, not a location: Docker's rule is
// [a-zA-Z0-9][a-zA-Z0-9_.-]*. Structural checks below are deliberately plain
// byte comparisons rather than regexps — separators include a backslash, and
// escaping one through a regexp literal is a bug waiting to happen.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ErrUNC reports a UNC path, which WSL2 cannot bind-mount: the guest reaches
// Windows files through /mnt/<drive>, and a network share has no drive letter
// unless the user maps one. Callers should surface this to the user rather than
// pass a path the engine will reject less legibly.
type ErrUNC struct{ Path string }

func (e *ErrUNC) Error() string {
	return fmt.Sprintf("cannot bind-mount UNC path %q: map it to a drive letter first "+
		"(WSL2 reaches Windows files through /mnt/<drive>)", e.Path)
}

func isSep(c byte) bool { return c == '/' || c == '\\' }

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// driveLen returns the length of a leading Windows drive designator
// ("C:\" or "C:/"), or 0 if the path does not start with one.
func driveLen(p string) int {
	if len(p) >= 3 && isAlpha(p[0]) && p[1] == ':' && isSep(p[2]) {
		return 3
	}
	return 0
}

// isUNC reports whether the path starts with a UNC prefix (\\ or //).
func isUNC(p string) bool {
	return len(p) >= 2 && isSep(p[0]) && isSep(p[1])
}

// EngineSocket is where dockerd listens inside the engine distro; a Windows
// named pipe bind-mounted into a Linux container can only mean this.
const EngineSocket = "/var/run/docker.sock"

// isPipe reports whether the path names a Windows named pipe (\\.\pipe\name,
// also spelled //./pipe/name by tools that speak forward slashes).
func isPipe(p string) bool {
	if !isUNC(p) || len(p) < 8 || p[2] != '.' || !isSep(p[3]) {
		return false
	}
	return strings.EqualFold(p[4:8], "pipe") && len(p) > 8 && isSep(p[8])
}

// toSlash normalizes separators without path/filepath, whose behavior is
// host-dependent — these rules must mean the same thing on every platform.
func toSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// ToWSL converts an absolute Windows path to its /mnt/<drive> equivalent.
//
//	C:\src\app  -> /mnt/c/src/app
//	D:/data     -> /mnt/d/data
//
// Paths that are already POSIX (/mnt/c/src, /app) are returned unchanged, so
// translation is idempotent and users who already speak WSL are not punished.
//
// A Windows named pipe (\\.\pipe\docker_engine, //./pipe/skrog_engine) maps to
// the engine's own socket, /var/run/docker.sock: that is what Docker Desktop
// does for its pipe, and what Testcontainers' Ryuk, docker-in-docker helpers and
// `-v //./pipe/docker_engine:/var/run/docker.sock` expect. Whatever pipe the
// caller named, the container is being created through this engine, so this
// engine's socket is the only thing the mount can mean (#164).
func ToWSL(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if isPipe(path) {
		return EngineSocket, nil
	}
	if isUNC(path) {
		return "", &ErrUNC{Path: path}
	}
	n := driveLen(path)
	if n == 0 {
		// Already POSIX, or relative — nothing to translate.
		return toSlash(path), nil
	}
	drive := strings.ToLower(path[:1])
	rest := toSlash(path[n:])
	out := "/mnt/" + drive
	if rest != "" {
		out += "/" + rest
	}
	return out, nil
}

// TranslateBind rewrites the source half of a Docker bind spec.
//
// Parsing is the delicate part: a bind spec is source:destination[:options],
// but a Windows source contains its own colon (C:\src), so a naive Split(":")
// yields three fields and the engine reports `invalid mode: /app` — exactly the
// failure Spike A hit. The drive designator is therefore consumed before the
// separator search begins.
//
//	C:\src:/app        -> /mnt/c/src:/app
//	C:\src:/app:ro     -> /mnt/c/src:/app:ro
//	myvolume:/data     -> myvolume:/data     (named volume, never translated)
//	/mnt/c/src:/app    -> /mnt/c/src:/app    (already POSIX)
//	/app               -> /app               (anonymous volume, no source)
func TranslateBind(spec string) (string, error) {
	if spec == "" {
		return "", fmt.Errorf("empty bind spec")
	}

	src, rest, ok := splitBindSource(spec)
	if !ok {
		// No separator: an anonymous volume ("/data"), nothing to rewrite.
		return spec, nil
	}

	// Translating a named volume would invent a bind mount the user never
	// asked for and silently change what the container sees.
	if volumeName.MatchString(src) {
		return spec, nil
	}

	translated, err := ToWSL(src)
	if err != nil {
		return "", err
	}
	return translated + ":" + rest, nil
}

// splitBindSource splits a bind spec into its source and the remainder,
// treating a leading Windows drive designator as part of the source rather
// than as a field separator.
func splitBindSource(spec string) (src, rest string, ok bool) {
	offset := driveLen(spec)
	if offset == 0 && isUNC(spec) {
		offset = 2
	}

	i := strings.Index(spec[offset:], ":")
	if i < 0 {
		return spec, "", false
	}
	i += offset
	return spec[:i], spec[i+1:], true
}

// TranslateBinds rewrites every spec in a slice, as found in a container-create
// request's HostConfig.Binds. It fails on the first bad spec rather than
// silently dropping one: a mount that vanishes is worse than a clear error.
func TranslateBinds(binds []string) ([]string, error) {
	if binds == nil {
		return nil, nil
	}
	out := make([]string, len(binds))
	for i, b := range binds {
		t, err := TranslateBind(b)
		if err != nil {
			return nil, fmt.Errorf("bind %q: %w", b, err)
		}
		out[i] = t
	}
	return out, nil
}

// SplitDrive splits a Windows path into its drive letter and the remainder,
// with separators normalised:
//
//	C:\src\app  -> ("c", "src/app", true)
//	C:/src      -> ("c", "src", true)
//	C:\         -> ("c", "", true)
//	/tmp        -> ("", "", false)
//
// Nothing in the product calls this: ToWSL prepends /mnt/<drive> itself. It is
// kept, exported and tested because the drive-designator spellings driveLen
// accepts are the subtle part of this package, and this is where that
// behaviour is stated plainly rather than implied by a translation.
func SplitDrive(path string) (drive, rest string, ok bool) {
	n := driveLen(path)
	if n == 0 {
		return "", "", false
	}
	return strings.ToLower(path[:1]), strings.TrimPrefix(toSlash(path[n:]), "/"), true
}
