package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
}

// resolveTimeout bounds the extra engine round trip. Short: it sits in front of
// a container create, and a user waiting to run something must not pay for a
// wedged engine twice -- the create itself is about to fail anyway.
const resolveTimeout = 5 * time.Second

// RecordPull notes where an image came from, after a pull the gate allowed.
//
// The reference is resolved to an image ID first, which is also what makes
// this safe against a pull that reported failure inside a 200 stream: an image
// that is not there does not resolve, and nothing is recorded.
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
	if err := provenance.Compact(p.stateDir); err != nil {
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
	_, known := provenance.Lookup(p.stateDir, id)
	return id, known
}

// resolve asks the engine for an image's ID.
//
// A second connection rather than the client's: the client's is mid-request,
// and this has to happen before that request is forwarded. pipeproxy.Dialer is
// exactly the seam for it -- the bridge has never originated a request before,
// and this is the first thing that needs to.
//
// Everything here returns "" on failure. The caller treats that as "cannot
// say", never as "not allowed".
func (p *imageProvenance) resolve(ref string) string {
	if ref == "" || p.dialer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	conn, err := p.dialer.Dial(ctx)
	if err != nil {
		p.logger().Debug("provenance: could not reach the engine to resolve an image",
			"error", err, "ref", logSafe(ref))
		return ""
	}
	defer conn.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/images/x/json", nil)
	if err != nil {
		return ""
	}
	// Set the path after construction so the reference is escaped by
	// URL.EscapedPath rather than parsed as a URL: a reference carries slashes
	// and a colon, and "ghcr.io/org/img:1" is not a URL.
	req.URL.Path = "/images/" + ref + "/json"
	req.Host = "engine"

	if err := req.Write(conn); err != nil {
		return ""
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
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

	conn, err := p.dialer.Dial(ctx)
	if err != nil {
		return nil
	}
	defer conn.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/images/json", nil)
	if err != nil {
		return nil
	}
	req.Host = "engine"
	if err := req.Write(conn); err != nil {
		return nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
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
func logSafe(s string) string {
	const max = 256
	truncated := false
	if len(s) > max {
		s, truncated = s[:max], true
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	if truncated {
		b.WriteString("...")
	}
	return b.String()
}
