package main

import (
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/version"
)

// noInstallMessage is what to write when resolving the engine found nothing.
//
// Usually that means what it says. It can also mean the manifest belongs to
// the session backend removed in #451, and then "no install found. Run `skrog
// install` first." is both wrong and useless: there IS an install, and running
// `skrog install` on top of it would not remove the old one. Those two need
// different actions from the user, so they get different messages.
//
// Returns the whole thing to write, trailing newline included.
func noInstallMessage(p *provision.Provisioner, opts provision.Options) string {
	if m, err := p.ReadManifest(opts); err == nil && m.IsLegacySession() {
		return legacyBackendMessage
	}
	return "skrog: no install found. Run `skrog install` first.\n"
}

// legacyBackendMessage is said to the small number of machines that installed
// Skrog 0.6.x onto the session backend.
//
// Uninstall first, because the manifest is what records the old backend and a
// fresh install must not inherit it. Nothing here can migrate that install in
// place: the engine it pointed at is not the engine Skrog now serves, and the
// containers and images in it belong to a different daemon.
const legacyBackendMessage = "skrog: this install uses the WSL container session backend, which has been removed.\n" +
	"\n" +
	"  Skrog now serves one engine: Docker Engine in a WSL2 distro it owns.\n" +
	"  The session backend served a different engine with a different API\n" +
	"  surface, which is the compatibility promise Skrog exists to keep.\n" +
	"\n" +
	"  To move over:\n" +
	"\n" +
	"    skrog uninstall\n" +
	"    skrog install\n" +
	"\n" +
	"  Containers and images in the old session are NOT carried over -- they\n" +
	"  belong to the other engine. Save anything you need with `wslc` before\n" +
	"  uninstalling.\n"

// requireDistroInstall resolves the engine distro for a command that needs
// one, and explains itself when there is not one.
func requireDistroInstall(p *provision.Provisioner, opts provision.Options) (string, bool) {
	distro, msg := resolveDistroFor(p, opts)
	if msg != "" {
		fmt.Fprint(os.Stderr, msg)
		return "", false
	}
	return distro, true
}

// resolveDistroFor is the decision, kept separate from printing it so the
// wording is testable without redirecting os.Stderr. An empty msg means the
// distro is usable; otherwise msg is the whole thing to write, newline
// included.
func resolveDistroFor(p *provision.Provisioner, opts provision.Options) (distro, msg string) {
	if d, ok := resolveDistro(p, opts); ok {
		return d, ""
	}
	return "", noInstallMessage(p, opts)
}

// printCLIHintIfMissing says where the docker CLI comes from, for someone who
// just installed the engine and has nothing to drive it with.
func printCLIHintIfMissing() {
	// selfexe, not os.Executable: a winget portable install runs through a
	// symlink, and the bundled CLI sits beside the real binary (#360).
	if len(version.FindDockerBinaries(version.Env{SkrogBin: selfexe.Dir()})) > 0 {
		return
	}
	fmt.Printf(`No ` + "`docker`" + ` command found on PATH. Skrog runs the engine; the CLI is
separate, and installs the upstream tools (docker, compose, buildx):

  skrog cli install

`)
}
