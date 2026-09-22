package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/internal/audit"
	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/logging"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/provision"
	"github.com/wslkit/skrog/internal/remotecert"
)

func runServe(args []string) int {
	if len(args) > 0 && args[0] == "cert" {
		return runServeCert(args[1:])
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		addr     = fs.String("tcp", "", "address to serve on, e.g. 0.0.0.0:2376 (required)")
		distro   = fs.String("distro", "", "WSL distro (default: from the install manifest)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog serve --tcp <addr>

Exposes the engine over the network with MUTUAL TLS, so a teammate or a CI
runner can target it. Only holders of a client certificate this machine's CA
signed can connect — the engine is never open to the network at large.

First mint the certificates (once):

  skrog serve cert --host <this-machine-hostname-or-ip>

then run the server:

  skrog serve --tcp 0.0.0.0:2376

On the client, copy ca.pem + client.pem + client-key.pem and:

  $env:DOCKER_HOST = "tcp://<host>:2376"
  $env:DOCKER_TLS_VERIFY = "1"
  $env:DOCKER_CERT_PATH = "<dir with the three files>"
  docker version

The engine must be running (`+"`skrog start`"+`); pair remote serving with
idle-timeout off so a remote client never meets a stopped engine.

Exit codes: 0 ok, %d error, %d usage, %d not installed / no certs.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "skrog: --tcp <addr> is required (e.g. --tcp 0.0.0.0:2376)")
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir, Distro: *distro})
	log := cliLogger(false)

	cfg, err := serverTLSConfig(tlsDir(opts.StateDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitNotFound
	}

	p := &provision.Provisioner{Logger: log}
	targetDistro, ok := resolveDistro(p, opts)
	if !ok {
		fmt.Fprintln(os.Stderr, "skrog: no install found. Run `skrog install` first.")
		return exitNotFound
	}

	ln, err := tls.Listen("tcp", *addr, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: listening on %s: %v\n", *addr, err)
		return exitError
	}
	defer ln.Close()
	log.Info("serving the engine over mutual TLS", "addr", *addr)

	ctx, stop := interruptible()
	defer stop()

	// Guarded, like the supervisor's listener.
	//
	// This one used the bare rewriter, so every rule the machine's owner wrote
	// was unenforced for remote clients and none of their calls were audited
	// (#257). The project's "a hostile local user owns the machine anyway"
	// scope does not cover it: the whole point of `skrog serve` is that the
	// requester is NOT the machine's owner and holds only a client
	// certificate. A rule set that stops at the pipe is not a rule set the
	// operator asked for, and docs/policy.md scopes the feature by API
	// endpoint, never by listener.
	watcher := policy.NewWatcher(opts.StateDir)
	watcher.OnError = func(err error) {
		log.Error("policy file is not valid", "error", err, "path", policy.Path(opts.StateDir))
	}
	// One watcher, not one per request: it stats before it reads, so consulting
	// it per docker call is cheap only if it is the same watcher each time.
	settings := config.NewWatcher(opts.StateDir)
	auditor := &audit.Switch{
		Enabled: func() bool { return settings.Config().Audit },
		Open: func() (io.WriteCloser, error) {
			return logging.NewRotatingWriter(filepath.Join(opts.StateDir, "audit.log"), 0, 0)
		},
	}
	defer auditor.Close()

	// Provenance here too, for the #257 reason above: a rule the operator set
	// must not stop at one listener. The store is the supervisor's -- same
	// state dir -- so a remote client is judged against the record the machine
	// already kept, and a pull through this listener joins it (#343).
	dialer := engineDialer(targetDistro, "", opts.StateDir, log)
	prov := &imageProvenance{stateDir: opts.StateDir, dialer: dialer, log: log}

	// Seed here too: Seed is a no-op once the store exists, so whichever
	// listener comes up first records what was already on the machine, and a
	// machine whose supervisor has not yet run with this feature does not
	// refuse its entire existing image set (#343).
	go prov.SeedExisting(ctx)

	srv := &pipeproxy.Server{
		Logger:  log,
		Handler: pipeproxy.RewriteBindsProvenanced(auditor, watcher, prov),
		Dialer:  dialer,
	}
	if err := srv.Serve(ctx, ln); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	log.Info("remote server stopped")
	return exitOK
}

func runServeCert(args []string) int {
	fs := flag.NewFlagSet("serve cert", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	client := fs.String("client", "client", "name (CN) for the client certificate")
	var hosts multiFlag
	fs.Var(&hosts, "host", "extra hostname or IP the server cert is valid for (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog serve cert [--host <name-or-ip>] [--client <name>]

Mints the mutual-TLS material for `+"`skrog serve --tcp`"+`: a CA (reused if it
already exists, so existing clients keep working), a server certificate valid
for this machine's names, and a client certificate to hand out. Files land in
the tls/ directory under the state dir.

Pass --host for every name or IP a client will dial (localhost and the local
addresses are always included).

Exit codes: 0 ok, %d error, %d usage.

flags:
`, exitError, exitUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir})
	dir := tlsDir(opts.StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	// Reuse the CA if present: regenerating it would invalidate every client
	// certificate already handed out.
	ca, err := loadOrCreateCA(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	server, err := remotecert.GenerateServer(ca, serverHosts(hosts))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if err := writeBundle(dir, "server", server); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	cl, err := remotecert.GenerateClient(ca, *client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if err := writeBundle(dir, *client, cl); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	fmt.Printf(`Wrote TLS material to %s

Serve it:   skrog serve --tcp 0.0.0.0:2376

Give the client these three files (%s), and point docker at the engine:

  ca.pem  %s.pem  %s-key.pem

  DOCKER_HOST=tcp://<this-host>:2376  DOCKER_TLS_VERIFY=1  DOCKER_CERT_PATH=<dir>

Keep ca-key.pem and *-key.pem private; anyone with a signed client cert can reach the engine.
`, dir, *client, *client, *client)
	return exitOK
}

// tlsDir is where the remote-serving certificates live.
func tlsDir(stateDir string) string { return filepath.Join(stateDir, "tls") }

// serverHosts is the SAN list: the caller's extras plus the local names.
func serverHosts(extra []string) []string {
	hosts := append([]string{"localhost", "127.0.0.1", "::1"}, extra...)
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	return hosts
}

func loadOrCreateCA(dir string) (remotecert.Bundle, error) {
	certPEM, cErr := os.ReadFile(filepath.Join(dir, "ca.pem"))
	keyPEM, kErr := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if cErr == nil && kErr == nil {
		return remotecert.Bundle{CertPEM: certPEM, KeyPEM: keyPEM}, nil
	}
	ca, err := remotecert.GenerateCA()
	if err != nil {
		return remotecert.Bundle{}, err
	}
	return ca, writeBundle(dir, "ca", ca)
}

// writeBundle writes <name>.pem (cert, 0644) and <name>-key.pem (key, 0600).
func writeBundle(dir, name string, b remotecert.Bundle) error {
	if err := os.WriteFile(filepath.Join(dir, name+".pem"), b.CertPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+"-key.pem"), b.KeyPEM, 0o600)
}

// serverTLSConfig builds the mutual-TLS config: present the server cert, and
// require + verify a client cert signed by our CA.
func serverTLSConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	if err != nil {
		return nil, fmt.Errorf("loading server certificate (run `skrog serve cert` first): %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("loading CA (run `skrog serve cert` first): %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file has no certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
