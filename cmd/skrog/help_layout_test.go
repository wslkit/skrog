package main

import (
	"strings"
	"testing"
)

// Every command's name must be separated from its summary by a real gap.
//
// The list was padded to a fixed width of 10 and `healthcheck` is 11
// characters, so that one row had NO gap at all: the name ran straight into
// its description and the column the other rows line up in simply stopped. It
// shipped that way, because nothing rendered the list and looked at it.
//
// Checked against the widest name rather than a constant, so adding a longer
// command cannot reintroduce it.
func TestNoCommandNameCollidesWithItsSummary(t *testing.T) {
	names := make([]string, 0, len(commands()))
	for _, c := range commands() {
		names = append(names, c.name)
	}
	col := helpColumn(names)

	for _, c := range commands() {
		if len(c.name) >= col {
			t.Errorf("%q is %d characters and the column is %d: its summary would "+
				"start with no gap", c.name, len(c.name), col)
		}
		gap := col - len(c.name)
		if gap < helpGap {
			t.Errorf("%q leaves a %d-space gap, want at least %d", c.name, gap, helpGap)
		}
	}
}

// And the rendered output agrees, which is the half a width calculation alone
// does not prove.
func TestRenderedCommandListIsAligned(t *testing.T) {
	var sb strings.Builder
	rows := [][2]string{
		{"healthcheck", "the longest one"},
		{"cli", "a short one"},
	}
	helpList(&sb, rows)

	starts := map[int]bool{}
	for _, line := range strings.Split(strings.TrimRight(sb.String(), "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		name := strings.Fields(trimmed)[0]
		desc := strings.Index(line, strings.TrimSpace(line[strings.Index(line, name)+len(name):]))
		starts[desc] = true
		if !strings.Contains(line, name+"   ") {
			t.Errorf("no gap after %q in %q", name, line)
		}
	}
	if len(starts) != 1 {
		t.Errorf("descriptions start at %d different columns, want 1: %q", len(starts), sb.String())
	}
}
