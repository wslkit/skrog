package doctor

import (
	"fmt"

	"github.com/wslkit/skrog/internal/version"
)

// checkEngine reports whether an engine is installed and flags WSL version skew
// since install — the "WSL was X at install and is Y now" class that produces
// networking and mount oddities after a `wsl --update`.
func checkEngine() Check {
	c := Check{Name: "engine", Title: "engine install"}
	c.Run = func(f Facts) Result {
		e := f.Report.Engine
		if !e.Installed {
			r := result(c, Warn, "no engine installed")
			r.Remedy = "run `skrog install` to provision the engine distro."
			return r
		}

		detail := []string{
			fmt.Sprintf("  version %s", orUnknown(e.Version)),
			fmt.Sprintf("  distro  %s", e.Distro),
		}
		if e.Rootfs != "" {
			detail = append(detail, "  rootfs  "+version.RootfsLine(e.Ref, e.Rootfs))
		}

		if e.WSLAtInstall != "" && f.Report.WSL != "" && e.WSLAtInstall != f.Report.WSL {
			r := result(c, Warn, fmt.Sprintf(
				"WSL was %s at install and is %s now", e.WSLAtInstall, f.Report.WSL))
			r.Detail = detail
			r.Remedy = "if the engine misbehaves (networking, mounts), `skrog restart`; " +
				"a reinstall picks up WSL's newer defaults if problems persist."
			return r
		}

		r := result(c, OK, fmt.Sprintf("engine %s installed (distro %s)",
			orUnknown(e.Version), e.Distro))
		r.Detail = detail
		return r
	}
	return c
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}
