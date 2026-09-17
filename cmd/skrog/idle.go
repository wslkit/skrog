package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/integrate"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/regcache"
	"github.com/wslkit/skrog/internal/supervise"
)

// demandDialer wakes an idle-stopped engine before dialing it: the first
// docker command after an idle stop pays the cold start and works, instead of
// failing against a stopped engine (#41).
type demandDialer struct {
	sup   *supervise.Supervisor
	inner pipeproxy.Dialer
}

func (d *demandDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	// Demand is a no-op unless the engine is idle-stopped, so the healthy
	// path pays one flag check, not an engine probe.
	if err := d.sup.Demand(ctx); err != nil {
		return nil, fmt.Errorf("waking the engine: %w", err)
	}
	return d.inner.Dial(ctx)
}

// rwcConn adapts the dialer's io.ReadWriteCloser to net.Conn for http.
// Deadlines are no-ops; the probe's lifetime is bounded by its context.
type rwcConn struct {
	io.ReadWriteCloser
}

// probedContainer is the slice of /containers/json the busy probe reads.
type probedContainer struct {
	Names []string `json:"Names"`
	State string   `json:"State"`
}

// isInfra reports whether this container is Skrog's own plumbing rather than
// the user's work, so an idle stop (#41) or a scheduled prune (#393) may
// ignore it. Getting this wrong in either direction is bad in a quiet way:
// counting infrastructure holds the engine awake forever, and skipping a
// user's container stops the engine out from under it. So the list is
// explicit, exact-match, and short.
func (c probedContainer) isInfra() bool {
	for _, n := range c.Names {
		switch strings.TrimPrefix(n, "/") {
		case regcache.ContainerName:
			return true
		}
	}
	return false
}

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

func (rwcConn) LocalAddr() net.Addr              { return dummyAddr("skrog") }
func (rwcConn) RemoteAddr() net.Addr             { return dummyAddr("engine") }
func (rwcConn) SetDeadline(time.Time) error      { return nil }
func (rwcConn) SetReadDeadline(time.Time) error  { return nil }
func (rwcConn) SetWriteDeadline(time.Time) error { return nil }

// busyProbe asks the engine whether any containers are running, over the same
// transport the bridge uses. The idle stop needs a definite "no": any error
// here vetoes it (supervise.Supervisor.Busy's contract).
func busyProbe(dialer pipeproxy.Dialer, busyLog *slog.Logger) func(ctx context.Context) (bool, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				rwc, err := dialer.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return rwcConn{rwc}, nil
			},
			// One probe, one connection: the engine socket is not a place to
			// pool idle keep-alives that would themselves look like activity.
			DisableKeepAlives: true,
		},
	}
	return func(ctx context.Context) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		// /containers/json lists RUNNING containers by default, which is
		// exactly the "would an idle stop kill something" question. An explicit
		// API version avoids any ambiguity with a bare, unversioned path.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/v1.44/containers/json", nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("engine returned %s to the container probe", resp.Status)
		}
		var containers []probedContainer
		if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
			return false, err
		}
		// Skrog's own infrastructure is not work (#385). The pull-through
		// cache is a long-lived container, and counting it would hold the
		// engine awake forever and suppress every scheduled prune — turning
		// on a cache would silently disable two other features, with no error
		// anywhere to explain it.
		containers = slices.DeleteFunc(containers, probedContainer.isInfra)
		if len(containers) > 0 && busyLog != nil {
			names := make([]string, len(containers))
			for i, c := range containers {
				names[i] = strings.Join(c.Names, ",") + "(" + c.State + ")"
			}
			busyLog.Info("container probe sees running containers", "count", len(containers), "containers", strings.Join(names, " "))
		}
		return len(containers) > 0, nil
	}
}

// engineBusy vetoes an idle stop when the engine is in use in any way the pipe
// cannot see (#72): a running container, OR — only when a distro has been
// wired with `skrog wsl-integrate` — an active connection to the engine
// socket over the /mnt/wsl share. Either signal, or an error probing
// containers, keeps the engine up so nothing is stopped mid-operation.
//
// The socket probe is gated on there being a recorded integration for a
// reason: Skrog's own health check dials the engine socket every tick, and
// counting connections cannot cleanly tell that apart from a sibling distro's
// client. Running it unconditionally made the engine never idle. Scoped to
// installs that actually share the socket, the rare false veto only costs an
// integrated-and-idle setup its RAM reclaim (documented), while every other
// install idles normally.
func engineBusy(dialer pipeproxy.Dialer, p *provision.Provisioner, opts provision.Options, log *slog.Logger) func(ctx context.Context) (bool, error) {
	containers := busyProbe(dialer, log)
	integrations := &integrate.Manager{StateDir: opts.StateDir, Logger: log}
	return func(ctx context.Context) (bool, error) {
		if busy, err := containers(ctx); err != nil || busy {
			return busy, err
		}
		if wired, err := integrations.List(); err != nil || len(wired) == 0 {
			return false, nil // no shared socket to worry about
		}
		return p.SocketBusy(ctx, opts)
	}
}
