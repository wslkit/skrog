package doctor

import (
	"path/filepath"

	"github.com/wslkit/skrog/internal/target"
)

// otherEngineDisks lists every local engine other than the one whose state dir
// is given, with its use and the volume it lives on (#539). Empty on a machine
// that never created a named engine, which is the common case and costs one
// directory read.
func otherEngineDisks(stateDir string) []EngineDisk {
	if stateDir == "" {
		return nil
	}
	base := target.BaseOf(stateDir)
	names, _ := target.List(base)
	if len(names) == 0 {
		return nil
	}
	self, _ := target.NameOf(stateDir)

	var out []EngineDisk
	if self != "" {
		// Doctoring a named engine: the default engine is one of the others.
		dir := filepath.Join(base, "distro")
		out = append(out, EngineDisk{
			Name: "local", UsedBytes: uint64(target.DiskUsage(dir)), Disk: diskInfo(dir),
		})
	}
	for _, n := range names {
		if n == self {
			continue
		}
		l := target.For(base, n)
		out = append(out, EngineDisk{
			Name:      n,
			UsedBytes: uint64(target.DiskUsage(l.StateDir)),
			Disk:      diskInfo(filepath.Join(l.StateDir, "distro")),
		})
	}
	return out
}
