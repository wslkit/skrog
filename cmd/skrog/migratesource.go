package main

import "fmt"

// Migration sources (#387).
//
// `--from-desktop` was always a context name in a trench coat: the migrator
// drives the source through the docker CLI, so "support Rancher" and "support
// Podman" are mostly the question of how to address each engine, not new
// transfer code. The save/load and volume-tar paths are untouched.
//
// Podman is addressed by PIPE rather than by context, because it does not
// register a docker context. Its API is Docker-compatible, so the docker CLI
// talks to it directly over `-H npipe:...` — which is also why nothing here
// shells out to `podman`.
type migrateSource struct {
	// Flag is the --from-<flag> that selects this source.
	Flag string
	// Label is how it is named in messages.
	Label string
	// Context is the docker context to read from; empty means use Host.
	Context string
	// Host is the engine endpoint to read from, for sources that register no
	// context.
	Host string
	// Hint is printed when planning fails: the thing most likely to be wrong,
	// phrased as something to check rather than a guess at the cause.
	Hint string
}

// migrateSources are the engines Skrog knows how to migrate from. Docker
// Desktop first because it is why most people are here.
func migrateSources() []migrateSource {
	return []migrateSource{
		{
			Flag:    "from-desktop",
			Label:   "Docker Desktop",
			Context: "desktop-linux",
			Hint:    "is Docker Desktop running, and is its \"desktop-linux\" context present? (`docker context ls`)",
		},
		{
			Flag:    "from-rancher",
			Label:   "Rancher Desktop",
			Context: "rancher-desktop",
			Hint: "is Rancher Desktop running with the dockerd (moby) container engine, and is its " +
				"\"rancher-desktop\" context present? (`docker context ls`)\n" +
				"  Rancher's containerd backend serves no Docker API, so there is nothing to read:\n" +
				"  switch it to dockerd in Preferences > Container Engine, or move the images with nerdctl.",
		},
		{
			Flag:  "from-podman",
			Label: "Podman",
			Host:  `npipe:////./pipe/podman-machine-default`,
			Hint: "is the podman machine running? (`podman machine list`)\n" +
				"  This reads Podman's Docker-compatible API over its named pipe. If your machine is not\n" +
				"  the default one, pass its pipe with --from-host npipe:////./pipe/<machine-name>.",
		},
	}
}

// resolveMigrateSource picks the source from the flags that were set.
//
// Exactly one source is required: defaulting would mean guessing which engine
// a user meant to copy from, and the cost of guessing wrong is a long transfer
// of the wrong data.
func resolveMigrateSource(selected map[string]bool, ctxOverride, hostOverride string) (migrateSource, error) {
	// An explicit context or host is its own source, and overrides the menu.
	if ctxOverride != "" || hostOverride != "" {
		return migrateSource{
			Label:   "the engine you named",
			Context: ctxOverride,
			Host:    hostOverride,
			Hint:    "is that context or host reachable? (`docker context ls`)",
		}, nil
	}

	var chosen []migrateSource
	for _, s := range migrateSources() {
		if selected[s.Flag] {
			chosen = append(chosen, s)
		}
	}
	switch len(chosen) {
	case 1:
		return chosen[0], nil
	case 0:
		return migrateSource{}, fmt.Errorf(
			"specify a source: --from-desktop, --from-rancher, --from-podman " +
				"(or --from-context <name> / --from-host <endpoint>)")
	default:
		names := make([]string, len(chosen))
		for i, s := range chosen {
			names[i] = "--" + s.Flag
		}
		return migrateSource{}, fmt.Errorf(
			"choose one source, not %d (%s): they are separate engines and a run copies from one",
			len(chosen), joinWords(names))
	}
}

func joinWords(w []string) string {
	switch len(w) {
	case 0:
		return ""
	case 1:
		return w[0]
	}
	out := ""
	for i, s := range w {
		switch {
		case i == 0:
			out = s
		case i == len(w)-1:
			out += " and " + s
		default:
			out += ", " + s
		}
	}
	return out
}
