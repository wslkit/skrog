package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/doctor"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/portrelay"
	"github.com/wslkit/skrog/internal/provision"
)

// publishedPortsTimeout bounds the container listing. Short: both callers --
// `skrog doctor` and the supervisor's relay tick -- have something better to do
// than wait on a wedged engine, and neither needs an answer badly enough to
// hang for one.
const publishedPortsTimeout = 10 * time.Second

// publishedPorts lists the host-side port mappings of running containers.
//
// Over the engine API rather than by exec'ing a docker client into the distro,
// for the reason #501 made concrete: there IS no docker client in the distro,
// only the daemon. The same `rwcConn` + `http.Client` shape the busy probe and
// the provenance resolver use, so the context actually bounds the round trip
// rather than only the dial.
func publishedPorts(dialer pipeproxy.Dialer, log *slog.Logger) func(context.Context) []doctor.PublishedPort {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				rwc, err := dialer.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return rwcConn{rwc}, nil
			},
			DisableKeepAlives: true,
		},
	}
	return func(ctx context.Context) []doctor.PublishedPort {
		ctx, cancel := context.WithTimeout(ctx, publishedPortsTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://engine/v1.44/containers/json", nil)
		if err != nil {
			return nil
		}
		resp, err := client.Do(req)
		if err != nil {
			// Not an error to report: doctor runs when things are broken, and
			// an engine that will not answer is already said elsewhere.
			if log != nil {
				log.Debug("could not list containers for published ports", "error", err)
			}
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil
		}

		var containers []struct {
			Names []string `json:"Names"`
			Ports []struct {
				IP          string `json:"IP"`
				PrivatePort int    `json:"PrivatePort"`
				PublicPort  int    `json:"PublicPort"`
				Type        string `json:"Type"`
			} `json:"Ports"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
			return nil
		}

		var out []doctor.PublishedPort
		seen := map[string]bool{}
		for _, c := range containers {
			name := "?"
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			for _, p := range c.Ports {
				// PublicPort zero means the port is exposed but NOT published:
				// nothing on the host side, so nothing this is about.
				if p.PublicPort == 0 {
					continue
				}
				key := p.Type + "/" + itoa(p.PublicPort)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, doctor.PublishedPort{
					Container: name,
					HostIP:    p.IP,
					HostPort:  p.PublicPort,
					Proto:     p.Type,
				})
			}
		}
		return out
	}
}

// relayPorts is the same listing reduced to what the relay binds: TCP only,
// because a stream relay cannot carry datagrams and pretending otherwise would
// publish a port that silently drops every packet (#405 parked exactly that).
func relayPorts(list []doctor.PublishedPort) []portrelay.Port {
	var out []portrelay.Port
	for _, p := range list {
		if !strings.EqualFold(p.Proto, "tcp") {
			continue
		}
		out = append(out, portrelay.Port{Number: p.HostPort, Container: p.Container})
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

// installedDistro is the distro this machine actually has.
//
// The manifest rather than the flag default, which matters when the install
// used a custom name -- the same read `skrog proxy` and `skrog serve` do before
// building a dialer.
func installedDistro(opts provision.Options) string {
	if m, err := (&provision.Provisioner{}).ReadManifest(opts); err == nil && m.Distro != "" {
		return m.Distro
	}
	return opts.Distro
}
