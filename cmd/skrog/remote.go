package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wslkit/skrog/internal/dockerctx"
	"github.com/wslkit/skrog/internal/remote"
	tgt "github.com/wslkit/skrog/internal/target"
)

// runRemote is `skrog remote`: the client side of `skrog serve` (#138).
func runRemote(args []string) int {
	fs := flag.NewFlagSet("remote", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		host     = fs.String("host", "", "engine address for `add`, e.g. tcp://desktop:2376")
		certs    = fs.String("certs", "", "directory with ca.pem + cert.pem/key.pem (or client.pem/client-key.pem) for `add`")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON (list, test)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog remote                                   list remotes
       skrog remote --host tcp://<h>:2376 --certs <dir> add <name>
       skrog remote use <name>|local
       skrog remote test <name>
       skrog remote remove <name>

The client side of `+"`skrog serve --tcp`"+`. `+"`add`"+` registers a remote engine's address
and its mutual-TLS material (the ca.pem, client cert and key the server's
`+"`skrog serve cert`"+` produced) and creates a docker context named skrog-<name>.
`+"`use`"+` makes that context current, so plain `+"`docker`"+` — and anything that follows
the docker context, such as VS Code Dev Containers — talks to the remote engine;
`+"`use local`"+` switches back to the local skrog context. Certificates are copied
under the state dir and never printed.

Flags come before the verb. Exit codes: 0 ok, %d error, %d usage, %d no such remote.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	base := targetBase(*stateDir)
	rm := &remote.Manager{StateDir: base}
	dc := &dockerctx.Manager{}
	ctx := context.Background()
	rest := fs.Args()

	switch {
	case len(rest) == 0 || (rest[0] == "list" && len(rest) == 1):
		return remoteList(ctx, rm, dc, *asJSON)
	case rest[0] == "add" && len(rest) == 2:
		return remoteAdd(ctx, rm, dc, base, rest[1], *host, *certs)
	case rest[0] == "use" && len(rest) == 2:
		return useTarget(ctx, base, rest[1])
	case rest[0] == "test" && len(rest) == 2:
		return remoteTest(ctx, rm, rest[1], *asJSON)
	case rest[0] == "remove" && len(rest) == 2:
		return remoteRemove(ctx, rm, dc, rest[1])
	default:
		fs.Usage()
		return exitUsage
	}
}

func remoteAdd(ctx context.Context, rm *remote.Manager, dc *dockerctx.Manager, base, name, host, certs string) int {
	if tgt.ValidName(name) == nil && tgt.Exists(base, name) {
		fmt.Fprintf(os.Stderr, "skrog: %q is already a local engine; targets share one namespace, pick another name\n", name)
		return exitError
	}
	if host == "" || certs == "" {
		fmt.Fprintln(os.Stderr, "skrog: remote add needs --host tcp://<host>:<port> and --certs <dir>")
		return exitUsage
	}
	info, err := rm.Add(name, host, certs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	ca, cert, key := rm.CertPaths(name)
	if err := dc.CreateTLS(ctx, remote.ContextName(name), "Skrog remote engine "+name, host, ca, cert, key); err != nil {
		// Registered, but no docker CLI to hold the context: still usable via env.
		var noCLI *dockerctx.ErrNoDockerCLI
		if errors.As(err, &noCLI) {
			fmt.Fprintf(os.Stderr, "skrog: remote %q registered, but no docker CLI to create its context (%v).\n"+
				"Point docker at it with DOCKER_HOST=%s DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=%s\n",
				name, err, host, info.Dir)
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "skrog: creating docker context: %v\n", err)
		return exitError
	}

	fmt.Printf("registered remote %q -> %s (client cert valid until %s)\n\n"+
		"  skrog remote test %s     check it answers\n"+
		"  skrog remote use %s      make it docker's default (`skrog remote use local` to go back)\n",
		name, host, info.CertNotAfter.Format("2006-01-02"), name, name)
	return exitOK
}

func remoteUse(ctx context.Context, rm *remote.Manager, dc *dockerctx.Manager, name string) int {
	if name == "local" {
		if err := dc.Use(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		fmt.Println("docker now targets the local Skrog engine (context skrog)")
		return exitOK
	}
	info, err := rm.Get(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitNotFound
	}
	if err := dc.UseNamed(ctx, remote.ContextName(name)); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Printf("docker now targets remote %q at %s (context %s)\n", name, info.Host, remote.ContextName(name))
	return exitOK
}

// currentRemote maps docker's current context to the contract's `current`
// value: "local" for skrog, a remote's name for skrog-<name>, "" otherwise.
func currentRemote(ctx context.Context, dc *dockerctx.Manager) string {
	cur, err := dc.Current(ctx)
	if err != nil {
		return ""
	}
	if cur == dockerctx.Name {
		return "local"
	}
	if name, ok := strings.CutPrefix(cur, remote.ContextPrefix); ok {
		return name
	}
	return ""
}

func remoteList(ctx context.Context, rm *remote.Manager, dc *dockerctx.Manager, asJSON bool) int {
	infos, err := rm.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	cur := currentRemote(ctx, dc)

	if asJSON {
		out := remoteListJSON{Current: cur, Remotes: []remoteEntryJSON{}}
		for _, i := range infos {
			out.Remotes = append(out.Remotes, remoteEntryJSON{Info: i, Current: i.Name == cur})
		}
		return emitJSON(out)
	}

	if len(infos) == 0 {
		fmt.Println("no remotes; `skrog remote --host tcp://<host>:2376 --certs <dir> add <name>` registers one")
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, " \tNAME\tHOST\tCERT EXPIRES")
	for _, i := range infos {
		marker := " "
		if i.Name == cur {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, i.Name, i.Host, i.CertNotAfter.Format("2006-01-02"))
	}
	tw.Flush()
	if cur == "local" {
		fmt.Println("\n(docker currently targets the local engine)")
	}
	return exitOK
}

func remoteRemove(ctx context.Context, rm *remote.Manager, dc *dockerctx.Manager, name string) int {
	if !rm.Exists(name) {
		fmt.Fprintf(os.Stderr, "skrog: no such remote %q\n", name)
		return exitNotFound
	}
	// Context first (switching back to local if it is current), then the files.
	if err := dc.RemoveNamed(ctx, remote.ContextName(name), dockerctx.Name); err != nil {
		var noCLI *dockerctx.ErrNoDockerCLI
		if !errors.As(err, &noCLI) {
			fmt.Fprintf(os.Stderr, "skrog: removing docker context: %v\n", err)
			return exitError
		}
	}
	if err := rm.Remove(name); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	fmt.Printf("removed remote %q\n", name)
	return exitOK
}

// remoteTest asks the remote engine for its version through the context and
// reports the round trip. DOCKER_HOST and friends are scrubbed from the child
// environment: docker refuses --context when DOCKER_HOST is also set, and the
// point here is to test the context exactly as `remote use` would exercise it.
func remoteTest(ctx context.Context, rm *remote.Manager, name string, asJSON bool) int {
	info, err := rm.Get(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitNotFound
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "docker", "--context", remote.ContextName(name),
		"version", "--format", "{{.Server.Version}}")
	cmd.Env = dockerEnvWithoutOverrides()
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	text := strings.TrimSpace(string(out))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: remote %q at %s did not answer: %v\n%s\n", name, info.Host, err, text)
		return exitError
	}
	if asJSON {
		return emitJSON(remoteTestJSON{Name: name, ServerVersion: text, Millis: elapsed.Milliseconds()})
	}
	fmt.Printf("remote %q at %s answers: engine %s (%d ms)\n", name, info.Host, text, elapsed.Milliseconds())
	return exitOK
}

// dockerEnvWithoutOverrides is the process environment minus the variables
// that would override the docker context being tested.
func dockerEnvWithoutOverrides() []string {
	var env []string
	for _, kv := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		switch key {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
			continue
		}
		env = append(env, kv)
	}
	return env
}
