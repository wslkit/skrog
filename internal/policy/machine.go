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
// which is administrator-writable and standard-user-readable by default, and
// the user layer stays where it was. They merge with one rule:
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
// It is still not a boundary against a local ADMINISTRATOR: they can edit the
// machine file, or uninstall Skrog. It IS a boundary against a standard user,
// which is the actual configuration of a managed corporate laptop, and it is
// deployable and auditable. docs/policy.md keeps its disclaimer and
// distinguishes the two cases rather than overclaiming.

// MachineDirEnv overrides where the machine layer is read from. It exists for
// tests and for a fleet that keeps ProgramData somewhere unusual; it is read
// from the environment rather than from config on purpose, because a setting a
// user could edit would defeat the layer entirely.
const MachineDirEnv = "SKROG_MACHINE_POLICY_DIR"

// MachineDir is the directory holding the machine-wide rule file.
func MachineDir() string {
	if v := strings.TrimSpace(os.Getenv(MachineDirEnv)); v != "" {
		return v
	}
	// ProgramData, not Program Files: it is the documented home for
	// machine-wide application data, administrator-writable and readable by
	// every user, which is exactly the ACL shape this layer wants.
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

// LoadMachine reads the machine-wide rule set. A missing file is an empty rule
// set, not an error: most machines have no fleet policy.
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
	return Parse(b)
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
	if p := MachinePath(); p != "" && fileExists(p) {
		src.MachinePath = p
		src.Machine = machine
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
