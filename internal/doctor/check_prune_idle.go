package doctor

import (
	"fmt"
	"strconv"
	"time"
)

// checkPruneIdle reports a `prune.every` short enough to stop the engine ever
// idling (#496).
//
// The automatic prune drives docker back through Skrog's own pipe, so every
// sweep stamps the same "last activity" clock the idle timeout reads. With
// `prune.every` at or below `idle-timeout`, the window is reset before it can
// ever expire and the engine never idle-stops: the supervisor keeps waking it
// up to tidy it.
//
// Both settings are off by default, so nothing ships in this state -- it takes
// turning both on and setting the prune interval shorter, which is the
// opposite of how either is normally tuned. But the symptom when it happens is
// "my engine never releases its RAM", which is a thing people notice, cannot
// explain, and would have no reason to connect to a housekeeping setting.
//
// Judged from the CONFIGURATION, not from the idle-stop counters. Counters
// look like the obvious evidence and are the weaker signal: a machine that has
// simply been busy, or started five minutes ago, also shows zero idle stops.
// Two durations and a comparison is a verdict; a counter at zero is a guess.
func checkPruneIdle() Check {
	c := Check{Name: "prune-idle", Title: "idle-stop versus automatic prune"}
	c.Run = func(f Facts) Result {
		if f.IdleTimeout <= 0 {
			return result(c, Skip, "idle-timeout is off, so there is no idle stop to prevent")
		}
		if f.PruneEvery <= 0 {
			return result(c, Skip, "prune.every is off, so nothing resets the idle clock")
		}
		if f.PruneEvery > f.IdleTimeout {
			return result(c, OK, fmt.Sprintf(
				"prune.every %s is longer than idle-timeout %s, so the engine can still idle",
				short(f.PruneEvery), short(f.IdleTimeout)))
		}

		r := result(c, Warn, fmt.Sprintf(
			"prune.every %s is not longer than idle-timeout %s, so the engine will never idle-stop",
			short(f.PruneEvery), short(f.IdleTimeout)))
		r.Detail = []string{
			"The automatic prune talks to the engine through Skrog's own pipe, and that",
			"traffic restarts the idle window -- so the timeout is reset before it can",
			"expire and the RAM is never returned.",
		}
		r.Remedy = fmt.Sprintf(
			"set prune.every longer than idle-timeout: `skrog config set prune.every %s` "+
				"(or lower idle-timeout). Tracked as #496.", suggest(f.IdleTimeout))
		return r
	}
	return c
}

// short renders a duration the way the setting is written, not the way Go
// prints it: `10m`, not `10m0s`. The remedy is meant to be pasted.
//
// By division rather than by trimming the string. Trimming looks simpler and
// is wrong: `strings.TrimSuffix("1m30s", "0s")` is `"1m3"`, because the suffix
// matches the end of "30s" too.
func short(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		return d.String()
	}
}

// suggest is a prune interval comfortably clear of the idle timeout rather
// than one second past it: a value that only just clears the comparison would
// still race a sweep that runs long.
func suggest(idle time.Duration) string {
	d := idle * 4
	if d < time.Hour {
		d = time.Hour
	}
	return short(d.Round(time.Minute))
}
