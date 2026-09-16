package upgrade

import (
	"path/filepath"
	"testing"
)

// setRoots points every manager's environment variable at a scratch tree, so
// these tests describe layouts rather than this developer's machine.
func setRoots(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(base, "local"))
	t.Setenv("USERPROFILE", filepath.Join(base, "home"))
	t.Setenv("ProgramData", filepath.Join(base, "pd"))
	t.Setenv("SCOOP", "")
	t.Setenv("SCOOP_GLOBAL", "")
	t.Setenv("ChocolateyInstall", "")
	return base
}

func TestWingetInstallIsOwned(t *testing.T) {
	base := setRoots(t)
	exe := filepath.Join(base, "local", "Microsoft", "WinGet", "Packages",
		"wslkit.skrog__DefaultSource", "skrog.exe")

	owner, owned := OwnedBy(exe)
	if !owned {
		t.Fatalf("a winget package install must be recognised as owned: %s", exe)
	}
	if owner.Name != "winget" {
		t.Errorf("owner = %q, want winget", owner.Name)
	}
	if owner.Command != "winget upgrade wslkit.skrog" {
		t.Errorf("command = %q", owner.Command)
	}
}

func TestScoopAndChocolateyAreOwned(t *testing.T) {
	base := setRoots(t)

	for _, tc := range []struct {
		name string
		exe  string
		want string
	}{
		{"scoop per-user", filepath.Join(base, "home", "scoop", "apps", "skrog", "current", "skrog.exe"), "scoop"},
		{"scoop global", filepath.Join(base, "pd", "scoop", "apps", "skrog", "current", "skrog.exe"), "scoop"},
		{"chocolatey", filepath.Join(base, "pd", "chocolatey", "lib", "skrog", "tools", "skrog.exe"), "Chocolatey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, owned := OwnedBy(tc.exe)
			if !owned {
				t.Fatalf("%s should be owned", tc.exe)
			}
			if owner.Name != tc.want {
				t.Errorf("owner = %q, want %q", owner.Name, tc.want)
			}
		})
	}
}

// TestAHandUnpackedZipIsNotOwned is the case that must keep working. It is the
// common install -- the one-liner puts it exactly here -- and breaking
// self-upgrade for it to protect package-manager installs would be a poor
// trade.
func TestAHandUnpackedZipIsNotOwned(t *testing.T) {
	base := setRoots(t)

	for _, exe := range []string{
		filepath.Join(base, "local", "Programs", "skrog", "skrog.exe"),
		filepath.Join(base, "home", "Downloads", "skrog", "skrog.exe"),
		`C:\tools\skrog\skrog.exe`,
		filepath.Join(base, "home", "scoop", "apps", "somethingelse", "current", "skrog.exe"),
	} {
		if owner, owned := OwnedBy(exe); owned {
			t.Errorf("%s was reported as owned by %s; an unrecognised location is unmanaged", exe, owner.Name)
		}
	}
}

// TestWingetLinksDirectoryIsNotThePackage records why the caller must resolve
// symlinks first (#360).
//
// The alias on PATH lives in ...\WinGet\Links\, which is NOT inside Packages\,
// so testing the link would find no owner and the guard would never fire. The
// caller passes selfexe.Path(); this pins the reason.
func TestWingetLinksDirectoryIsNotThePackage(t *testing.T) {
	base := setRoots(t)
	link := filepath.Join(base, "local", "Microsoft", "WinGet", "Links", "skrog.exe")

	if _, owned := OwnedBy(link); owned {
		t.Error("the Links alias must not itself look owned — " +
			"if it did, the resolved-path requirement would be untested and could rot")
	}
}

func TestEmptyPathIsNotOwned(t *testing.T) {
	setRoots(t)
	if _, owned := OwnedBy(""); owned {
		t.Error("an empty path must not be reported as owned")
	}
}

// TestOwnershipIgnoresPathSpelling: the resolved path can differ in case from
// the environment variable it is compared against, and on CI it can arrive in
// 8.3 short form. A mismatch there is what made an earlier ownership check
// silently decline to act (#360).
func TestOwnershipIgnoresPathSpelling(t *testing.T) {
	base := setRoots(t)
	exe := filepath.Join(base, "LOCAL", "MICROSOFT", "WinGet", "PACKAGES",
		"wslkit.skrog__DefaultSource", "SKROG.EXE")

	if _, owned := OwnedBy(exe); !owned {
		t.Errorf("ownership must not depend on path casing: %s", exe)
	}
}

// A SCOOP override must win over the default under the profile, since that is
// the whole point of setting it.
func TestScoopEnvironmentOverride(t *testing.T) {
	base := setRoots(t)
	t.Setenv("SCOOP", filepath.Join(base, "elsewhere"))

	exe := filepath.Join(base, "elsewhere", "apps", "skrog", "current", "skrog.exe")
	owner, owned := OwnedBy(exe)
	if !owned {
		t.Fatalf("a relocated scoop install should be recognised: %s", exe)
	}
	if owner.Name != "scoop" {
		t.Errorf("owner = %q, want scoop", owner.Name)
	}
}
