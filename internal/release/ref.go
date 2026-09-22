package release

import (
	"path"
	"strings"
)

// RefFromURL derives an engine REF -- version plus rootfs revision, "29.8.1-3"
// -- from the rootfs asset it was installed from, falling back to the bare
// version when the URL does not carry one.
//
// A ref is the unit an upgrade moves between, because two rootfs revisions can
// carry the same dockerd version and differ in what else is in the image (#65,
// #462). The version alone cannot answer "am I current".
//
// The architecture suffix is trimmed (#388). A ref names an engine BUILD, and
// it is recorded in the install manifest and compared against later. Leaving
// the architecture in would make the same engine release report a different
// ref on an arm64 machine than on an amd64 one, and would print "-amd64" on
// every row of `skrog engine list` on a machine that has no other choice.
// Neither tells anyone anything.
//
// It lives here rather than in cmd/skrog because four callers need the same
// answer -- `engine list`, `upgrade`, `version` and `doctor` -- and a naming
// rule repeated per printer is a naming rule that drifts (#484).
func RefFromURL(version, rootfsURL string) string {
	base := path.Base(rootfsURL)
	base = strings.TrimSuffix(base, ".tar.gz")
	for _, arch := range []string{"-amd64", "-arm64"} {
		base = strings.TrimSuffix(base, arch)
	}
	if rest, ok := strings.CutPrefix(base, "skrog-rootfs-"); ok && rest != "" {
		return rest
	}
	return version
}
