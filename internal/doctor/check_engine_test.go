package doctor

import (
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/version"
)

func TestCheckEngine(t *testing.T) {
	c := checkEngine()

	if got := c.Run(Facts{Report: report(version.Report{})}).Status; got != Warn {
		t.Errorf("not installed: got %v, want Warn", got)
	}

	installed := Facts{Report: report(version.Report{
		WSL:    "2.7.8",
		Engine: version.EngineInfo{Installed: true, Version: "29.7.2", Distro: "skrog-engine", WSLAtInstall: "2.7.8"},
	})}
	if got := c.Run(installed).Status; got != OK {
		t.Errorf("healthy: got %v, want OK", got)
	}

	skewed := Facts{Report: report(version.Report{
		WSL:    "2.8.0",
		Engine: version.EngineInfo{Installed: true, Version: "29.7.2", Distro: "skrog-engine", WSLAtInstall: "2.7.8"},
	})}
	if got := c.Run(skewed).Status; got != Warn {
		t.Errorf("skew: got %v, want Warn", got)
	}
}

// The rendered line, not the helper: reverting the printer to the bare SHA
// left every other test passing (#484).
func TestCheckEngineDetailNamesTheRootfsRevision(t *testing.T) {
	f := Facts{Report: report(version.Report{
		WSL: "2.9.12.0",
		Engine: version.EngineInfo{
			Installed:    true,
			Version:      "29.8.1",
			Distro:       "skrog-engine",
			Rootfs:       "f1be49d99b9adab037480f66a9bedc1bca2a0d1a29e278f99adcb8357d18e4ab",
			Ref:          "29.8.1-3",
			WSLAtInstall: "2.7.13.0",
		},
	})}
	detail := strings.Join(checkEngine().Run(f).Detail, "\n")
	if !strings.Contains(detail, "29.8.1-3") {
		t.Errorf("detail does not name the revision:\n%s", detail)
	}
	// The SHA stays: it is what a bug report and --rootfs-sha256 speak.
	if !strings.Contains(detail, "f1be49d99b9a") {
		t.Errorf("detail dropped the checksum:\n%s", detail)
	}
}
