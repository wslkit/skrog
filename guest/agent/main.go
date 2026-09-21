//go:build linux

// skrog-agent runs inside the engine distro and relays vsock connections
// from the Windows host to dockerd's unix socket (#40).
//
// It replaces the per-connection `wsl.exe socat` path: owning both ends of
// the transport gives explicit EOFs in both directions (shutdown(SHUT_WR) is
// propagated by the relay), which is the complete fix for the connection
// leak class of #35 — socat could not tell a Ctrl-C'd CLI from a build
// upload's half-close.
//
// Security posture: only the host partition (CID 2) is allowed to connect,
// and every connection must open with the vsockproto handshake before a
// single byte reaches dockerd. Other distros in the shared utility VM (a
// vsock port space they can reach) get a closed connection and no banner.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wslkit/skrog/internal/vsockproto"
	"golang.org/x/sys/unix"
)

// Identity is what the handshake reports; the host logs it and gates features
// on it. Bumped to /2 with mutual-auth support (#81): the host provisions and
// requires the shared secret only against a /2 agent, so an older /1 rootfs
// keeps working over the unauthenticated handshake instead of being refused as
// a downgrade.
const Identity = "skrog-agent/2"

// SecretFile is where install writes the per-install auth secret (#81),
// root-readable only. Absent on a rootfs that predates auth: the agent then
// speaks the v1 handshake, and the host's socat fallback still works.
const SecretFile = "/etc/skrog/agent-secret"

func main() {
	var (
		version    = flag.Bool("version", false, "print the agent identity and exit")
		port       = flag.Uint("port", uint(vsockproto.Port), "vsock port to listen on")
		socket     = flag.String("socket", "/var/run/docker.sock", "engine socket to relay to")
		secretFile = flag.String("secret-file", SecretFile, "per-install auth secret (empty file disables auth)")
	)
	flag.Parse()
	if *version {
		fmt.Println(Identity)
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)

	// Missing secret is not fatal: an older provisioning has none, and the
	// agent must still serve (unauthenticated, as before #81).
	secret := ""
	if b, err := os.ReadFile(*secretFile); err == nil {
		secret = strings.TrimSpace(string(b))
	}
	if secret == "" {
		log.Printf("no auth secret at %s; serving the v1 (unauthenticated) handshake", *secretFile)
	}

	if err := run(uint32(*port), *socket, secret); err != nil {
		log.Fatalf("skrog-agent: %v", err)
	}
}

func run(port uint32, socket, secret string) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		return fmt.Errorf("bind(vsock:%d): %w", port, err)
	}
	if err := unix.Listen(fd, 32); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Printf("%s listening on vsock port %d, relaying to %s", Identity, port, socket)

	for {
		cfd, peer, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			switch err {
			case unix.EINTR, unix.ECONNABORTED:
				continue
			case unix.EMFILE, unix.ENFILE, unix.ENOBUFS, unix.ENOMEM:
				// Transient resource pressure (#92): killing the agent here
				// would take the whole fast path down until the next
				// StartEngine, when the fd it could not get will likely be
				// free again. Pause briefly and keep accepting; the deadline
				// on the handshake below bounds the leaked-fd source.
				log.Printf("accept: %v; backing off", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		vm, ok := peer.(*unix.SockaddrVM)
		if !ok || vm.CID != vsockHostCID {
			log.Printf("rejected connection from non-host peer %+v", peer)
			unix.Close(cfd)
			continue
		}
		conn, err := newVsockConn(cfd)
		if err != nil {
			log.Printf("wrapping connection: %v", err)
			unix.Close(cfd)
			continue
		}
		go serve(conn, socket, secret)
	}
}

// vsockHostCID is VMADDR_CID_HOST: the Windows host partition.
const vsockHostCID = 2

// handshakeTimeout bounds the opening handshake: a peer that connects and then
// says nothing must not pin a goroutine and fd forever (#92).
const handshakeTimeout = 10 * time.Second

func serve(conn *vsockConn, socket, secret string) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	if err := vsockproto.ServerHandshake(conn, Identity, secret); err != nil {
		log.Printf("handshake refused: %v", err)
		return
	}
	// Clear the deadline before the transparent relay phase: a long-lived
	// idle stream (docker events on a quiet engine) is legitimate.
	conn.SetReadDeadline(time.Time{})
	backend, err := net.Dial("unix", socket)
	if err != nil {
		log.Printf("dialing %s: %v", socket, err)
		return
	}
	if err := vsockproto.Relay(conn, backend.(*net.UnixConn)); err != nil && !ignorable(err) {
		log.Printf("relay: %v", err)
	}
}

// ignorable filters the errors every teardown race produces.
func ignorable(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, unix.EPIPE) || errors.Is(err, unix.ECONNRESET) ||
		errors.Is(err, os.ErrClosed)
}
