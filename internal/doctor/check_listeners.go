package doctor

import (
	"fmt"
	"sort"
	"strings"
)

// checkContainerListeners reports a published port that -p cannot reach
// because of what the container itself listens on (#510).
//
// Two ways, both invisible from everywhere a user looks. `docker ps` shows the
// mapping, the container is healthy, and a connection to the port is reset:
//
//   - the server listens on 127.0.0.1 INSIDE the container. That is the
//     container's own loopback, and -p forwards to the container's network
//     interface, so nothing published ever reaches it. The default for Vite,
//     Next.js's dev server, Flask, and most tooling that assumes it is running
//     on a laptop rather than in a container.
//   - nothing listens on the container port at all: the right-hand side of
//     `-p 8080:80` names a port the app does not use.
//
// Measured on skrog-engine: `python -m http.server --bind 127.0.0.1 8000`
// published with -p answered nothing, and the same with `--bind ::` answered
// 200.
//
// Only containers whose sockets were actually read are judged. One that could
// not be read is left alone rather than reported as listening on nothing -- a
// false "nothing is listening" would send someone to debug an app that works.
func checkContainerListeners() Check {
	c := Check{Name: "container-listeners", Title: "published ports have something -p can reach"}
	c.Run = func(f Facts) Result {
		if !f.EngineReachable {
			return result(c, Skip, "engine is not running, so nothing is published")
		}

		type target struct {
			container, id  string
			hostPort, port int
		}
		var targets []target
		seen := map[string]bool{}
		for _, p := range f.PublishedPorts {
			if !strings.EqualFold(p.Proto, "tcp") || p.ContainerID == "" || p.ContainerPort == 0 {
				continue
			}
			key := fmt.Sprintf("%s/%d", p.ContainerID, p.ContainerPort)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, target{p.Container, p.ContainerID, p.HostPort, p.ContainerPort})
		}
		if len(targets) == 0 {
			return result(c, Skip, "no container is publishing a TCP port")
		}

		var loopback, nothing []string
		measured := 0
		for _, t := range targets {
			ls, ok := f.ContainerListeners[t.id]
			if !ok {
				continue
			}
			measured++
			var onPort, reachable int
			for _, l := range ls {
				if l.Port != t.port {
					continue
				}
				onPort++
				if !l.Loopback() {
					reachable++
				}
			}
			switch {
			case onPort == 0:
				nothing = append(nothing, fmt.Sprintf("%s: nothing listens on container port %d (published as %d)",
					t.container, t.port, t.hostPort))
			case reachable == 0:
				loopback = append(loopback, fmt.Sprintf("%s: port %d listens on 127.0.0.1 only, inside the container (published as %d)",
					t.container, t.port, t.hostPort))
			}
		}
		if measured == 0 {
			return result(c, Skip, "could not read what the publishing containers listen on")
		}
		if len(loopback) == 0 && len(nothing) == 0 {
			return result(c, OK, fmt.Sprintf("%s listening where -p can reach", plural(measured, "container is", "containers are")))
		}

		sort.Strings(loopback)
		sort.Strings(nothing)
		problems := append(append([]string{}, loopback...), nothing...)
		summary := problems[0]
		if len(problems) > 1 {
			summary += fmt.Sprintf(" (and %d more)", len(problems)-1)
		}
		r := result(c, Warn, summary)
		// One problem is the summary already; a detail line would repeat it.
		const maxDetail = 6
		for i, p := range problems {
			if len(problems) == 1 {
				break
			}
			if i == maxDetail {
				r.Detail = append(r.Detail, fmt.Sprintf("... and %d more", len(problems)-maxDetail))
				break
			}
			r.Detail = append(r.Detail, p)
		}

		var remedy []string
		if len(loopback) > 0 {
			remedy = append(remedy, "Make the server listen on 0.0.0.0 (or ::) inside the container: "+
				"127.0.0.1 there is the container's own loopback, which -p cannot reach. For example "+
				"`vite --host 0.0.0.0`, `next dev -H 0.0.0.0`, `flask run --host=0.0.0.0`, "+
				"`python -m http.server --bind 0.0.0.0`.")
		}
		if len(nothing) > 0 {
			remedy = append(remedy, "Check that the right-hand side of -p is the port the app listens on "+
				"(`docker logs <container>` usually says), or give it a moment if it is still starting.")
		}
		r.Remedy = strings.Join(remedy, " ")
		return r
	}
	return c
}

// plural renders "1 container is" / "3 containers are".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
