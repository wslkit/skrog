package upgrade

import (
	"os"
	"path/filepath"
	"strings"
)

// Owner is a package manager that installed, and therefore owns, this binary.
type Owner struct {
	// Name is how the user knows it: "winget", "scoop", "Chocolatey".
	Name string

	// Command upgrades Skrog the way that manager expects.
	Command string
}

// OwnedBy reports the package manager that owns the binary at exe, if any.
//
// `skrog upgrade --apply` replaces the binary in place. Under a package manager
// that is the wrong thing to do: the manager records a version and a file list,
// and writing over them behind its back leaves it believing something untrue.
// For a winget `portable` install the consequences are concrete (#370):
//
//   - `winget list` keeps reporting the version it installed
//   - `winget upgrade` later reinstalls over our newer binary, silently
//     downgrading it
//   - `winget uninstall` removes a package whose contents no longer match its
//     manifest
//
// exe MUST be the path with symlinks already resolved. A winget install puts an
// alias symlink in ...\WinGet\Links\ and the real files in ...\WinGet\Packages\,
// and it is the Packages path that identifies the owner — testing the link would
// find nothing (#360).
//
// A plain zip unpacked by hand to %LOCALAPPDATA%\Programs\skrog is owned by
// nobody and must keep upgrading itself exactly as before. That is the common
// case, so this errs towards "not owned": an unrecognised location is always
// unmanaged.
func OwnedBy(exe string) (Owner, bool) {
	if exe == "" {
		return Owner{}, false
	}
	dir := normalize(filepath.Dir(exe))
	if dir == "" {
		return Owner{}, false
	}

	for _, c := range candidates() {
		root := normalize(c.root)
		if root == "" {
			continue
		}
		if dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return c.owner, true
		}
	}
	return Owner{}, false
}

type candidate struct {
	root  string
	owner Owner
}

// candidates lists the install roots each manager uses, read from the
// environment rather than hardcoded, because all three are relocatable.
func candidates() []candidate {
	var out []candidate

	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		out = append(out, candidate{
			root: filepath.Join(local, "Microsoft", "WinGet", "Packages"),
			owner: Owner{
				Name:    "winget",
				Command: "winget upgrade wslkit.skrog",
			},
		})
	}

	// scoop honours SCOOP for a per-user install and SCOOP_GLOBAL for a
	// machine-wide one; both default under the profile / ProgramData.
	scoopRoots := []string{os.Getenv("SCOOP"), os.Getenv("SCOOP_GLOBAL")}
	if home := os.Getenv("USERPROFILE"); home != "" {
		scoopRoots = append(scoopRoots, filepath.Join(home, "scoop"))
	}
	if pd := os.Getenv("ProgramData"); pd != "" {
		scoopRoots = append(scoopRoots, filepath.Join(pd, "scoop"))
	}
	for _, r := range scoopRoots {
		if r == "" {
			continue
		}
		out = append(out, candidate{
			root:  filepath.Join(r, "apps", "skrog"),
			owner: Owner{Name: "scoop", Command: "scoop update skrog"},
		})
	}

	choco := os.Getenv("ChocolateyInstall")
	if choco == "" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			choco = filepath.Join(pd, "chocolatey")
		}
	}
	if choco != "" {
		out = append(out, candidate{
			root:  filepath.Join(choco, "lib", "skrog"),
			owner: Owner{Name: "Chocolatey", Command: "choco upgrade skrog"},
		})
	}

	return out
}

// normalize makes two spellings of the same directory comparable: cleaned,
// lowercased, and with symlinks and 8.3 short names expanded where possible.
//
// EvalSymlinks does more than follow links -- it also expands C:\Users\RUNNER~1
// to its long form -- and a mismatch there is what made an earlier ownership
// check silently decline to act (see internal/autostart, #360). A path that
// cannot be resolved falls back to the cleaned spelling rather than being
// dropped, because during an upgrade the directory certainly exists but a
// parent may not be resolvable.
func normalize(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return strings.ToLower(filepath.Clean(p))
}
