package tray

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/doctor"
)

// The tooltip is parsed out of the real renderer's output, not a hand-written
// imitation of it. A summary that silently stops matching the report's layout
// is how this item ends up lying again.
func renderReport(t *testing.T, results []doctor.Result) string {
	t.Helper()
	var b bytes.Buffer
	if err := doctor.WriteMarkdownReport(&b, "0.6.0", results); err != nil {
		t.Fatalf("WriteMarkdownReport: %v", err)
	}
	return b.String()
}

func TestDoctorSummaryLeadsWithWhatIsWrong(t *testing.T) {
	got := summarizeDoctor(renderReport(t, []doctor.Result{
		{Title: "wsl", Status: doctor.OK, Summary: "2.9.11.0"},
		{Title: "engine", Status: doctor.Fail, Summary: "not running", Remedy: "`skrog start`"},
		{Title: "disk", Status: doctor.Warn, Summary: "4 GiB free"},
		{Title: "gpu", Status: doctor.Skip, Summary: "no NVIDIA adapter"},
		{Title: "pipe", Status: doctor.OK, Summary: "docker_engine"},
	}))

	// Someone who just clicked "Run doctor" is asking whether anything is
	// wrong, so the counts that answer that come first.
	want := "1 failed, 1 warning, 2 ok, 1 skipped"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestDoctorSummaryOnAHealthyMachine(t *testing.T) {
	got := summarizeDoctor(renderReport(t, []doctor.Result{
		{Title: "wsl", Status: doctor.OK, Summary: "2.9.11.0"},
		{Title: "engine", Status: doctor.OK, Summary: "running"},
	}))
	if want := "2 ok"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// The remedies section is prose, not table rows, and must not be counted --
// a remedy line mentioning "fail" would otherwise inflate the count.
func TestDoctorSummaryIgnoresTheRemediesSection(t *testing.T) {
	report := renderReport(t, []doctor.Result{
		{Title: "engine", Status: doctor.Fail, Summary: "not running",
			Remedy: "run `skrog start`; if that fails, see docs/troubleshooting.md"},
	})
	if !strings.Contains(report, "### remedies") {
		t.Fatal("the fixture did not produce a remedies section; the test proves nothing")
	}
	if got, want := summarizeDoctor(report), "1 failed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// Degrade to something honest rather than to a wrong count.
func TestDoctorSummaryFallsBackWhenTheLayoutIsUnrecognised(t *testing.T) {
	if got, want := summarizeDoctor("## skrog doctor report\n\nnothing here\n"), "Report saved"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
