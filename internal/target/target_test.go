package target

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "proj-a", "proj_a", "p1", "a-b-c", "x2y"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"", "local", "engine", "Proj", "-a", "a-", "_a", "a.b", "a b", "a/b", `a\b`,
		"..", "con.", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 33
	} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted a bad name", bad)
		}
	}
}

// The default engine's distro is skrog-engine; a named engine called "engine"
// would import over it.
func TestNoNameCollidesWithTheDefaultDistro(t *testing.T) {
	if For("base", "x").Distro == "skrog-engine" {
		t.Fatal("test premise broken")
	}
	if ValidName("engine") == nil {
		t.Error("\"engine\" would collide with the default engine's distro")
	}
}

func TestForLayout(t *testing.T) {
	l := For(`C:\s`, "proj-a")
	want := Layout{
		Name:     "proj-a",
		StateDir: filepath.Join(`C:\s`, "engines", "proj-a"),
		Distro:   "skrog-proj-a",
		Pipe:     `\\.\pipe\skrog-proj-a`,
		Context:  "skrog-proj-a",
	}
	if l != want {
		t.Errorf("got %+v, want %+v", l, want)
	}
}

func TestNameOfAndBaseOf(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Skrog")
	l := For(base, "proj-a")

	if n, ok := NameOf(l.StateDir); !ok || n != "proj-a" {
		t.Errorf("NameOf(engine dir) = %q, %v", n, ok)
	}
	if got := BaseOf(l.StateDir); got != base {
		t.Errorf("BaseOf(engine dir) = %q, want %q", got, base)
	}
	// The default engine's dir is not a named engine.
	if n, ok := NameOf(base); ok {
		t.Errorf("NameOf(base) = %q, true", n)
	}
	if got := BaseOf(base); got != base {
		t.Errorf("BaseOf(base) = %q", got)
	}
	// An engines dir whose child is not a valid name is not one either.
	if _, ok := NameOf(filepath.Join(base, "engines", "Bad Name")); ok {
		t.Error("an invalid directory name was taken for an engine")
	}
	if _, ok := NameOf(""); ok {
		t.Error("empty state dir was taken for an engine")
	}
}

func TestListAndExists(t *testing.T) {
	base := t.TempDir()
	if got, err := List(base); err != nil || len(got) != 0 {
		t.Fatalf("List on a base with no engines = %v, %v", got, err)
	}
	for _, n := range []string{"zeta", "alpha"} {
		if err := os.MkdirAll(For(base, n).StateDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Junk the registry must ignore: a file, and a directory with a bad name.
	_ = os.WriteFile(filepath.Join(EnginesRoot(base), "stray.txt"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(EnginesRoot(base), "Not Valid"), 0o755)

	got, err := List(base)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	if !Exists(base, "alpha") || Exists(base, "nope") {
		t.Error("Exists disagrees with List")
	}
}

func TestDiskUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "distro"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "distro", "ext4.vhdx"), make([]byte, 400), 0o644)
	if got := DiskUsage(dir); got != 500 {
		t.Errorf("DiskUsage = %d, want 500", got)
	}
	if got := DiskUsage(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("DiskUsage(missing) = %d, want 0", got)
	}
}

func TestCurrentName(t *testing.T) {
	engines := []string{"proj-a"}
	for ctx, want := range map[string]string{
		"skrog":         Local,
		"skrog-proj-a":  "proj-a",
		"skrog-desk":    "", // a remote, not an engine
		"default":       "",
		"desktop-linux": "",
		"":              "",
	} {
		if got := CurrentName(ctx, engines); got != want {
			t.Errorf("CurrentName(%q) = %q, want %q", ctx, got, want)
		}
	}
}
