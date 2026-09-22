package provenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordAndLookup(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := Lookup(dir, "sha256:aaa"); ok {
		t.Fatal("found a record in an empty store")
	}
	if err := Record(dir, Entry{ID: "sha256:aaa", Registry: "ghcr.io", Ref: "ghcr.io/x:1", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := Lookup(dir, "sha256:aaa")
	if !ok {
		t.Fatal("recorded entry not found")
	}
	if got.Registry != "ghcr.io" || got.Source != SourcePull {
		t.Errorf("entry = %+v", got)
	}
	if got.At.IsZero() {
		t.Error("a record with no timestamp cannot be compacted or aged out")
	}
}

// The store is append-only, so an ID can have several records. The newest
// wins: an image retagged and repulled from somewhere else is now from
// somewhere else.
func TestLookupTakesTheNewestRecord(t *testing.T) {
	dir := t.TempDir()
	older := time.Now().Add(-time.Hour)
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "old.example.com", Source: SourcePull, At: older}); err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "new.example.com", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Lookup(dir, "sha256:a")
	if got.Registry != "new.example.com" {
		t.Errorf("registry = %q, want the newest record", got.Registry)
	}
}

// An entry with no ID is meaningless: the ID is the whole point, because a
// reference is a mutable label.
func TestRecordRefusesAnEntryWithNoID(t *testing.T) {
	if err := Record(t.TempDir(), Entry{Registry: "ghcr.io", Source: SourcePull}); err == nil {
		t.Error("recorded an entry with no image ID")
	}
}

// A truncated final line is what an interrupted append leaves. Losing one
// record is not a reason to refuse to read the rest.
func TestReadSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if err := Record(dir, Entry{ID: "sha256:good", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"sha256:trunc","sou`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, ok, _ := Lookup(dir, "sha256:good"); !ok {
		t.Error("a truncated trailing line hid the records before it")
	}
}

// A corrupt line in the MIDDLE must not hide the records after it.
//
// The test above only appends garbage at the end and only checks the record
// before it, so it cannot fail for the bug its name describes. This one can:
// unparsable lines are skipped individually, and everything after one is still
// read.
func TestAnUnparsableLineDoesNotHideTheRecordsAfterIt(t *testing.T) {
	dir := t.TempDir()
	if err := Record(dir, Entry{ID: "sha256:before", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this is not json at all\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Record(dir, Entry{ID: "sha256:after", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := Lookup(dir, "sha256:after"); err != nil || !ok {
		t.Errorf("a corrupt line in the middle hid the record after it (ok=%v err=%v)", ok, err)
	}
}

// A store that cannot be READ is not a store with nothing in it, and the two
// must not be reported the same way.
//
// Collapsing them is a fail-open violation with a user-visible symptom:
// Attributable reports "no record", deny-unattributable-images refuses, and
// every container on the machine stops starting because one file could not be
// opened. docs/policy.md promises the opposite in as many words.
func TestAnUnreadableStoreIsAnError(t *testing.T) {
	dir := t.TempDir()
	// A directory where the store file belongs: open fails on every platform.
	if err := os.MkdirAll(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Lookup(dir, "sha256:a"); err == nil {
		t.Error("Lookup reported a clean miss for a store it could not read")
	}
	if _, err := Summarize(dir); err == nil {
		t.Error("Summarize reported counts for a store it could not read")
	}
}

// A line longer than the scanner's buffer must be an error, not a silent
// truncation of everything after it.
func TestAnOverlongLineIsReportedRatherThanSwallowed(t *testing.T) {
	dir := t.TempDir()
	if err := Record(dir, Entry{ID: "sha256:first", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", maxLineBytes+10) + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Record(dir, Entry{ID: "sha256:last", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Lookup(dir, "sha256:last"); err == nil {
		t.Error("an over-long line silently truncated the store; every record after " +
			"it vanished and nothing said so")
	}
}

// Seeding is once. A machine's history must not be relabelled on every
// supervisor start -- that would quietly turn every image ever loaded into a
// trusted one.
func TestSeedOnlyRunsOnce(t *testing.T) {
	dir := t.TempDir()
	n, err := Seed(dir, []string{"sha256:a", "sha256:b"})
	if err != nil || n != 2 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	n, err = Seed(dir, []string{"sha256:c"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("seeded %d on a store that already exists", n)
	}
	if _, ok, _ := Lookup(dir, "sha256:c"); ok {
		t.Error("a second seed added records")
	}
}

// Seeded images say what they are. Disguising them as pulls would make the
// report claim an origin nobody recorded.
func TestSeededEntriesAreNamedPreExisting(t *testing.T) {
	dir := t.TempDir()
	if _, err := Seed(dir, []string{"sha256:a"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Lookup(dir, "sha256:a")
	if got.Source != SourcePreExisting {
		t.Errorf("source = %q, want %q", got.Source, SourcePreExisting)
	}
	if got.Registry != "" {
		t.Errorf("registry = %q; a pre-existing image has no known origin", got.Registry)
	}
}

func TestSummarizeCountsEachIDOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := Seed(dir, []string{"sha256:a", "sha256:b"}); err != nil {
		t.Fatal(err)
	}
	// A repull of one of them: still one image.
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	st, _ := Summarize(dir)
	if st.Total != 2 {
		t.Errorf("total = %d, want 2", st.Total)
	}
	if st.Pulled != 1 || st.PreExisting != 1 {
		t.Errorf("pulled=%d preExisting=%d; the newest record per ID decides", st.Pulled, st.PreExisting)
	}
}

// The store must not grow without bound on a machine that pulls constantly.
//
// "nothing is still installed" is the case where everything is evictable, so
// this is the bound at its loosest.
func TestCompactBoundsTheStore(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEntries+50; i++ {
		if err := Record(dir, Entry{ID: "sha256:" + string(rune('a'+i%26)) + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	none := func() map[string]bool { return map[string]bool{"sha256:none": true} }
	if err := Compact(dir, none); err != nil {
		t.Fatal(err)
	}
	if got := len(mustRead(t, dir)); got > maxEntries {
		t.Errorf("store holds %d entries after compaction, cap is %d", got, maxEntries)
	}
}

// ...and it must never evict the record of an image that is STILL INSTALLED.
//
// This is the half that was missing, and the bug it hides is a time bomb: a
// base image pulled long ago and used every day loses its only record, and
// deny-unattributable-images then refuses a container that has worked for
// months. Nothing about age or arrival order says whether an image is still on
// the machine, so compaction has to ask.
func TestCompactNeverEvictsAnImageThatIsStillInstalled(t *testing.T) {
	dir := t.TempDir()
	// The one under test goes in FIRST, so eviction by age or by position
	// would take it.
	if err := Record(dir, Entry{ID: "sha256:base", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxEntries+200; i++ {
		if err := Record(dir, Entry{ID: "sha256:filler" + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	stillHere := func() map[string]bool { return map[string]bool{"sha256:base": true} }
	if err := Compact(dir, stillHere); err != nil {
		t.Fatal(err)
	}

	got, ok, err := Lookup(dir, "sha256:base")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("compaction evicted the only record of an image that is still installed; " +
			"`docker run` on it would now be refused")
	}
	if got.Registry != "ghcr.io" {
		t.Errorf("record survived but lost its origin: %+v", got)
	}
}

// And when the engine cannot say what is installed, nothing is evicted at all.
// Growing the file is recoverable; refusing a container that should run is not
// what this feature promises.
func TestCompactEvictsNothingWhenTheInstalledSetIsUnknown(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEntries+50; i++ {
		if err := Record(dir, Entry{ID: "sha256:x" + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(mustRead(t, dir))
	if err := Compact(dir, func() map[string]bool { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(mustRead(t, dir)); got != before {
		t.Errorf("store went from %d to %d entries while the installed set was unknown", before, got)
	}
}

// Compaction keeps the newest record per ID, so it must not lose a lookup.
func TestCompactKeepsTheNewestPerID(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEntries+10; i++ {
		if err := Record(dir, Entry{ID: "sha256:filler" + itoa(i), Source: SourcePull}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Record(dir, Entry{ID: "sha256:keep", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	if err := Compact(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := Lookup(dir, "sha256:keep")
	if !ok || got.Registry != "ghcr.io" {
		t.Errorf("compaction dropped the newest record: %+v ok=%v", got, ok)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Two records written in the same clock tick must resolve by FILE ORDER, not
// by whichever the loop happened to see first.
//
// This is not a hypothetical. `Seed` stamps time.Now() and a pull recorded
// immediately after stamps time.Now() again; on Windows those are routinely
// the same value, and a strict `After` comparison then keeps the SEED -- so a
// pulled image reads back as pre-existing. It reached main and CI caught it.
func TestTheLaterLineWinsOnAnIdenticalTimestamp(t *testing.T) {
	dir := t.TempDir()
	same := time.Now()
	if err := Record(dir, Entry{ID: "sha256:a", Source: SourcePreExisting, At: same}); err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, Entry{ID: "sha256:a", Registry: "ghcr.io", Source: SourcePull, At: same}); err != nil {
		t.Fatal(err)
	}

	got, ok, _ := Lookup(dir, "sha256:a")
	if !ok {
		t.Fatal("no record found")
	}
	if got.Source != SourcePull || got.Registry != "ghcr.io" {
		t.Errorf("Lookup = %+v, want the later line (the pull)", got)
	}
	if st, _ := Summarize(dir); st.Pulled != 1 || st.PreExisting != 0 {
		t.Errorf("Summarize: pulled=%d preExisting=%d, want 1/0 — the later line decides",
			st.Pulled, st.PreExisting)
	}
}

// ...and compaction must decide by position too, or it drops the record that
// is actually current.
//
// Every entry here shares one timestamp, so the clock cannot order any of
// them: only arrival position can say that the pull came after the seed, and
// that "sha256:x" is newer than the filler. Capping by time rather than by
// position drops it.
//
// (An earlier version of this test claimed to pin the ORDER the compacted
// block is written in. It did not, and could not: compaction leaves one line
// per ID, so there is no tie left in the output to resolve. Reversing the
// write order left it green, which is how that was found.)
func TestCompactDecidesByPositionWhenEveryTimestampIsEqual(t *testing.T) {
	dir := t.TempDir()
	same := time.Now()
	for i := 0; i < maxEntries; i++ {
		if err := Record(dir, Entry{ID: "sha256:filler" + itoa(i), Source: SourcePull, At: same}); err != nil {
			t.Fatal(err)
		}
	}
	// The pair under test, last and sharing every other record's timestamp.
	if err := Record(dir, Entry{ID: "sha256:x", Source: SourcePreExisting, At: same}); err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, Entry{ID: "sha256:x", Registry: "ghcr.io", Source: SourcePull, At: same}); err != nil {
		t.Fatal(err)
	}
	if err := Compact(dir, nil); err != nil {
		t.Fatal(err)
	}

	got, ok, _ := Lookup(dir, "sha256:x")
	if !ok {
		t.Fatal("compaction dropped the newest record; the cap did not keep the tail")
	}
	if got.Source != SourcePull {
		t.Errorf("after compaction Lookup = %+v, want the pull — the later line is the current one", got)
	}
}

// The Write error must still be identifiable when Close also fails: the
// joined error is for reporting both, not for burying the first.
func TestAppendFailureStillReportsTheWriteError(t *testing.T) {
	dir := t.TempDir()
	// A directory where the store file must go: the open succeeds on some
	// platforms and the write does not, which is the shape this path handles.
	// Where the open fails instead, the error still names the store.
	if err := os.MkdirAll(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Record(dir, Entry{ID: "sha256:a", Source: SourcePull})
	if err == nil {
		t.Fatal("recording into a directory succeeded")
	}
	if !strings.Contains(err.Error(), "provenance:") {
		t.Errorf("error does not identify the store: %v", err)
	}
}

func mustRead(t *testing.T, dir string) []Entry {
	t.Helper()
	got, err := read(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// Seeding is gated on its OWN marker, not on the store existing.
//
// This is the ordinary first boot and the first version got it wrong: the
// supervisor starts while the engine is still down, so the seed correctly
// records nothing; then a `docker pull` cold-starts the engine and creates the
// store. Gating on "does the store exist" then short-circuits every later
// seed, and the machine's whole pre-upgrade image cache stays unattributable
// forever, silently.
func TestSeedStillRunsAfterAPullCreatedTheStore(t *testing.T) {
	dir := t.TempDir()

	// First boot: engine down, nothing to seed. Not marked as seeded, because
	// nothing was asked.
	if n, err := Seed(dir, nil); err != nil || n != 0 {
		t.Fatalf("empty seed: n=%d err=%v", n, err)
	}
	if !Seeded(dir) {
		t.Fatal("an answered-but-empty seed should still count as seeded")
	}

	// Now the realistic failure: a machine where the FIRST thing that happens
	// is a pull, before any seed ran at all.
	dir2 := t.TempDir()
	if err := Record(dir2, Entry{ID: "sha256:pulled", Registry: "ghcr.io", Source: SourcePull}); err != nil {
		t.Fatal(err)
	}
	n, err := Seed(dir2, []string{"sha256:old1", "sha256:old2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded %d; a store created by a pull must not disable seeding", n)
	}
	for _, id := range []string{"sha256:old1", "sha256:old2"} {
		if _, ok, _ := Lookup(dir2, id); !ok {
			t.Errorf("%s was never seeded, so it stays unattributable forever", id)
		}
	}
}
