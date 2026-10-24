package wslconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wslkit/skrog/internal/wslconfig"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".wslconfig")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, path string) *wslconfig.File {
	t.Helper()
	f, err := wslconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f
}

func TestPlanIsEmptyWhenValuesAlreadyMatch(t *testing.T) {
	// The headless `--yes` path has to be idempotent: a converge run must not
	// rewrite a global config file every time it runs.
	p := write(t, "[wsl2]\nmemory=4GB\nprocessors=2\n")
	f := load(t, p)
	plan := f.Plan(map[string]string{"memory": "4GB", "processors": "2"})
	if len(plan) != 0 {
		t.Errorf("plan = %v, want empty", plan)
	}
}

func TestPlanReportsAddsAndEdits(t *testing.T) {
	p := write(t, "[wsl2]\nmemory=8GB\n")
	f := load(t, p)
	plan := f.Plan(map[string]string{"memory": "4GB", "processors": "2"})
	if len(plan) != 2 {
		t.Fatalf("plan = %v, want two changes", plan)
	}
	if plan[0].Key != "memory" || plan[0].Old != "8GB" || plan[0].New != "4GB" || plan[0].Added {
		t.Errorf("memory change = %+v", plan[0])
	}
	if plan[1].Key != "processors" || !plan[1].Added {
		t.Errorf("processors change = %+v", plan[1])
	}
	// The diff has to name the old value: consent is given to a specific edit.
	if !strings.Contains(plan[0].String(), "- memory=8GB") ||
		!strings.Contains(plan[0].String(), "+ memory=4GB") {
		t.Errorf("diff line = %q", plan[0].String())
	}
}

func TestUnsetKeysAreNotOurs(t *testing.T) {
	// A key Skrog has no opinion about must not be touched, even if the file
	// sets it: `skrog config set wsl.memory` is not a claim over swap.
	p := write(t, "[wsl2]\nswap=8GB\n")
	f := load(t, p)
	if plan := f.Plan(map[string]string{"memory": "4GB", "swap": ""}); len(plan) != 1 {
		t.Errorf("plan = %v, want only the memory add", plan)
	}
}

func TestApplyPreservesEverythingElse(t *testing.T) {
	// The file is global and belongs to the user: other sections, unknown keys,
	// comments, blank lines and ordering all have to survive.
	original := `# my machine
[wsl2]
networkingMode=mirrored   # for the VPN
dnsTunneling=true
memory=8GB

[experimental]
sparseVhd=false

[user]
default=zoltan
`
	p := write(t, original)
	f := load(t, p)
	plan := f.Plan(map[string]string{"memory": "4GB", "processors": "2"})
	if err := f.Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"# my machine",
		"networkingMode=mirrored   # for the VPN",
		"dnsTunneling=true",
		"memory=4GB",
		"processors=2",
		"[experimental]",
		"sparseVhd=false",
		"[user]",
		"default=zoltan",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("%q is missing from:\n%s", want, s)
		}
	}
	if strings.Contains(s, "memory=8GB") {
		t.Errorf("the old value survived:\n%s", s)
	}
	// processors must have landed inside [wsl2], not in a later section.
	wsl2 := s[strings.Index(s, "[wsl2]"):strings.Index(s, "[experimental]")]
	if !strings.Contains(wsl2, "processors=2") {
		t.Errorf("processors landed outside [wsl2]:\n%s", s)
	}
}

func TestApplyKeepsAnInlineComment(t *testing.T) {
	p := write(t, "[wsl2]\nmemory=8GB  # half the box\n")
	f := load(t, p)
	if err := f.Apply(f.Plan(map[string]string{"memory": "4GB"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "memory=4GB  # half the box") {
		t.Errorf("the comment was dropped:\n%s", b)
	}
}

func TestApplyCreatesTheFileAndSection(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".wslconfig")
	f := load(t, p)
	if f.Existed() {
		t.Error("Existed() true for a file that is not there")
	}
	if err := f.Apply(f.Plan(map[string]string{"memory": "4GB"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("the file was not created: %v", err)
	}
	if !strings.Contains(string(b), "[wsl2]") || !strings.Contains(string(b), "memory=4GB") {
		t.Errorf("content = %q", b)
	}
}

func TestApplyAddsSectionWithoutDisturbingAnother(t *testing.T) {
	p := write(t, "[user]\ndefault=zoltan\n")
	f := load(t, p)
	if err := f.Apply(f.Plan(map[string]string{"memory": "4GB"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "[user]") || !strings.Contains(s, "default=zoltan") {
		t.Errorf("the existing section was damaged:\n%s", s)
	}
	if !strings.Contains(s, "[wsl2]") {
		t.Errorf("[wsl2] was not created:\n%s", s)
	}
}

func TestApplyTwiceIsAnEmptySecondPlan(t *testing.T) {
	p := write(t, "[wsl2]\nmemory=8GB\n")
	desired := map[string]string{"memory": "4GB", "processors": "2", "autoMemoryReclaim": "gradual"}

	f := load(t, p)
	if err := f.Apply(f.Plan(desired)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before, _ := os.ReadFile(p)

	f2 := load(t, p)
	if plan := f2.Plan(desired); len(plan) != 0 {
		t.Fatalf("second plan = %v, want empty", plan)
	}
	if err := f2.Apply(f2.Plan(desired)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Errorf("a second apply rewrote the file:\n%s\n---\n%s", before, after)
	}
}

func TestCaseInsensitiveSectionAndKey(t *testing.T) {
	// WSL does not care about case here, so neither may we -- otherwise a
	// [WSL2] section gets a duplicate key added below it.
	p := write(t, "[WSL2]\nMemory=8GB\n")
	f := load(t, p)
	if v, ok := f.Get("memory"); !ok || v != "8GB" {
		t.Fatalf("Get(memory) = %q, %v", v, ok)
	}
	if err := f.Apply(f.Plan(map[string]string{"memory": "4GB"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "emory=") != 1 {
		t.Errorf("a duplicate key was added:\n%s", b)
	}
}

func TestCommentedOutKeysAreNotEdited(t *testing.T) {
	p := write(t, "[wsl2]\n# memory=16GB\n")
	f := load(t, p)
	plan := f.Plan(map[string]string{"memory": "4GB"})
	if len(plan) != 1 || !plan[0].Added {
		t.Fatalf("plan = %+v, want an add: a commented line is not a setting", plan)
	}
	if err := f.Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# memory=16GB") {
		t.Errorf("the comment was rewritten:\n%s", b)
	}
}

func TestValidate(t *testing.T) {
	ok := []struct{ key, in, want string }{
		{"memory", "4GB", "4GB"},
		{"memory", "4gb", "4GB"},
		{"memory", "512MB", "512MB"},
		{"swap", "0", "0"},
		{"processors", "2", "2"},
		{"autoMemoryReclaim", "Gradual", "gradual"},
		{"autoMemoryReclaim", "dropcache", "dropcache"},
		{"memory", "", ""},
	}
	for _, c := range ok {
		got, err := wslconfig.Validate(c.key, c.in)
		if err != nil {
			t.Errorf("Validate(%s, %q): %v", c.key, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Validate(%s, %q) = %q, want %q", c.key, c.in, got, c.want)
		}
	}

	bad := []struct{ key, in string }{
		{"memory", "4"},       // no unit: bytes, which nobody means
		{"memory", "lots"},    //
		{"processors", "0"},   // zero CPUs is not a configuration
		{"processors", "-1"},  //
		{"processors", "two"}, //
		{"autoMemoryReclaim", "yes"},
		{"networkingMode", "mirrored"}, // not ours to write
	}
	for _, c := range bad {
		if _, err := wslconfig.Validate(c.key, c.in); err == nil {
			t.Errorf("Validate(%s, %q) accepted an invalid value", c.key, c.in)
		}
	}
}

func TestValidateBareNumberSaysWhatToWrite(t *testing.T) {
	// "memory=4" in .wslconfig means four bytes. The error has to name the fix.
	_, err := wslconfig.Validate("memory", "4")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "4GB") {
		t.Errorf("error should suggest 4GB: %v", err)
	}
}

func TestBytes(t *testing.T) {
	cases := map[string]uint64{
		"4GB":   4 << 30,
		"512MB": 512 << 20,
		"0":     0,
		"":      0,
		"junk":  0,
	}
	for in, want := range cases {
		if got := wslconfig.Bytes(in); got != want {
			t.Errorf("Bytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAllReportsOnlyManagedKeys(t *testing.T) {
	p := write(t, "[wsl2]\nmemory=4GB\nnetworkingMode=mirrored\nswap=0\n")
	f := load(t, p)
	all := f.All()
	if len(all) != 2 || all["memory"] != "4GB" || all["swap"] != "0" {
		t.Errorf("All() = %v, want just memory and swap", all)
	}
}

// virtiofs (#327) is a boolean, and the value is written verbatim into a file
// WSL parses -- so a typo that reaches ~/.wslconfig would be ignored silently
// and the user would be left wondering why /mnt/c is still 9p.
func TestValidateVirtiofs(t *testing.T) {
	for _, in := range []string{"true", "TRUE", "True", "false", " false "} {
		got, err := wslconfig.Validate(wslconfig.KeyVirtiofs, in)
		if err != nil {
			t.Errorf("wslconfig.Validate(virtiofs, %q): %v", in, err)
			continue
		}
		if got != "true" && got != "false" {
			t.Errorf("wslconfig.Validate(virtiofs, %q) = %q, want a lowercased bool", in, got)
		}
	}
	for _, in := range []string{"yes", "1", "on", "enabled", "9p"} {
		if _, err := wslconfig.Validate(wslconfig.KeyVirtiofs, in); err == nil {
			t.Errorf("wslconfig.Validate(virtiofs, %q) accepted a value WSL does not parse as a bool", in)
		}
	}
	// Clearing stays allowed, like every other key: that is how a user goes
	// back to WSL's default rather than pinning false forever.
	if got, err := wslconfig.Validate(wslconfig.KeyVirtiofs, ""); err != nil || got != "" {
		t.Errorf("clearing virtiofs = (%q, %v), want empty and no error", got, err)
	}
}

func TestVirtiofsIsManaged(t *testing.T) {
	var found bool
	for _, k := range wslconfig.Managed() {
		if k == wslconfig.KeyVirtiofs {
			found = true
		}
	}
	if !found {
		t.Fatalf("virtiofs is not in wslconfig.Managed(), so wsl-config would never write it: %v", wslconfig.Managed())
	}
}

// The key name must match what WSL reads. It is `wsl2.virtiofs` in the
// binaries' own config table, so the file key is "virtiofs" under [wsl2] --
// getting this wrong writes a line WSL ignores without complaint.
func TestVirtiofsKeySpelling(t *testing.T) {
	if wslconfig.KeyVirtiofs != "virtiofs" {
		t.Errorf("wslconfig.KeyVirtiofs = %q; WSL reads wsl2.virtiofs", wslconfig.KeyVirtiofs)
	}
	if s := wslconfig.SectionOf(wslconfig.KeyVirtiofs); s != "wsl2" {
		t.Errorf("SectionOf(virtiofs) = %q; virtiofs lives under [wsl2]", s)
	}
}

// keySection returns the section a key's live line sits in, "" when absent,
// and how many uncommented lines set it anywhere. Reads the file as WSL does:
// the section is whatever [header] last preceded the line.
func keySection(t *testing.T, path, key string) (string, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	section, found, n := "", "", 0
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			section = strings.ToLower(strings.Trim(l, "[]"))
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			found = section
			n++
		}
	}
	return found, n
}

// #550: WSL reads autoMemoryReclaim only as experimental.autoMemoryReclaim.
// Under [wsl2] it prints "Unknown key 'wsl2.autoMemoryReclaim'" and ignores
// it -- reproduced on WSL 3.0.3 -- which is where every release before the fix
// wrote it.
func TestAutoMemoryReclaimIsWrittenUnderExperimental(t *testing.T) {
	p := write(t, "[wsl2]\nmemory=8GB\n")
	f := load(t, p)
	if err := f.Apply(f.Plan(map[string]string{"memory": "4GB", "autoMemoryReclaim": "gradual"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if s, n := keySection(t, p, "autoMemoryReclaim"); s != "experimental" || n != 1 {
		t.Errorf("autoMemoryReclaim is under [%s] (%d lines); WSL only reads it under [experimental]", s, n)
	}
	if s, _ := keySection(t, p, "memory"); s != "wsl2" {
		t.Errorf("memory moved to [%s]; it belongs under [wsl2]", s)
	}
}

func TestAutoMemoryReclaimUnderWsl2IsNotEffective(t *testing.T) {
	// `wsl-config show` and doctor report Get/All as what WSL applies. A line
	// WSL ignores must not be reported as in effect.
	f := load(t, write(t, "[wsl2]\nautoMemoryReclaim=gradual\n"))
	if v, ok := f.Get("autoMemoryReclaim"); ok {
		t.Errorf("Get(autoMemoryReclaim) = %q from [wsl2]; WSL ignores it there", v)
	}
	if _, ok := f.All()["autoMemoryReclaim"]; ok {
		t.Error("All() reports autoMemoryReclaim from [wsl2] as effective")
	}
}

func TestStaleWsl2AutoMemoryReclaimIsMoved(t *testing.T) {
	// The file an earlier Skrog left behind: same value, wrong section. The
	// plan must not be empty (that would leave the setting dead forever), and
	// the diff must say why a line with an unchanged value moves.
	p := write(t, "[wsl2]\nmemory=4GB\nautoMemoryReclaim=gradual\n")
	desired := map[string]string{"memory": "4GB", "autoMemoryReclaim": "gradual"}
	f := load(t, p)
	plan := f.Plan(desired)
	if len(plan) != 1 || plan[0].Key != "autoMemoryReclaim" || plan[0].StaleSection != "wsl2" {
		t.Fatalf("plan = %+v, want one move of autoMemoryReclaim out of [wsl2]", plan)
	}
	if s := plan[0].String(); !strings.Contains(s, "where WSL ignores it") || !strings.Contains(s, "[experimental]") {
		t.Errorf("diff does not explain the move:\n%s", s)
	}
	if err := f.Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if s, n := keySection(t, p, "autoMemoryReclaim"); s != "experimental" || n != 1 {
		t.Errorf("after apply autoMemoryReclaim is under [%s] (%d lines), want one line under [experimental]", s, n)
	}
	if again := load(t, p).Plan(desired); len(again) != 0 {
		t.Errorf("second plan = %v, want empty", again)
	}
}

func TestStaleCopyRemovedWhenTheLiveOneIsAlreadyRight(t *testing.T) {
	p := write(t, "[wsl2]\nautoMemoryReclaim=dropcache\n\n[experimental]\nautoMemoryReclaim=gradual\n")
	f := load(t, p)
	plan := f.Plan(map[string]string{"autoMemoryReclaim": "gradual"})
	if len(plan) != 1 {
		t.Fatalf("plan = %+v, want only the removal of the [wsl2] copy", plan)
	}
	if s := plan[0].String(); strings.Contains(s, "+ ") {
		t.Errorf("diff adds a line that is already there:\n%s", s)
	}
	if err := f.Apply(plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if s, n := keySection(t, p, "autoMemoryReclaim"); s != "experimental" || n != 1 {
		t.Errorf("autoMemoryReclaim under [%s], %d lines; want exactly one, under [experimental]", s, n)
	}
}

func TestStaleCopyLeftAloneWhenSkrogHasNoOpinion(t *testing.T) {
	// Unset in Skrog's config means not ours, even for a line we can tell is
	// dead: someone may have written it themselves.
	f := load(t, write(t, "[wsl2]\nautoMemoryReclaim=gradual\n"))
	if plan := f.Plan(map[string]string{"autoMemoryReclaim": ""}); len(plan) != 0 {
		t.Errorf("plan = %v, want empty", plan)
	}
}

func TestSectionOf(t *testing.T) {
	for key, want := range map[string]string{
		"memory": "wsl2", "processors": "wsl2", "swap": "wsl2", "virtiofs": "wsl2",
		"autoMemoryReclaim": "experimental",
	} {
		if got := wslconfig.SectionOf(key); got != want {
			t.Errorf("SectionOf(%q) = %q, want %q", key, got, want)
		}
	}
}
