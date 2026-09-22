package provision

import (
	"context"
	"regexp"
	"strings"

	"github.com/wslkit/skrog/internal/procnet"
)

// containerIDPattern is a full engine container ID. Enforced before any ID
// reaches the shell below: the IDs come from dockerd, and dockerd is trusted,
// but a value interpolated into `sh -c` should not have to be.
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ContainerListeners reads the listening TCP sockets inside each container, by
// ID, from the container's own network namespace (#510).
//
// The container's first process is found through its cgroup,
// /sys/fs/cgroup/docker/<id>, which is where the engine's cgroupfs driver puts
// every container on this rootfs; /proc/<that pid>/net/tcp{,6} is then the
// namespace's socket table. One exec for every container, with nothing run
// inside any of them -- most images have no `ss` or `netstat` to run.
//
// A container whose cgroup or namespace cannot be read is left out of the map
// rather than reported as listening on nothing: "not measured" and "nothing is
// listening" lead to different advice.
//
// Execs in the distro, so callers must only call it when the engine is
// already up (#82).
func (p *Provisioner) ContainerListeners(ctx context.Context, opts Options, ids []string) map[string][]procnet.Listener {
	opts = opts.withDefaults()
	args := listenerArgs(ids)
	if args == nil {
		return nil
	}
	out, err := p.wsl().Exec(ctx, opts.Distro, "root", args...)
	if err != nil {
		return nil
	}
	return parseListenerSections(out)
}

// listenerArgs is the exec for ids, or nil when none of them is a container
// ID. `sh -c script sh id...`: the second "sh" is $0, so the IDs are $1...
func listenerArgs(ids []string) []string {
	var valid []string
	for _, id := range ids {
		if containerIDPattern.MatchString(id) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	return append([]string{"sh", "-c", listenersScript, "sh"}, valid...)
}

// listenersScript prints one section per container that could be read:
//
//	== <id>
//	<its /proc/net/tcp and /proc/net/tcp6>
//
// The IDs arrive as positional arguments, never spliced into the script.
const listenersScript = `for id in "$@"; do
  pid=$(head -n1 "/sys/fs/cgroup/docker/$id/cgroup.procs" 2>/dev/null)
  [ -n "$pid" ] || continue
  t=$(cat "/proc/$pid/net/tcp" "/proc/$pid/net/tcp6" 2>/dev/null) || continue
  printf '== %s\n%s\n' "$id" "$t"
done`

// parseListenerSections splits listenersScript's output by container.
func parseListenerSections(out string) map[string][]procnet.Listener {
	res := map[string][]procnet.Listener{}
	var id string
	var body strings.Builder
	flush := func() {
		if id != "" {
			res[id] = procnet.Parse(body.String())
		}
		body.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "== "); ok {
			flush()
			id = strings.TrimSpace(rest)
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return res
}
