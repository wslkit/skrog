package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Report is the machine-readable shape emitted by --json.
type Report struct {
	App     string   `json:"app"`
	Worst   string   `json:"worst"`
	Results []Result `json:"results"`
}

// glyph is the leading marker for a status in text output. ASCII on purpose: it
// renders identically in every Windows console and pastes cleanly into issues.
func glyph(s Status) string {
	switch s {
	case OK:
		return "[ ok ]"
	case Skip:
		return "[skip]"
	case Warn:
		return "[warn]"
	case Fail:
		return "[FAIL]"
	default:
		return "[????]"
	}
}

// WriteText renders results for a terminal: one block per check, remedies
// indented under the checks that need them, and a one-line summary at the end.
func WriteText(w io.Writer, app string, results []Result) error {
	if _, err := fmt.Fprintf(w, "skrog doctor (skrog %s)\n\n", app); err != nil {
		return err
	}
	var nWarn, nFail int
	for _, r := range results {
		switch r.Status {
		case Warn:
			nWarn++
		case Fail:
			nFail++
		}
		fmt.Fprintf(w, "%s %s: %s\n", glyph(r.Status), r.Title, r.Summary)
		for _, d := range r.Detail {
			fmt.Fprintf(w, "       %s\n", d)
		}
		if r.Fixed != "" {
			fmt.Fprintf(w, "       fixed: %s\n", r.Fixed)
		}
		if r.Remedy != "" && r.Fixed == "" {
			fmt.Fprintf(w, "       fix: %s\n", wrapIndent(r.Remedy, "            "))
		}
	}

	fmt.Fprintln(w)
	switch {
	case nFail > 0:
		fmt.Fprintf(w, "%d problem(s), %d warning(s). Address the failures above.\n", nFail, nWarn)
	case nWarn > 0:
		fmt.Fprintf(w, "no failures, %d warning(s).\n", nWarn)
	default:
		fmt.Fprintln(w, "all checks passed.")
	}
	return nil
}

// WriteJSON emits the report as indented JSON for scripts and CI.
func WriteJSON(w io.Writer, app string, results []Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(Report{App: app, Worst: Worst(results).String(), Results: results})
}

// WriteMarkdownReport renders a report ready to paste into a GitHub issue: a
// status table plus the remedies, so a bug report carries its own diagnosis.
func WriteMarkdownReport(w io.Writer, app string, results []Result) error {
	fmt.Fprintf(w, "## skrog doctor report\n\n")
	fmt.Fprintf(w, "skrog version: `%s`\n\n", app)
	fmt.Fprintf(w, "| check | status | summary |\n|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(w, "| %s | %s | %s |\n", r.Title, r.Status, mdCell(r.Summary))
	}

	var haveRemedies bool
	for _, r := range results {
		if r.Remedy != "" && r.Fixed == "" {
			haveRemedies = true
			break
		}
	}
	if haveRemedies {
		fmt.Fprintf(w, "\n### remedies\n\n")
		for _, r := range results {
			if r.Remedy == "" || r.Fixed != "" {
				continue
			}
			fmt.Fprintf(w, "- **%s**: %s\n", r.Title, r.Remedy)
		}
	}
	return nil
}

// ApplyFixes runs the Fix for each check whose result is not OK or Skip and that
// has one, re-running that check afterward so the returned results reflect the
// post-fix state. Checks without a Fix are left with their Remedy for the user.
func ApplyFixes(ctx context.Context, reg []Check, f Facts, results []Result) []Result {
	byName := map[string]Check{}
	for _, c := range reg {
		byName[c.Name] = c
	}
	out := make([]Result, len(results))
	copy(out, results)
	for i, r := range out {
		if r.Status == OK || r.Status == Skip {
			continue
		}
		c := byName[r.Name]
		if c.Fix == nil {
			continue
		}
		done, err := c.Fix(ctx, f)
		if err != nil {
			out[i].Detail = append(out[i].Detail, "fix failed: "+err.Error())
			continue
		}
		if done == "" {
			continue // nothing to do
		}
		// Re-run to reflect the new state; carry the fix note forward.
		nr := c.Run(f)
		nr.Fixed = done
		out[i] = nr
	}
	return out
}

func mdCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// wrapIndent leaves the first line as-is and prefixes continuation lines with
// indent, so a long remedy stays readable in a terminal without a wrapping lib.
//
// Line breaks the author wrote are PRESERVED. This used to run strings.Fields
// over the whole string, which splits on newlines too, so a remedy written as
// a sequence of commands came out as one paragraph:
//
//	fix: needs WSL 2.9 or newer: skrog config set wsl.virtiofs true skrog
//	     wsl-config apply wsl --shutdown ~/.wslconfig is shared by every...
//
// That is worse than ugly. It looks copy-pasteable and is not, and the same
// flattening ran the numbered steps of the injected-modules remedy together.
// A remedy is the one part of a check output a user is meant to act on.
func wrapIndent(s, indent string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		w := wrapLine(line, indent)
		if i > 0 {
			// Continuation lines carry the block indent; the first does not,
			// because the caller has already printed a "fix: " prefix.
			w = indent + w
		}
		out = append(out, w)
	}
	return strings.Join(out, "\n")
}

// wrapLine wraps one logical line, keeping its own leading whitespace on any
// continuation it produces.
//
// The leading whitespace matters: remedies indent their command blocks, and a
// wrapped command that lost its indent would sit flush against the prose and
// read as if it were part of the sentence.
func wrapLine(line, indent string) string {
	const width = 76
	lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	words := strings.Fields(line)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lead)
	lineLen := len(lead)
	for i, word := range words {
		if i > 0 {
			if lineLen+1+len(word) > width {
				b.WriteString("\n" + indent + lead)
				lineLen = len(lead)
			} else {
				b.WriteString(" ")
				lineLen++
			}
		}
		b.WriteString(word)
		lineLen += len(word)
	}
	return b.String()
}
