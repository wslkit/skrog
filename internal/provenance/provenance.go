// Package provenance records where an image on this machine came from (#343).
//
// # The hole it closes
//
// allow-registries judges the REFERENCE in a request. Nothing recorded where
// an image actually came from, so any image could be renamed into an allowed
// reference and run:
//
//	docker load -i bad.tar
//	docker tag bad contoso.azurecr.io/anything:1
//	docker run contoso.azurecr.io/anything:1     # passes
//
// # Why entries are keyed by image ID
//
// The cheap version of this records "reference R was pulled from registry X"
// and judges by reference. It closes the example above and dies to a one-line
// variation: pull something legitimate at that reference first, creating a
// record, then load and tag over it. A reference is a mutable label, so
// provenance bound to one attests to a name that anything can be renamed to --
// which is the mistake the allowlist already makes, one level down.
//
// The image ID is the config digest: content-addressed, and what a container
// create ultimately resolves to. Renaming an image keeps its ID, so a record
// follows the content rather than the label. That is also why tag is not
// gated: renaming an image with a record keeps the record, and renaming one
// without does not create one.
//
// # What this is NOT
//
// It is not a boundary against someone who sets out to get around it. The
// engine distro is registered in the user's own WSL installation, so
// `wsl -d skrog-engine -u root docker load -i bad.tar` never touches the pipe
// and cannot be gated here (#418, route 3).
//
// It protects against the accident and the drift: a base image that arrived
// through a docker load in a script, a CI job importing a tarball, a mirror
// nobody vetted. Those are real, and they are what an allowlist is for on a
// cooperating machine. Enforcement is opt-in for the same reason -- see
// policy.Rules.DenyUnattributableImages.
//
// # Storage
//
// An append-only JSONL file. Appends rather than read-modify-write because the
// writer is a long-lived bridge handling concurrent connections, and because
// nothing stops a second skrog process from touching the same state dir. A
// read-modify-write would lose records silently under either; an append
// cannot. Compaction rewrites the file when it grows, keeping the newest entry
// per ID.
package provenance

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName is the store, inside the state dir.
const FileName = "image-provenance.jsonl"

// Source says how an image came to be on this machine.
type Source string

const (
	// SourcePull is an image the gate allowed and watched arrive.
	//
	// This covers more than a user typing `docker pull`. `skrog prewarm`
	// drives the docker CLI, so its pulls cross the bridge like any other and
	// are recorded here with no special case.
	//
	// There is deliberately no "bundle" source. The air-gap bundle (#75)
	// carries a rootfs, not container images, so nothing loads images from it
	// and a source with no producer would be dead code claiming a guarantee.
	SourcePull Source = "pull"
	// SourcePreExisting is an image that was already here when provenance
	// recording started.
	//
	// Seeded rather than left unknown, and named rather than disguised as a
	// pull: on a machine with images predating the feature, refusing the whole
	// cache is not a defensible default, and "everything before today is
	// trusted" is a smaller lie when it says so out loud. `skrog policy show`
	// reports how many entries are of this kind.
	SourcePreExisting Source = "pre-existing"
)

// Entry is one image's provenance.
type Entry struct {
	// ID is the image ID (config digest), the content identity a container
	// create resolves to.
	ID string `json:"id"`
	// Registry is the host the image was fetched from. Empty for a source
	// that is not a registry.
	Registry string `json:"registry,omitempty"`
	// Ref is the reference as it was requested, kept for reporting. It is
	// never what a judgement is made on.
	Ref string `json:"ref,omitempty"`
	// Source is how it arrived.
	Source Source `json:"source"`
	// At is when the record was made.
	At time.Time `json:"at"`
}

// maxEntries bounds the store. Image IDs accumulate on a machine that pulls a
// lot, and a file nobody prunes is a file that eventually becomes the problem.
//
// There is deliberately NO age limit any more. One was tried, at 180 days, and
// it is a time bomb: a base image pulled seven months ago and still installed
// loses its only record, and `deny-unattributable-images` then refuses a
// container that has been running fine since spring. Age says nothing about
// whether an image is still on the machine -- so the cap is on count, and
// compaction asks the caller which images still exist before evicting any.
const maxEntries = 4096

// maxLineBytes bounds one record. Large enough that no honest entry reaches it
// (an entry is an ID, a registry, a reference and a timestamp), small enough
// that a corrupt file cannot make the reader allocate without limit.
const maxLineBytes = 1024 * 1024

// mu serialises this process's writes. It does not coordinate with another
// process -- the append is what makes that safe.
//
// With ONE exception, stated rather than glossed: Compact is a
// read-modify-write, so a second skrog process appending between its read and
// its rename loses those records. Compaction only runs on a store past
// maxEntries, and a lost record means a refused container, so this is a real
// if narrow hole. It is not closed with a lock file because the failure needs
// two skrog processes writing the same state dir at the same moment AND a
// store over the cap; naming it here is worth more than a lock nobody can
// test the absence of.
var mu sync.Mutex

func storePath(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Record appends an entry. A record for an ID that already has one is kept:
// Lookup takes the newest, and compaction drops the rest.
func Record(stateDir string, e Entry) error {
	if e.ID == "" {
		return fmt.Errorf("provenance: an entry needs an image ID")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("provenance: creating state dir: %w", err)
	}
	f, err := os.OpenFile(storePath(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("provenance: opening the store: %w", err)
	}
	// Closed explicitly, and its error returned: a deferred Close on a
	// WRITABLE file throws away the one error that says the record did not
	// reach the disk. Write can succeed into a buffer that Close then fails to
	// flush, and a provenance store that silently lost its last append is the
	// exact failure this package exists to make visible.
	if _, werr := f.Write(append(line, '\n')); werr != nil {
		// Both, not just the first. A failed Write and a Close that then also
		// failed are two different things to have gone wrong, and dropping the
		// second is the same data loss the deferred Close used to hide, just
		// on the path where something had already gone wrong.
		return fmt.Errorf("provenance: appending: %w", errors.Join(werr, f.Close()))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("provenance: closing the store: %w", err)
	}
	return nil
}

// supersedes reports whether a LATER LINE should replace the current winner
// for an image ID.
//
// "Not before" rather than "after", and the difference is the whole point: the
// store is append-only, so line order is arrival order, and a strict `After`
// makes two records written in the same clock tick a tie that resolves to
// whichever the loop saw first -- the OLDER one. That is not hypothetical on
// Windows, where the wall clock granularity is coarse enough that a seed and
// the pull immediately after it share a timestamp; it turned a pulled image
// back into a pre-existing one, and CI caught it on main (#343).
//
// Position is the tie-break because position is the fact: a line further down
// the file was appended later, whatever the clock said at the time.
func supersedes(candidate, current Entry) bool {
	return !candidate.At.Before(current.At)
}

// Lookup returns the newest record for an image ID.
//
// Three outcomes, not two, and the third is the one that matters: an error
// means the store could not be READ, which is not the same as the store having
// nothing to say. A caller that collapses them refuses a container because a
// file was unreadable -- see the fail-open promise in docs/policy.md.
func Lookup(stateDir, id string) (Entry, bool, error) {
	if id == "" {
		return Entry{}, false, nil
	}
	entries, err := read(stateDir)
	if err != nil {
		return Entry{}, false, err
	}
	var found Entry
	var ok bool
	for _, e := range entries {
		if e.ID == id && (!ok || supersedes(e, found)) {
			found, ok = e, true
		}
	}
	return found, ok, nil
}

// Stats is what `skrog policy show` and doctor report: how much of this
// machine's image set can be attributed, before anything is refused for not
// being.
type Stats struct {
	Total       int `json:"total"`
	Pulled      int `json:"pulled"`
	PreExisting int `json:"preExisting"`
}

// Summarize counts the newest record per ID.
//
// The error matters here for a different reason than in Lookup: a report that
// silently under-counts is worse than one that says it could not read the
// store, because the number is exactly what someone uses to decide whether it
// is safe to turn the rule on.
func Summarize(stateDir string) (Stats, error) {
	entries, err := read(stateDir)
	if err != nil {
		return Stats{}, err
	}
	newest := map[string]Entry{}
	for _, e := range entries {
		if prev, ok := newest[e.ID]; !ok || supersedes(e, prev) {
			newest[e.ID] = e
		}
	}
	var s Stats
	for _, e := range newest {
		s.Total++
		switch e.Source {
		case SourcePull:
			s.Pulled++
		case SourcePreExisting:
			s.PreExisting++
		}
	}
	return s, nil
}

// SeededFileName marks that this machine's pre-existing images were recorded.
//
// A marker of its own, and NOT the store's existence, which is what the first
// version used (#343). The store is also created by the first recorded pull,
// and on the ordinary first boot the pull comes first: the supervisor starts
// while the engine is still down, the seed finds no images and correctly does
// nothing, then a `docker pull` cold-starts the engine and creates the store.
// Every later seed then short-circuited on "the file exists" and the machine's
// entire pre-upgrade image cache stayed unattributable forever, silently --
// the outcome SourcePreExisting exists to prevent.
const SeededFileName = "image-provenance.seeded"

// Seeded reports whether this machine's pre-existing images have been
// recorded. Callers use it to decide whether to pay for an image listing.
func Seeded(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, SeededFileName))
	return err == nil
}

// Seed records images that were already present, once.
//
// The marker is written even when the list is EMPTY of usable ids, because
// "this machine had nothing" is just as much an answer as a list -- but the
// caller decides whether it got a real answer: an engine that could not be
// reached must not be recorded as a machine with no images. See the contract
// on the ids argument.
//
// ids must be the engine's real image list. Pass nil only when the engine
// answered and genuinely holds nothing.
func Seed(stateDir string, ids []string) (seeded int, err error) {
	if Seeded(stateDir) {
		return 0, nil
	}

	now := time.Now()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := Record(stateDir, Entry{ID: id, Source: SourcePreExisting, At: now}); err != nil {
			return seeded, err
		}
		seeded++
	}

	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return seeded, fmt.Errorf("provenance: creating state dir: %w", err)
	}
	// Written last: a marker written before the records would, if the process
	// died between the two, leave a machine that believes it seeded and did
	// not. This way the worst case is seeding twice, which is idempotent --
	// the same ids, the same source, and Lookup takes the newest.
	if err := os.WriteFile(filepath.Join(stateDir, SeededFileName), nil, 0o644); err != nil {
		return seeded, fmt.Errorf("provenance: recording the seed marker: %w", err)
	}
	return seeded, nil
}

// Compact rewrites the store with one entry per ID, keeping the newest
// maxEntries. A no-op while the file is small, which is the usual case.
//
// stillHere is consulted ONLY when compaction would actually evict something,
// so the caller pays for an engine round trip on the rare sweep rather than on
// every pull. It returns the set of image IDs the engine still holds; a record
// for one of those is never evicted, however old.
//
// That guard is the difference between a bound and a time bomb. Evicting by
// age or by position alone says nothing about whether the image is still on
// the machine, so a base image pulled long ago and used every day loses its
// only record and `deny-unattributable-images` then refuses a container that
// has worked for months. nil means "cannot say what is installed", and then
// nothing is evicted at all: growing the file is recoverable, refusing a
// container that should run is what this feature promises not to do.
func Compact(stateDir string, stillHere func() map[string]bool) error {
	mu.Lock()
	defer mu.Unlock()

	entries, err := readLocked(stateDir)
	if err != nil {
		return err
	}
	if len(entries) <= maxEntries {
		return nil
	}

	// Winners and the cap are both decided by POSITION, not by the clock.
	//
	// In an append-only log the order records arrived in is a fact, and it is
	// the one thing a coarse clock cannot blur -- which matters here for the
	// same reason it matters in supersedes: a seed and the pull right after it
	// routinely share a timestamp on Windows.
	newestIdx := map[string]int{}
	for i, e := range entries {
		if j, ok := newestIdx[e.ID]; !ok || supersedes(e, entries[j]) {
			newestIdx[e.ID] = i
		}
	}
	idxs := make([]int, 0, len(newestIdx))
	for _, i := range newestIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	if len(idxs) <= maxEntries {
		// Deduplication alone got us under the cap; nothing has to be evicted,
		// so nothing needs to be asked about.
		return writeCompacted(stateDir, entries, idxs)
	}

	var keep map[string]bool
	if stillHere != nil {
		keep = stillHere()
	}
	if keep == nil {
		// Cannot say what is installed: keep everything rather than evict
		// something that is.
		return writeCompacted(stateDir, entries, idxs)
	}

	// Evict from the OLDEST end, skipping anything still installed.
	over := len(idxs) - maxEntries
	pruned := make([]int, 0, len(idxs))
	for _, i := range idxs {
		if over > 0 && !keep[entries[i].ID] {
			over--
			continue
		}
		pruned = append(pruned, i)
	}
	return writeCompacted(stateDir, entries, pruned)
}

// writeCompacted replaces the store with the entries at the given indices, in
// file order. Caller holds mu.
func writeCompacted(stateDir string, entries []Entry, idxs []int) error {
	var b strings.Builder
	for _, i := range idxs {
		line, err := json.Marshal(entries[i])
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := storePath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("provenance: writing the compacted store: %w", err)
	}
	if err := os.Rename(tmp, storePath(stateDir)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("provenance: committing the compacted store: %w", err)
	}
	return nil
}

func read(stateDir string) ([]Entry, error) {
	mu.Lock()
	defer mu.Unlock()
	return readLocked(stateDir)
}

// readLocked parses the store.
//
// The error is load-bearing and must not be collapsed into "no records"
// (#343). A caller that cannot tell "the store says nothing about this image"
// from "the store could not be read" refuses a container for a missing disk,
// a permissions change or a half-written file -- which is the opposite of what
// this feature promises, and the opposite of what its own documentation says.
//
// A truncated FINAL line is still expected rather than exceptional: an append
// interrupted by a power loss leaves one, and losing the last record is not a
// reason to refuse to read the rest. That is why a line that will not parse is
// skipped, while a failure of the READ itself is reported.
func readLocked(stateDir string) ([]Entry, error) {
	f, err := os.Open(storePath(stateDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No store is a fact, not a failure: nothing has been recorded.
			return nil, nil
		}
		return nil, fmt.Errorf("provenance: reading the store: %w", err)
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.ID == "" {
			continue
		}
		out = append(out, e)
	}
	// Checked, and this is the half that was missing: without it a read error
	// partway through -- or one line longer than the scanner's buffer -- ends
	// the loop silently and every record AFTER it disappears, with the result
	// indistinguishable from a store that never held them.
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("provenance: reading the store: %w", err)
	}
	return out, nil
}
