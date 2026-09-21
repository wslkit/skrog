package doctor

// checkVirtiofs reports how the engine distro mounts Windows drives, and says
// so when it is still on 9p.
//
// This one DOES warn, unlike the multi-arch check next door, and the
// difference is worth stating because the reasoning there was "a standing
// warning about a capability they do not use is the noise that teaches people
// to stop reading doctor output".
//
// Two things make this the other case:
//
//   - It is not a capability people opt into using. Every `docker run -v
//     ${PWD}:/app` goes through this mount. It is the common path, not a
//     corner of the product.
//   - The warning is permanently silenceable by taking a beneficial action.
//     Set the key once and this goes quiet forever. A warning you can retire
//     is not the kind that trains people to skim.
//
// Measured, on this project's own hardware and written up in
// docs/vm-sizing.md: reads 4.0x, `ls -l` of 1000 files 3.1x, deletes 1.7x.
// Writes are a wash. That is the shape of `npm install`, a gradle build, or a
// large `docker build` context.
//
// The verdict comes from /proc/mounts, not from ~/.wslconfig, because the two
// disagree exactly when someone would ask. See provision.MountTransport.
func checkVirtiofs() Check {
	c := Check{Name: "virtiofs", Title: "Windows drive mounts"}
	c.Run = func(f Facts) Result {
		switch f.MountTransport {
		case "":
			// Either the engine is down, or the grep found nothing. Both mean
			// "not measured", and doctor does not boot a distro to answer
			// (#82). Guessing from the config file here would be worse than
			// silence: it is the guess that is wrong in the interesting cases.
			return result(c, Skip, "engine not running, so the live mount was not read")

		case "virtiofs":
			r := result(c, OK, "Windows drives are mounted over virtiofs")
			r.Detail = []string{
				"  the faster transport, and confirmed live rather than inferred from",
				"  ~/.wslconfig -- WSL below 2.9 ignores the key silently, and it needs a",
				"  `wsl --shutdown` to take effect.",
			}
			return r
		}

		r := result(c, Warn, "Windows drives are mounted over 9p; virtiofs is measurably faster")
		r.Detail = []string{
			"  Every bind mount of a Windows folder goes through this -- `docker run`",
			"  with -v ${PWD}, a build context, a dev container's workspace.",
			"",
			"  Measured on WSL 2.9.11 (docs/vm-sizing.md):",
			"    read 256 MB        205 MB/s -> 811 MB/s   4.0x",
			"    ls -l 1000 files       0.68 s -> 0.22 s   3.1x",
			"    delete 1000 files      1.52 s -> 0.92 s   1.7x",
			"    writes                              a wash",
			"",
			"  chmod, symlinks and inotify all keep working; that was checked, not assumed.",
		}
		r.Remedy = "needs WSL 2.9 or newer, and it is a machine-wide change:\n" +
			"        skrog config set wsl.virtiofs true\n" +
			"        skrog wsl-config apply\n" +
			"        wsl --shutdown\n" +
			"      ~/.wslconfig is shared by every WSL2 distro, so `apply` shows the diff\n" +
			"      first and the shutdown stops everything, Docker Desktop included."
		return r
	}
	return c
}
