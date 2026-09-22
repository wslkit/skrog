package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wslkit/skrog/internal/policy"
	"github.com/wslkit/skrog/internal/provenance"
	"github.com/wslkit/skrog/internal/provision"
)

// exitDenied is its own code so a script can tell "the rules refused this"
// from "the command went wrong" — which `policy test` exists to be used in.
const exitDenied = 13

// runPolicy is `skrog policy`: local admission control for the docker API
// (#120).
func runPolicy(args []string) int {
	if len(args) == 0 {
		policyUsage()
		return exitUsage
	}
	switch args[0] {
	case "show":
		return runPolicyShow(args[1:])
	case "check":
		return runPolicyCheck(args[1:])
	case "test":
		return runPolicyTest(args[1:])
	case "-h", "--help", "help":
		policyUsage()
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "skrog policy: unknown subcommand %q (show|check|test)\n", args[0])
	return exitUsage
}

func policyUsage() {
	fmt.Fprintf(os.Stderr, `usage: skrog policy show|check|test

Local admission control for the docker API. Skrog already sits in the request
path, so it can refuse a container the machine's owner has ruled out — no
elevation, no engine change, no daemon plugin.

  show    print the rules in effect, and where they come from
  check   validate the rules file without applying it
  test    judge a container-create body against the rules, and say why

Rules live in %s inside the state dir. A missing file means no rules.
Edits take effect on the next container create -- nothing to restart.

  deny-privileged:            true        # refuse --privileged
  deny-added-capabilities:    true        # refuse any --cap-add
  deny-capabilities:          [SYS_ADMIN] # ...or only these
  deny-host-namespaces:       true        # refuse --network/--pid/--ipc/--uts=host
  allow-bind-sources:         [C:\work]   # bind mounts may only come from here
  allow-registries:           [registry.example.com, "*.internal"]
  require-digest:             true        # images must be pinned by digest
  deny-unattributable-builds: true        # refuse docker build while images are restricted
  deny-unattributable-images: true        # ...and images with no recorded origin

This is a guardrail, not a security boundary: whoever owns the machine can
edit the file or bypass the bridge. It is for catching mistakes and for
shared and CI machines.

Exit codes: 0 ok, %d error, %d usage, %d not installed, %d the rules deny it.
`, policy.FileName, exitError, exitUsage, exitNotFound, exitDenied)
}

// stateDirFor resolves the install's state dir for a policy subcommand.
func stateDirFor(fs *flag.FlagSet, override string) string {
	opts := optsWithResolvedStateDir(provision.Options{StateDir: override})
	return opts.StateDir
}

func runPolicyShow(args []string) int {
	fs := flag.NewFlagSet("policy show", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	dir := stateDirFor(fs, *stateDir)

	rules, src, err := policy.LoadLayered(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if *asJSON {
		emitJSON(policyShowJSON{
			Path:              policy.Path(dir),
			Active:            !rules.Empty(),
			Rules:             rules,
			Source:            src,
			Exists:            fileExists(policy.Path(dir)),
			Enforced:          !rules.Empty(),
			MachineProvenance: policy.MachineProvenance(),
		})
		return exitOK
	}

	// Each layer separately before the effective set: someone reading this
	// wants to know which file to go and argue with, and on a managed laptop
	// that is not the one in their own state dir.
	// Provenance before contents (#418). The layer cannot be made
	// unbypassable — the supervisor runs as the user — so the honest promise
	// is that this report does not lie about which bypass happened. A refused
	// layer and an absent one used to look identical here.
	prov := policy.MachineProvenance()
	if prov.Redirected && prov.Path == "" {
		// The documented bypass, and the one that produced silence: point the
		// variable at an empty directory and the machine layer simply is not
		// there. Indistinguishable from a machine that never had one, which is
		// the whole problem — so it is said out loud.
		fmt.Printf("machine rules: none found\n")
		fmt.Printf("  %s redirects the machine layer to %s, which holds no %s.\n",
			policy.MachineDirEnv, prov.RedirectedTo, policy.FileName)
		fmt.Println("  If this machine is supposed to carry fleet policy, that variable is why")
		fmt.Println("  it is not. It is read from the environment, which the user owns.")
		fmt.Println()
	}
	if prov.Path != "" && !prov.Trusted {
		fmt.Printf("machine rules: %s  REFUSED\n", prov.Path)
		fmt.Printf("  ignored because %s.\n", prov.Why)
		fmt.Println("  A machine layer is only fleet configuration if the fleet wrote it; one")
		fmt.Println("  a standard user could have written carries no more authority than their")
		fmt.Println("  own rules, so it is not merged. Deploy it to a directory owned by")
		fmt.Println("  Administrators or SYSTEM.")
		if prov.Redirected {
			fmt.Printf("  Note: %s pointed the machine layer at %s.\n",
				policy.MachineDirEnv, prov.RedirectedTo)
		}
		fmt.Println()
	}
	if src.MachinePath != "" {
		// "administrator-writable" is what this said until #418, and it is not
		// something skrog checks — the default ProgramData ACL lets a standard
		// user create the directory and own it. Claiming a property the code
		// never verifies is the wrong thing to print next to a file path.
		//
		// It is checked now, which is why the line can say something about it:
		// this branch is only reached for a file that passed.
		fmt.Printf("machine rules: %s  (deployed machine-wide; you cannot loosen these)\n", src.MachinePath)
		if prov.Redirected {
			// A trusted file in a redirected location is legitimate -- a fleet
			// may keep ProgramData elsewhere -- and still worth saying, because
			// the variable is also the cheapest way to retire the layer.
			fmt.Printf("  location set by %s: %s\n", policy.MachineDirEnv, prov.RedirectedTo)
		}
		for _, line := range describe(src.Machine) {
			fmt.Printf("  %s\n", line)
		}
	}
	if src.MachinePath != "" {
		fmt.Printf("\nyour rules: %s\n", policy.Path(dir))
	} else {
		fmt.Printf("rules: %s\n", policy.Path(dir))
	}
	if src.UserPath == "" || src.User.Empty() {
		fmt.Println("  none")
	} else {
		for _, line := range describe(src.User) {
			fmt.Printf("  %s\n", line)
		}
	}

	if rules.Empty() {
		fmt.Println("\nno rules in effect — every request is allowed")
		fmt.Println("(write the file to add some; `skrog policy --help` lists the keys)")
		return exitOK
	}
	if src.MachinePath != "" {
		fmt.Println("\nin effect (machine rules, tightened by yours):")
		for _, line := range describe(rules) {
			fmt.Printf("  %s\n", line)
		}
	}

	// Provenance coverage, reported BEFORE anything is refused for the lack of
	// it (#343). Turning deny-unattributable-images on without knowing how
	// much of a machine's image set has a record is how a security feature
	// gets switched off again an hour later.
	st, provErr := provenance.Summarize(dir)
	switch {
	case provErr != nil:
		// Said out loud rather than shown as zero. The count is exactly what
		// someone uses to decide whether it is safe to turn the rule on, and a
		// silent under-count is worse than an error.
		fmt.Printf("\nimage provenance: could not be read (%v)\n", provErr)
		if rules.DenyUnattributableImages {
			fmt.Println("  Requests are still ALLOWED while it cannot be read; the rule fails open.")
		}
	case st.Total > 0 || rules.DenyUnattributableImages:
		fmt.Printf("\nimage provenance: %d image(s) recorded", st.Total)
		if st.Total > 0 {
			fmt.Printf(" — %d pulled, %d pre-existing", st.Pulled, st.PreExisting)
		}
		fmt.Println()
		if st.PreExisting > 0 {
			fmt.Println("  pre-existing means it was already here when recording started, so its")
			fmt.Println("  origin is unknown and it is trusted anyway. That is deliberate, and it")
			fmt.Println("  is the honest reading of what this machine can prove.")
		}
		if !rules.DenyUnattributableImages {
			fmt.Println("  Nothing is refused for missing provenance: set deny-unattributable-images")
			fmt.Println("  (with allow-registries) to make it bite.")
		}
	}

	fmt.Println("\nedits take effect on the next container create")
	return exitOK
}

func runPolicyCheck(args []string) int {
	fs := flag.NewFlagSet("policy check", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "override Skrog's state directory")
	file := fs.String("file", "", "validate this file instead of the installed one")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path := *file
	if path == "" {
		path = policy.Path(stateDirFor(fs, *stateDir))
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Printf("no rules file at %s — every request is allowed\n", path)
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	rules, err := policy.Parse(b)
	if err != nil {
		// The whole point of `check`: find this here rather than discovering
		// at restart that the supervisor is enforcing nothing.
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if rules.Empty() {
		fmt.Printf("%s parses, but sets no rules — every request would be allowed\n", path)
		return exitOK
	}
	fmt.Printf("%s is valid:\n", path)
	for _, line := range describe(rules) {
		fmt.Printf("  %s\n", line)
	}
	return exitOK
}

func runPolicyTest(args []string) int {
	fs := flag.NewFlagSet("policy test", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		rulesArg = fs.String("rules", "", "rule file to test against (default: the installed one)")
		asJSON   = fs.Bool("json", false, "emit machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog policy test [--rules <file>] [--json] <create-body.json>|-

Judges a container-create body against the rules and says which rule, if any,
refuses it — without running anything. "-" reads the body from stdin.

The body is the JSON the docker CLI POSTs to /containers/create. The quickest
way to get a real one is `+"`skrog audit tail`"+` on a machine with the audit log on,
or hand-write the fields you care about:

  echo '{"Image":"ubuntu","HostConfig":{"Privileged":true}}' | skrog policy test -

Exit codes: 0 allowed, %d error, %d usage, %d denied.
`, exitError, exitUsage, exitDenied)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}

	var (
		rules policy.Rules
		err   error
	)
	if *rulesArg != "" {
		var b []byte
		if b, err = os.ReadFile(*rulesArg); err == nil {
			rules, err = policy.Parse(b)
		}
	} else {
		// Layered, because `policy test` answers "would this be refused on
		// this machine" and on a managed one the answer is mostly the machine
		// layer's. Testing only the user's file would tell people their
		// request is fine and let the engine refuse it anyway (#386).
		rules, _, err = policy.LoadLayered(stateDirFor(fs, *stateDir))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	raw, err := readBody(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	body, err := policy.DecodeCreateBody(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}

	d := rules.EvaluateCreate(body)
	if *asJSON {
		emitJSON(policyTestJSON{Denied: d.Denied, Rule: d.Rule, Reason: d.Reason})
	} else if d.Denied {
		fmt.Printf("DENIED by %s\n  %s\n", d.Rule, d.Reason)
	} else if rules.Empty() {
		fmt.Println("allowed (no rules are set)")
	} else {
		fmt.Println("allowed")
	}
	if d.Denied {
		return exitDenied
	}
	return exitOK
}

// readBody reads a create body from a file, or stdin for "-".
func readBody(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// describe renders the rule set as the sentences a reader can check against
// what they meant, rather than echoing the YAML back at them.
func describe(r policy.Rules) []string {
	var out []string
	if r.DenyPrivileged {
		out = append(out, "deny --privileged")
	}
	if r.DenyAddedCapabilities {
		out = append(out, "deny every --cap-add")
	}
	if len(r.DenyCapabilities) > 0 {
		out = append(out, fmt.Sprintf("deny capabilities: %s", joinList(r.DenyCapabilities)))
	}
	if r.DenyHostNamespaces {
		out = append(out, "deny host namespaces (--network/--pid/--ipc/--uts=host)")
	}
	if len(r.AllowBindSources) > 0 {
		out = append(out, fmt.Sprintf("bind mounts only from: %s", joinList(r.AllowBindSources)))
	}
	if len(r.AllowRegistries) > 0 {
		out = append(out, fmt.Sprintf("images only from: %s", joinList(r.AllowRegistries)))
	}
	if r.RequireDigest {
		out = append(out, "images must be pinned by digest")
	}
	if r.DenyUnattributableBuilds {
		// Says whether it is actually biting, because the rule is inert
		// without an allowlist and a reader should not have to infer that
		// from two lines further up.
		if len(r.AllowRegistries) > 0 {
			out = append(out, "deny `docker build` (unattributable while images are restricted)")
		} else {
			out = append(out, "deny unattributable builds — INERT: it needs allow-registries to bite")
		}
	}
	if r.DenyUnattributableImages {
		// Same treatment, same reason (#343).
		if len(r.AllowRegistries) > 0 {
			out = append(out, "deny images with no recorded provenance (loaded, imported or built locally)")
		} else {
			out = append(out, "deny unattributable images — INERT: it needs allow-registries to bite")
		}
	}
	return out
}
