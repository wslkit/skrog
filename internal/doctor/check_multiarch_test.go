package doctor

import (
	"strings"
	"testing"
)

// It must never warn or fail. Most people never cross-build, and a standing
// warning about an unused capability is what teaches people to stop reading
// doctor output — so this is pinned rather than left to judgement.
func TestCheckMultiArchNeverAlarms(t *testing.T) {
	for _, in := range []MultiArchInfo{
		{},
		{Probed: true},
		{Probed: true, Handlers: []string{"qemu-aarch64"}},
	} {
		got := checkMultiArch().Run(Facts{MultiArch: in}).Status
		if got == Warn || got == Fail {
			t.Errorf("MultiArchInfo%+v produced %v; this check is informational", in, got)
		}
	}
}

func TestCheckMultiArchSkipsWhenTheEngineIsDown(t *testing.T) {
	r := checkMultiArch().Run(Facts{MultiArch: MultiArchInfo{Probed: false}})
	if r.Status != Skip {
		t.Errorf("status = %v, want Skip when nothing was probed", r.Status)
	}
}

func TestCheckMultiArchReportsHandlers(t *testing.T) {
	r := checkMultiArch().Run(Facts{MultiArch: MultiArchInfo{
		Probed:   true,
		Handlers: []string{"qemu-aarch64", "qemu-riscv64"},
	}})
	if r.Status != OK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
	if !strings.Contains(r.Summary, "qemu-aarch64") {
		t.Errorf("summary does not name the handlers: %q", r.Summary)
	}
	// The table is machine-wide, so the report must not imply Skrog owns it.
	if !strings.Contains(strings.Join(r.Detail, " "), "another distro") {
		t.Errorf("detail does not say the handlers may be another distro's: %q", r.Detail)
	}
}

// The whole point: someone who just hit `exec format error` and ran doctor has
// to find the command that works.
func TestCheckMultiArchNamesTheWorkingCommand(t *testing.T) {
	r := checkMultiArch().Run(Facts{MultiArch: MultiArchInfo{Probed: true}})
	detail := strings.Join(r.Detail, "\n")

	if !strings.Contains(detail, "exec format error") {
		t.Error("detail does not mention the error the user actually saw")
	}
	if !strings.Contains(detail, "--driver docker-container") {
		t.Errorf("detail does not give the container-driver fix:\n%s", detail)
	}
	// And it must not read as a problem, because it is not one.
	if !strings.Contains(detail, "nothing is wrong here") {
		t.Errorf("detail does not make clear this is informational:\n%s", detail)
	}
}
