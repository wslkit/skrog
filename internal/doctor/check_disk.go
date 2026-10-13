package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Disk thresholds: the engine's VHDX grows on demand, and image pulls and
// builds fail ugly when the volume runs out. Warn early, fail before docker
// starts erroring on its own.
const (
	diskFailBytes = 2 << 30 // 2 GiB
	diskWarnBytes = 5 << 30 // 5 GiB
)

// checkDisk reports free space on the volume that holds the engine's data. Low
// free space there surfaces as opaque pull/build failures deep inside dockerd,
// so naming it up front saves a long hunt.
func checkDisk() Check {
	c := Check{Name: "disk", Title: "disk space for engine data"}
	c.Run = func(f Facts) Result {
		d := f.Disk
		if d.Err != "" {
			r := result(c, Skip, "could not determine free disk space")
			r.Detail = []string{"  " + d.Err}
			return r
		}

		detail := []string{fmt.Sprintf("  %s free of %s on %s",
			humanBytes(d.FreeBytes), humanBytes(d.TotalBytes), d.Path)}

		// With named engines (#539) each is its own VHDX on its own growth
		// path, so the floor is judged against the fullest volume any engine
		// lives on, not just the one being examined. A drive that is fine for
		// one engine can be full with five.
		many := len(f.OtherEngines) > 0
		if many {
			var used uint64
			for _, e := range f.OtherEngines {
				used += e.UsedBytes
				detail = append(detail, fmt.Sprintf("  engine %s holds %s on %s",
					e.Name, humanBytes(e.UsedBytes), volumeOf(e.Disk.Path)))
				if e.Disk.Err == "" && e.Disk.FreeBytes < d.FreeBytes {
					d = e.Disk
				}
			}
			detail = append(detail, fmt.Sprintf("  the other local engines hold %s together (`skrog target list`)", humanBytes(used)))
		}

		// The warn floor is configurable (`disk.warn-below`, #145) so a runner
		// with a small disk is flagged before pulls start failing; fail stays fixed.
		warn := uint64(diskWarnBytes)
		if f.DiskWarnBelow > 0 {
			warn = f.DiskWarnBelow
			detail = append(detail, fmt.Sprintf("  warn floor: %s (disk.warn-below)", humanBytes(warn)))
		}

		switch {
		case d.FreeBytes < diskFailBytes:
			r := result(c, Fail, fmt.Sprintf("only %s free for engine data", humanBytes(d.FreeBytes)))
			r.Detail = detail
			r.Remedy = "free space on that volume (`skrog prune --all --build-cache` reclaims " +
				"engine disk), or move the engine data with `--data-dir` on install; pulls " +
				"and builds fail when it runs out."
			if many {
				r.Remedy += " An engine you no longer use frees its whole disk with `skrog target remove`."
			}
			return r
		case d.FreeBytes < warn:
			r := result(c, Warn, fmt.Sprintf("%s free for engine data (below the %s floor)",
				humanBytes(d.FreeBytes), humanBytes(warn)))
			r.Detail = detail
			r.Remedy = "reclaim space with `skrog prune` (add --all --build-cache for the full " +
				"sweep); large images or builds may exhaust it."
			if many {
				r.Remedy += " An engine you no longer use frees its whole disk with `skrog target remove`."
			}
			return r
		default:
			return result(c, OK, fmt.Sprintf("%s free for engine data", humanBytes(d.FreeBytes)))
		}
	}
	return c
}

// volumeOf is the drive part of a path, for naming which volume an engine is on.
func volumeOf(p string) string {
	if v := filepath.VolumeName(p); v != "" {
		return v
	}
	return strings.TrimSpace(p)
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
