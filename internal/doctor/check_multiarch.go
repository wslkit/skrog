package doctor

import (
	"fmt"
	"strings"
)

// checkMultiArch reports whether the DEFAULT buildx driver can build for
// another architecture, and — when it cannot — says what does work (#384).
//
// Deliberately never warns. Most people never cross-build, and a standing
// warning about a capability they do not use is the noise that teaches people
// to stop reading doctor output. But `exec format error` is an opaque way to
// discover this, and it reads like a broken Dockerfile or a bad base image, so
// someone who just hit it and ran `doctor` should find the answer here.
//
// The distinction that matters, and the one the original issue got wrong:
//
//	default `docker` driver   builds in dockerd's own BuildKit, which carries
//	                          no emulator -> needs a host binfmt_misc handler
//	`docker-container` driver builds in an official BuildKit image, which
//	                          BUNDLES qemu -> cross-builds with no handler
//
// So the honest report is not "broken", it is "the default builder is native
// only, and here is the one command that is not".
func checkMultiArch() Check {
	c := Check{Name: "multi-arch", Title: "cross-architecture builds"}
	c.Run = func(f Facts) Result {
		if !f.MultiArch.Probed {
			return result(c, Skip, "engine not running, so the handler table was not read")
		}

		if len(f.MultiArch.Handlers) > 0 {
			r := result(c, OK, fmt.Sprintf("the default builder can emulate: %s",
				strings.Join(f.MultiArch.Handlers, ", ")))
			r.Detail = []string{
				"  registered binfmt_misc handlers, so `docker buildx build --platform` works",
				"  on the default builder AND `docker run --platform` can start a foreign",
				"  container.",
				"  These live in the shared WSL2 utility VM, so they may have been registered",
				"  by another distro on this machine, by tonistiigi/binfmt, or by Skrog's own",
				"  `emulation.platforms` setting -- the table does not record who wrote it.",
			}
			return r
		}

		r := result(c, OK, "the default builder is native-only (no emulation registered)")
		r.Detail = []string{
			"  `docker buildx build --platform linux/arm64` on the DEFAULT builder fails",
			"  with \"exec format error\": dockerd's built-in BuildKit carries no emulator.",
			"",
			"  A container-driver builder does, and needs nothing installed:",
			"    docker buildx create --name cross --driver docker-container --bootstrap",
			"    docker buildx build --builder cross --platform linux/arm64 .",
			"",
			"  Only relevant if you build for another architecture; nothing is wrong here.",
		}
		return r
	}
	return c
}
