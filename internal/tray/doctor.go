package tray

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DoctorReport is where the tray wrote the report, and the one-line summary it
// puts in the item's tooltip.
type DoctorReport struct {
	Path    string
	Summary string
}

// Doctor runs `skrog doctor --report` and saves the Markdown somewhere the
// browser can open it.
//
// The tray shipped this item disabled, labelled "Run doctor (v0.3)" with the
// tooltip "Diagnostics arrive in v0.3", for every release from v0.3 onward --
// `skrog doctor` landed in v0.3 on 2026-09-08 and is a README headline. The
// most visible stale scaffolding in the product was a permanently greyed-out
// menu item saying a shipped feature did not exist.
//
// --report rather than plain doctor because a tray has no window to print to.
// A Markdown file the user can read, keep and paste into an issue is the form
// that survives the click; it is also exactly what the bug template wants.
func (c CLI) Doctor(ctx context.Context) (DoctorReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Exit 1 means checks failed, which is a result and the interesting one.
	// The report is on stdout either way, so the output is judged before the
	// exit code -- the same order CheckUpgrades uses for exit 3.
	out, err := hideWindow(exec.CommandContext(ctx, c.Exe, "doctor", "--report")).Output()
	if len(out) == 0 {
		if err != nil {
			return DoctorReport{}, err
		}
		return DoctorReport{}, fmt.Errorf("`skrog doctor --report` printed nothing")
	}

	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		dir = os.TempDir()
	} else {
		dir = filepath.Join(dir, "Skrog")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return DoctorReport{}, err
	}
	// One fixed name, not a timestamp: the tray must not litter a directory
	// the user never opens with a file per click.
	path := filepath.Join(dir, "doctor-report.md")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return DoctorReport{}, err
	}
	return DoctorReport{Path: path, Summary: summarizeDoctor(string(out))}, nil
}

// summarizeDoctor counts the report's status column for the tooltip.
//
// The Markdown report has no summary line of its own -- it opens with the
// version and goes straight into `| check | status | summary |` rows, where
// status is one of ok/skip/warn/fail (internal/doctor.Status.String). Counting
// those rows is cheaper than a second `doctor --json` run and says the thing a
// tooltip has room for: whether anything needs attention.
//
// Failures and warnings lead, because "12 ok" is not what someone who just
// clicked "Run doctor" is asking about.
func summarizeDoctor(report string) string {
	n := map[string]int{}
	for _, l := range strings.Split(report, "\n") {
		f := strings.Split(l, "|")
		// A data row is "", title, status, summary, "" -- five fields. The
		// header and the |---|---|---| separator are skipped by the status
		// lookup below rather than by counting dashes.
		if len(f) != 5 {
			continue
		}
		switch s := strings.TrimSpace(f[2]); s {
		case "ok", "skip", "warn", "fail":
			n[s]++
		}
	}
	if len(n) == 0 {
		return "Report saved"
	}

	var parts []string
	if n["fail"] > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", n["fail"]))
	}
	if n["warn"] > 0 {
		parts = append(parts, fmt.Sprintf("%d warning%s", n["warn"], plural(n["warn"])))
	}
	if n["ok"] > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", n["ok"]))
	}
	if n["skip"] > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", n["skip"]))
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
