// Package target is the naming and layout of per-workspace engines (#539,
// design: docs/design/per-workspace-engine.md).
//
// A named engine is a complete, independent Skrog install living under
// <state>\engines\<name>: its own WSL distro, VHDX, supervisor, pipe, docker
// context, settings, snapshots and logs. Nothing here touches WSL or docker;
// it only decides where things are called and where they live, so every other
// layer asks one place instead of rebuilding the convention.
//
// The names matter more than they look. The docker context for engine <name>
// is skrog-<name>, the same shape `skrog remote add <name>` already uses, so
// remotes and local engines share one namespace ("targets") and one switch
// verb. Anything that would let two of them claim the same name is refused.
package target

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// Local is the reserved target name for the default shared engine, whose
	// docker context is plain `skrog`.
	Local = "local"

	// EnginesDir is the directory under the base state dir that holds one
	// subdirectory per named engine.
	EnginesDir = "engines"

	// DistroPrefix, ContextPrefix and PipePrefix are prepended to the engine
	// name to form its WSL distro, docker context and named pipe.
	DistroPrefix  = "skrog-"
	ContextPrefix = "skrog-"
	PipePrefix    = `\\.\pipe\skrog-`

	// defaultDistroSuffix is what the default engine's distro takes
	// (skrog-engine), so no named engine may use it.
	defaultDistroSuffix = "engine"
)

// nameRE bounds an engine name. Lowercase only: the name becomes a directory,
// a WSL distro, a docker context and a pipe, and Windows treats the first,
// third and fourth case-insensitively, so "Proj" and "proj" would be one
// engine wearing two names. No leading or trailing separator, for the same
// reason a directory may not end in a dot.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,30}[a-z0-9])?$`)

// ValidName reports whether name may be used for a named engine.
func ValidName(name string) error {
	switch {
	case name == Local:
		return fmt.Errorf("%q is reserved for the default shared engine", Local)
	case name == defaultDistroSuffix:
		return fmt.Errorf("%q is reserved: its distro would be the default engine's (%s%s)",
			name, DistroPrefix, defaultDistroSuffix)
	case !nameRE.MatchString(name):
		return fmt.Errorf("invalid engine name %q: lowercase letters, digits, - and _ "+
			"(1 to 32), starting and ending with a letter or digit", name)
	}
	return nil
}

// Layout is where one named engine lives and what it is called.
type Layout struct {
	Name     string `json:"name"`
	StateDir string `json:"stateDir"`
	Distro   string `json:"distro"`
	Pipe     string `json:"pipe"`
	Context  string `json:"context"`
}

// For returns the layout of engine name under the base state dir. It does not
// validate: callers that accept a name from a user call ValidName first.
func For(baseStateDir, name string) Layout {
	return Layout{
		Name:     name,
		StateDir: filepath.Join(EnginesRoot(baseStateDir), name),
		Distro:   DistroPrefix + name,
		Pipe:     PipePrefix + name,
		Context:  ContextPrefix + name,
	}
}

// EnginesRoot is the directory holding every named engine of a base state dir.
func EnginesRoot(baseStateDir string) string {
	return filepath.Join(baseStateDir, EnginesDir)
}

// NameOf recognises a state dir that belongs to a named engine
// (<base>\engines\<name>) and returns the name. The default engine's state dir
// is not one, so it returns false for it, which is how the supervisor knows to
// keep its original pipe and `skrog` context behaviour untouched.
func NameOf(stateDir string) (string, bool) {
	if stateDir == "" {
		return "", false
	}
	clean := filepath.Clean(stateDir)
	name := filepath.Base(clean)
	if !strings.EqualFold(filepath.Base(filepath.Dir(clean)), EnginesDir) {
		return "", false
	}
	if ValidName(name) != nil {
		return "", false
	}
	return name, true
}

// BaseOf returns the base state dir a state dir belongs to: the parent of
// `engines` for a named engine, the dir itself otherwise. Remotes, the rootfs
// download cache and the engine registry live in the base, so a command run
// against an engine's state dir still finds them.
func BaseOf(stateDir string) string {
	if _, ok := NameOf(stateDir); ok {
		return filepath.Dir(filepath.Dir(filepath.Clean(stateDir)))
	}
	return stateDir
}

// List returns the names of the engines that exist under a base state dir, in
// name order. A directory counts whether or not its install finished: a
// half-created engine must still be visible so it can be removed.
func List(baseStateDir string) ([]string, error) {
	entries, err := os.ReadDir(EnginesRoot(baseStateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Exists reports whether an engine of that name has a directory.
func Exists(baseStateDir, name string) bool {
	st, err := os.Stat(For(baseStateDir, name).StateDir)
	return err == nil && st.IsDir()
}

// DiskUsage is the number of bytes under dir (the engine's VHDX dominates it).
// A missing directory is zero rather than an error, because a half-created
// engine is a case worth reporting, not failing on.
func DiskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// CurrentName maps docker's current context to a target: Local for `skrog`,
// the engine's name for skrog-<engine>, and "" for anything else (a remote, or
// a context that is not Skrog's). engines is the set of local engine names.
func CurrentName(dockerContext string, engines []string) string {
	if dockerContext == "skrog" {
		return Local
	}
	name, ok := strings.CutPrefix(dockerContext, ContextPrefix)
	if !ok {
		return ""
	}
	for _, e := range engines {
		if e == name {
			return name
		}
	}
	return ""
}
