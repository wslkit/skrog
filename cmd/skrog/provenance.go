package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wslkit/skrog/internal/imageref"
	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/provenance"
)

// imageProvenance implements pipeproxy.Provenance (#343).
//
// It lives here rather than in pipeproxy because answering "where did this
// image come from" needs two things that package deliberately does not own:
// the state dir the record lives in, and a connection to the engine to resolve
// a reference to an image ID.
//
// Every method fails OPEN. A resolve that cannot reach the engine, a store
// that cannot be written, a reference that does not parse -- none of them
// refuse anything. This is drift protection on a cooperating machine, not a
// boundary (#418 route 3), so a transport hiccup must never turn into a
// container that will not start.
type imageProvenance struct {
	stateDir string
	dialer   pipeproxy.Dialer
	log      *slog.Logger

	clientOnce sync.Once
	httpClient *http.Client
}

// resolveTimeout bounds the extra engine round trip. Short: it sits in front of
// a container create, and a user waiting to run something must not pay for a
// wedged engine twice -- the create itself is about to fail anyway.
const resolveTimeout = 5 * time.Second

// RecordPull notes where an image came from, after a pull the gate allowed.
//
// The reference is resolved to an image ID first, so a pull that reported
// failure inside a 200 stream and left nothing behind records nothing.
//
// That is NOT the same as being safe against laundering, and an earlier
// version of this comment claimed it was. `docker load` an image, tag it
// `ghcr.io/org/x:1`, then let a pull of that reference fail mid-stream: the
// tag still resolves -- to the LOADED image -- and this records it as pulled
// from ghcr.io. The bytes were never fetched from anywhere.
//
// Left as is, because the alternative is trusting a pull's own success
// reporting, which is the thing that is unreliable here. It is one more reason
// this feature is drift protection and not a boundary, and docs/policy.md says
// so.
func (p *imageProvenance) RecordPull(ref string) {
	id := p.resolve(ref)
	if id == "" {
		return
	}
	registry, _ := imageref.Registry(ref)
	e := provenance.Entry{
		ID:       id,
		Registry: registry,
		Ref:      ref,
		Source:   provenance.SourcePull,
	}
	if err := provenance.Record(p.stateDir, e); err != nil {
		p.logger().Warn("could not record image provenance", "error", err, "ref", logSafe(ref))
		return
	}
	// Cheap and a no-op while the store is small; this is the only place that
	// runs often enough to keep it bounded without a separate timer.
	// A no-op while the store is small, which is the usual case; this is the
	// only place that runs often enough to keep it bounded without a timer.
	// The callback is invoked only on a sweep that would actually evict, so the
	// engine round trip is not on the pull path.
	if err := provenance.Compact(p.stateDir, p.installedIDs); err != nil {
		p.logger().Warn("could not compact the image provenance store", "error", err)
	}
}

// Attributable resolves a reference and reports whether this machine recorded
// where those bytes came from.
func (p *imageProvenance) Attributable(ref string) (string, bool) {
	id := p.resolve(ref)
	if id == "" {
		// Not resolvable is not the same as unattributable: the image may
		// simply not be here yet, which is the ordinary `docker run` of
		// something about to be pulled. The pull itself is judged by DenyPull.
		return "", false
	}
	_, known, err := provenance.Lookup(p.stateDir, id)
	if err != nil {
		// Cannot READ the store is not "no record". Reporting an empty id makes
		// the gate treat this as unresolvable, which it allows -- the fail-open
		// direction docs/policy.md promises. Refusing every container because a
		// file could not be opened is the failure this feature must not have.
		p.logger().Warn("could not read the image provenance store; allowing the request",
			"error", err, "ref", logSafe(ref))
		return "", false
	}
	return id, known
}

// client is an http.Client over the engine transport, built once.
//
// An http.Client rather than a hand-rolled req.Write + http.ReadResponse, and
// that is the whole point: the hand-rolled version consulted the context only
// while DIALING. After the dial it blocked in ReadResponse with no deadline at
// all -- and the vsock dialer explicitly CLEARS the deadline it used for its
// own handshake before handing the connection over
// (internal/pipeproxy/dial_vsock_windows.go). So an engine that accepted the
// connection and then wedged -- a containerd hang, a full disk, the classic
// engine failure -- hung the caller forever.
//
// Forever mattered twice: Attributable runs BEFORE a container create is
// forwarded, so `docker run` hung with no output; and RecordPull runs on the
// relay goroutine after a pull, so rewriteBinds never returned, the bridge's
// client counter never decremented, and idle-stop was vetoed for the life of
// the process. Both are the failure this feature's own docs promise cannot
// happen ("it fails open").
//
// rwcConn is the same adapter cmd/skrog/idle.go uses for the same reason.
func (p *imageProvenance) client() *http.Client {
	p.clientOnce.Do(func() {
		p.httpClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					rwc, err := p.dialer.Dial(ctx)
					if err != nil {
						return nil, err
					}
					return rwcConn{rwc}, nil
				},
				// One question, one connection: an idle keep-alive to the
				// engine would itself look like bridge activity.
				DisableKeepAlives: true,
			},
		}
	})
	return p.httpClient
}

// resolve asks the engine for an image's ID.
//
// Everything here returns "" on failure. The caller treats that as "cannot
// say", never as "not allowed".
func (p *imageProvenance) resolve(ref string) string {
	if ref == "" || p.dialer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	// The path is set after construction so the reference is escaped by
	// URL.EscapedPath rather than parsed as a URL: a reference carries slashes
	// and a colon, and "ghcr.io/org/img:1" is not a URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/images/x/json", nil)
	if err != nil {
		return ""
	}
	req.URL.Path = "/images/" + ref + "/json"

	resp, err := p.client().Do(req)
	if err != nil {
		p.logger().Debug("provenance: could not ask the engine about an image",
			"error", err, "ref", logSafe(ref))
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 is the common one: the image is not present. That is a fact
		// about the machine, not an error to report.
		return ""
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.ID
}

func (p *imageProvenance) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// SeedExisting records the images already on this machine, once (#343).
//
// Without it, turning deny-unattributable-images on would refuse a machine's
// entire existing image cache: nothing pulled before the feature shipped has a
// record. Refusing everything someone already had is not a defensible default,
// and it is the fastest way to have a security feature switched back off.
//
// They are recorded as "pre-existing" rather than as pulls, because that is
// what they are. `skrog policy show` reports the count, so "everything from
// before today is trusted" is visible rather than implied.
//
// A no-op once the store exists, so it cannot relabel history on every start.
// It runs off the caller's goroutine and gives up quietly: an engine that is
// down at logon simply means the next start seeds instead.
func (p *imageProvenance) SeedExisting(ctx context.Context) {
	if _, err := os.Stat(filepath.Join(p.stateDir, provenance.FileName)); err == nil {
		return
	}
	ids := p.localImageIDs(ctx)
	if len(ids) == 0 {
		return
	}
	n, err := provenance.Seed(p.stateDir, ids)
	if err != nil {
		p.logger().Warn("could not seed image provenance", "error", err)
		return
	}
	if n > 0 {
		p.logger().Info("recorded the images already on this machine as pre-existing",
			"images", n, "note", "they are trusted by deny-unattributable-images; `skrog policy show` says how many")
	}
}

// localImageIDs lists every image the engine holds.
func (p *imageProvenance) localImageIDs(ctx context.Context) []string {
	if p.dialer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/images/json", nil)
	if err != nil {
		return nil
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var images []struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		return nil
	}
	ids := make([]string, 0, len(images))
	for _, im := range images {
		if im.ID != "" {
			ids = append(ids, im.ID)
		}
	}
	return ids
}

// logSafe makes a value that arrived off the wire safe to put in a log line.
//
// An image reference is whatever the client sent -- any local process can open
// the pipe and put a newline, an ANSI escape or a megabyte in it. Written
// straight into a log that an operator reads, and that `skrog doctor --report`
// pastes into an issue, that is forged log lines at best (CodeQL
// go/log-injection). slog's own text handler happens to quote this, but the
// handler is not this code's to choose, so the escaping happens here.
//
// Replaced rather than dropped, so a reference that was tampered with still
// looks wrong in the log instead of looking tidy.
//
// The cap is on the OUTPUT, and that is not a detail: truncating the input
// first and escaping afterwards lets a reference made of control characters
// expand threefold past the bound, because U+FFFD is three bytes and the
// character it replaces is one.
func logSafe(s string) string {
	const max = 256
	var b strings.Builder
	b.Grow(min(len(s), max) + 3)
	truncated := false
	for _, r := range s {
		rep := string(r)
		if r == utf8.RuneError || unicode.IsControl(r) {
			rep = "�"
		}
		if b.Len()+len(rep) > max {
			truncated = true
			break
		}
		b.WriteString(rep)
	}
	if truncated || b.Len() < len(s) {
		b.WriteString("...")
	}
	return b.String()
}

// installedIDs is the set of image IDs the engine still holds, for compaction.
//
// nil on any failure, which Compact reads as "cannot say" and responds to by
// evicting nothing. Growing the store is recoverable; evicting the only record
// of an image that is still installed refuses a container that should run.
func (p *imageProvenance) installedIDs() map[string]bool {
	ids := p.localImageIDs(context.Background())
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}
