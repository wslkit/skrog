package doctor

import (
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/emulation"
)

// checkEmulation reports whether the emulation the user ASKED FOR is actually
// live (#480).
//
// `skrog config set emulation.platforms linux/arm64` reports success and takes
// effect on the next engine start -- and the registration that happens there
// can fail. When it does, the only evidence is a WARN in supervisor.log:
// `skrog restart` still prints "engine is running", and the container the user
// then runs dies with `exec format error`, which is the exact symptom the
// setting was turned on to remove.
//
// That is what this check exists for, and it is why it is separate from
// checkMultiArch next door. They read the same table and ask different
// questions:
//
//	multi-arch   what CAN this machine do? Never warns, because most people
//	             never cross-build and a standing warning about an unused
//	             capability is what teaches people to skim doctor output.
//	emulation    did what you ASKED FOR happen? Silent unless you asked, and
//	             a warning when the answer is no.
//
// The first question cannot catch a broken registration: with no handlers at
// all it reports "the default builder is native-only ... nothing is wrong
// here", which on a machine that requested emulation is wrong twice.
func checkEmulation() Check {
	c := Check{Name: "emulation", Title: "foreign-architecture containers"}
	c.Run = func(f Facts) Result {
		if strings.TrimSpace(f.EmulationPlatforms) == "" {
			return result(c, Skip, "emulation.platforms is not set, so nothing should be registered")
		}

		want, err := emulation.Parse(f.EmulationPlatforms)
		if err != nil {
			r := result(c, Warn, fmt.Sprintf("emulation.platforms is not usable: %v", err))
			r.Remedy = "set it to a foreign architecture, or clear it, then `skrog restart`. " +
				"Supported: " + strings.Join(emulation.Supported(), ", ") +
				" -- minus this machine's own, which is never emulated."
			return r
		}
		if len(want) == 0 {
			return result(c, Skip, "emulation.platforms names no foreign architecture")
		}

		if !f.MultiArch.Probed {
			return result(c, Skip, "engine not running, so the handler table was not read")
		}

		live := map[string]bool{}
		for _, h := range f.MultiArch.Handlers {
			live[h] = true
		}
		var missing, present []string
		for _, h := range want {
			if live[h.Name] {
				present = append(present, h.Arch)
			} else {
				missing = append(missing, h.Arch)
			}
		}

		if len(missing) == 0 {
			return result(c, OK, fmt.Sprintf("handlers live for %s", strings.Join(present, ", ")))
		}

		r := result(c, Warn, fmt.Sprintf(
			"emulation.platforms asks for %s, but no handler is registered for %s",
			f.EmulationPlatforms, strings.Join(missing, ", ")))
		r.Detail = []string{
			"  `docker run --platform` for those architectures will fail with",
			"  \"exec format error\", exactly as if the setting were off.",
		}
		if len(present) > 0 {
			r.Detail = append(r.Detail,
				"  Live for: "+strings.Join(present, ", "))
		}
		// The overwhelmingly likely cause, and the one a user cannot guess:
		// the emulator is IN the engine image, and images before 29.8.1-3 do
		// not carry it (#479). Before 0.7.1, `engine upgrade` did not deliver
		// it either, so an install upgraded by an older skrog is in exactly
		// this state.
		r.Remedy = "the emulator ships in the engine image, and images before 29.8.1-3 " +
			"do not carry it: check `skrog engine list`, and `skrog engine upgrade` if " +
			"it is older. Then `skrog restart` and re-run this check. If the engine is " +
			"already current, the registration itself failed -- `skrog logs --supervisor` " +
			"has the reason, and \"missing interpreter\" means the image is not what " +
			"its ref claims."
		return r
	}
	return c
}
