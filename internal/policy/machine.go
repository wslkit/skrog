package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Machine-wide policy (#386).
//
// policy.yaml in the state dir is something each DEVELOPER configures for
// themselves, which is the opposite of what a guardrail is for. The audience
// this project exists for — the organisation that was told to stop using
// Docker Desktop — has Intune and Group Policy and a security team who will
// ask "how do we enforce it?", and the honest answer used to be "you ask each
// developer nicely".
//
// So rules now come in two layers. The machine layer lives under ProgramData,
// the documented home for machine-wide application data, and the user layer
// stays where it was. They merge with one rule:
//
//	THE USER LAYER MAY ONLY TIGHTEN.
//
// Never relax, never remove. A developer may forbid more than the machine
// does; they may not permit anything it forbids. Merge() is where that lives,
// and it is written so the guarantee is checkable rather than asserted: see
// TestMergeNeverRelaxes.
//
// # What this does and does not buy
//
// It is not a boundary against a local ADMINISTRATOR: they can edit the
// machine file, or uninstall Skrog.
//
// It is NOT a boundary against a standard user either, which this comment and
// docs/policy.md both claimed until #418. What it is: tamper-evident fleet
// configuration, enforced at the pipe. It stops the realistic accident -- a
// developer loosening their own policy.yaml -- and `skrog policy show` reports
// what is in force. It does not stop someone who sets out to get around it,
// for three independent reasons, none of which the merge algebra can fix:
// MachineDirEnv below, the default ProgramData ACL, and the fact that the
// pipe is not the only route to the engine (`wsl -d <distro> -u root`,
// `proxy --no-path-translation`, `wsl-integrate`). #418 tracks closing them.

// MachineDirEnv overrides where the machine layer is read from. It exists for
// tests and for a fleet that keeps ProgramData somewhere unusual.
//
// It is read from the environment rather than from config, on the reasoning
// that "a setting a user could edit would defeat the layer entirely" -- which
// is true, and which this variable is. The supervisor runs as the ordinary
// user (docs/security.md), so the user owns its whole environment block and
// `setx SKROG_MACHINE_POLICY_DIR <empty dir>` retires the machine layer for
// every later logon. Config would have been no worse. See #418: the fix is a
// trusted location that cannot be redirected at all, not a different place to
// read the redirection from.
const MachineDirEnv = "SKROG_MACHINE_POLICY_DIR"

// MachineDir is the directory holding the machine-wide rule file.
func MachineDir() string {
	if v := strings.TrimSpace(os.Getenv(MachineDirEnv)); v != "" {
		return v
	}
	// ProgramData, not Program Files: it is the documented home for
	// machine-wide application data.
	//
	// It is NOT administrator-writable-only, which this comment used to say.
	// The default ACL carries BUILTIN\Users:(CI)(WD,AD) plus CREATOR
	// OWNER:(OI)(CI)(IO)(F), so a standard user can create ProgramData\skrog
	// first and own it. LoadMachine checks the OWNER now and refuses a file
	// that fails (#418) -- so the wrong ACL means the layer does not load,
	// rather than the user's own rules wearing the administrator's authority.
	// An earlier version of this comment said skrog "checks neither owner nor
	// DACL", forty lines above the function that does.
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "skrog")
	}
	return ""
}

// MachinePath is the machine-wide rule file, or "" where there is no
// ProgramData to speak of (a non-Windows test host).
func MachinePath() string {
	dir := MachineDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, FileName)
}

// trustMachineFile is the ownership check, indirected so tests can drive the
// refusal path without needing a real ProgramData and an administrator to
// create it. Production never replaces it.
var trustMachineFile = checkOwner

// LoadMachine reads the machine-wide rule set. A missing file is an empty rule
// set, not an error: most machines have no fleet policy.
//
// The file's provenance is checked before it is trusted (#418). A machine
// layer is only fleet configuration if the fleet wrote it; one a standard user
// could have written is the user's own rules wearing the machine layer's
// authority, which is worse than having no machine layer at all, because
// `skrog policy show` would report it as in force.
//
// An untrusted file is REFUSED rather than merged, and the refusal is
// reported: see MachineProvenance and `skrog policy show`. Refusing makes the
// effective rules the user's own, which is what they already were in
// substance -- the difference is that it now says so.
func LoadMachine() (Rules, error) {
	path := MachinePath()
	if path == "" {
		return Rules{}, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Rules{}, nil
	}
	if err != nil {
		return Rules{}, fmt.Errorf("policy: reading %s: %w", path, err)
	}
	if ok, _ := trustMachineFile(path); !ok {
		return Rules{}, nil
	}
	return Parse(b)
}

// Provenance describes where the machine layer came from and whether it was
// trusted, so the answer is visible rather than implied (#418).
//
// Tamper-evidence is the honest promise here. The layer cannot be made
// unbypassable — the supervisor runs as the user, and three routes around it
// are recorded in this file's opening comment — so the thing worth building is
// a report that does not lie about which of them happened.
type Provenance struct {
	// Path is the machine file that was considered, empty when there is none.
	Path string
	// Redirected is true when MachineDirEnv pointed somewhere other than the
	// default. On its own that is not an accusation: a fleet may keep
	// ProgramData elsewhere. It is reported because it is also the cheapest
	// way to retire the layer, and a reader deserves to know which they are
	// looking at.
	Redirected bool
	// RedirectedTo is the directory the variable named.
	RedirectedTo string
	// Trusted says whether the file passed the ownership check. False with a
	// non-empty Why means the layer was refused.
	Trusted bool
	// Why explains a refusal, in a sentence that completes "the machine
	// policy at <path> was ignored because ...".
	Why string
}

// MachineProvenance reports how the machine layer was resolved and whether it
// was trusted. It performs the same checks LoadMachine does, so the two cannot
// disagree.
func MachineProvenance() Provenance {
	var p Provenance
	if v := strings.TrimSpace(os.Getenv(MachineDirEnv)); v != "" {
		p.Redirected, p.RedirectedTo = true, v
	}
	p.Path = MachinePath()
	if p.Path == "" || !fileExists(p.Path) {
		p.Path = ""
		return p
	}
	ok, why := trustMachineFile(p.Path)
	p.Trusted, p.Why = ok, why
	return p
}

// Merge returns the effective rules: the machine layer, tightened by the user
// layer. It is total and side-effect free, so the guarantee it carries can be
// tested exhaustively rather than reasoned about.
//
// Per field:
//
//   - booleans OR, so a machine "deny" cannot be un-denied
//   - deny-lists UNION, for the same reason
//   - allow-lists INTERSECT, which is the subtle one: an allow-list is a
//     permission, so tightening means keeping FEWER entries, and a user entry
//     survives only if the machine layer already permitted it
func Merge(machine, user Rules) Rules {
	return Rules{
		DenyPrivileged:        machine.DenyPrivileged || user.DenyPrivileged,
		DenyAddedCapabilities: machine.DenyAddedCapabilities || user.DenyAddedCapabilities,
		DenyCapabilities:      unionCaps(machine.DenyCapabilities, user.DenyCapabilities),
		DenyHostNamespaces:    machine.DenyHostNamespaces || user.DenyHostNamespaces,
		RequireDigest:         machine.RequireDigest || user.RequireDigest,
		// A deny rule, so it ORs like the rest (#376). Note the interaction
		// worth thinking about once: the machine can set this while the USER
		// supplies the allow-registries that makes it bite, and that is the
		// correct outcome — the machine said "no unattributable builds where
		// registries are restricted", and they are.
		DenyUnattributableBuilds: machine.DenyUnattributableBuilds || user.DenyUnattributableBuilds,
		// Same algebra, same reasoning (#343): either layer may turn it on and
		// neither can turn the other's off, because the user layer may only
		// tighten.
		DenyUnattributableImages: machine.DenyUnattributableImages || user.DenyUnattributableImages,

		AllowBindSources: intersectAllow(machine.AllowBindSources, user.AllowBindSources,
			func(userEntry string, machineRoots []string) bool {
				return underAny(userEntry, machineRoots)
			}),
		AllowRegistries: intersectAllow(machine.AllowRegistries, user.AllowRegistries,
			func(userEntry string, machinePatterns []string) bool {
				return matchesAny(userEntry, machinePatterns)
			}),
	}
}

// unionCaps concatenates two deny-lists without duplicates, comparing the way
// the evaluator does so CAP_SYS_ADMIN and sys_admin do not both survive.
func unionCaps(machine, user []string) []string {
	if len(machine) == 0 && len(user) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{machine, user} {
		for _, c := range list {
			k := normalizeCap(c)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// intersectAllow tightens an allow-list.
//
// The empty list means "no restriction", which makes the cases worth spelling
// out rather than leaving to a clever one-liner:
//
//	machine empty, user empty  -> no restriction
//	machine empty, user set    -> the user's (they tightened themselves)
//	machine set,   user empty  -> the machine's
//	machine set,   user set    -> the user entries the machine already permits
//
// The last case is why this is not a set intersection. `C:\work\proj` is not
// equal to `C:\work`, but it is *under* it, so a machine that permits `C:\work`
// permits it. permits() decides that per rule kind.
func intersectAllow(machine, user []string, permits func(userEntry string, machine []string) bool) []string {
	switch {
	case len(machine) == 0:
		return user
	case len(user) == 0:
		return machine
	}
	var out []string
	for _, u := range user {
		if permits(u, machine) {
			out = append(out, u)
		}
	}
	// Every user entry fell outside what the machine permits. Returning the
	// user's list would grant what the machine forbade; returning nil would
	// mean "no restriction", which grants everything. So fall back to the
	// machine's list — the tightest honest answer available.
	if len(out) == 0 {
		return machine
	}
	return out
}

// Source describes where an effective rule set came from, for `policy show`.
type Source struct {
	// MachinePath is the machine file, when one exists.
	MachinePath string `json:"machinePath,omitempty"`
	// UserPath is the per-install file, when one exists.
	UserPath string `json:"userPath,omitempty"`
	// Machine and User are the layers as read, before merging, so a reader can
	// see what each contributed rather than only the result.
	Machine Rules `json:"machine,omitempty"`
	User    Rules `json:"user,omitempty"`
}

// LoadLayered reads both layers and returns the effective rules plus where
// they came from. A machine file that will not parse is an ERROR rather than
// an empty layer: silently ignoring an unreadable fleet policy would turn a
// typo in a deployment into an unenforced machine, which is precisely the
// failure the layer exists to prevent.
func LoadLayered(stateDir string) (Rules, Source, error) {
	var src Source

	machine, err := LoadMachine()
	if err != nil {
		return Rules{}, src, err
	}
	// Only a file that was actually trusted is reported as a contributing
	// layer (#418). Source describes what shaped the effective rules, and a
	// refused file shaped nothing -- listing it anyway made `policy show`
	// print the refusal and then, three lines later, "deployed machine-wide;
	// you cannot loosen these" about the same path.
	if p := MachinePath(); p != "" && fileExists(p) {
		if ok, _ := trustMachineFile(p); ok {
			src.MachinePath = p
			src.Machine = machine
		}
	}

	user, err := Load(stateDir)
	if err != nil {
		return Rules{}, src, err
	}
	if p := Path(stateDir); fileExists(p) {
		src.UserPath = p
		src.User = user
	}

	return Merge(machine, user), src, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
