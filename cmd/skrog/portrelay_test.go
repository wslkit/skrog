package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/doctor"
	"github.com/wslkit/skrog/internal/portrelay"
)

// syncRecorder is a scriptable stand-in for portrelay.Relay.Sync.
type syncRecorder struct {
	mu    sync.Mutex
	calls [][]portrelay.Port
	got   chan []portrelay.Port
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{got: make(chan []portrelay.Port, 64)}
}

func (r *syncRecorder) sync(_ context.Context, ports []portrelay.Port) {
	r.mu.Lock()
	r.calls = append(r.calls, ports)
	r.mu.Unlock()
	r.got <- ports
}

var onePort = []doctor.PublishedPort{{Container: "web", HostPort: 8080, Proto: "tcp"}}

// TestRelayLoopSyncsOnAnEventWithoutWaitingForTheTick is the point of #510: a
// container that starts is relayed when it starts, not up to one poll later.
// The interval is an hour, so a sync inside the deadline can only have come
// from the event.
func TestRelayLoopSyncsOnAnEventWithoutWaitingForTheTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := newSyncRecorder()
	go relayLoop(ctx, relayDeps{
		scopeLAN: func() bool { return true },
		serving:  func() bool { return true },
		ports:    func(context.Context) []doctor.PublishedPort { return onePort },
		watch: func(ctx context.Context, changed func()) error {
			changed()
			<-ctx.Done()
			return ctx.Err()
		},
		interval: time.Hour,
		retry:    time.Hour,
	}, rec.sync)

	select {
	case ports := <-rec.got:
		if len(ports) != 1 || ports[0].Number != 8080 {
			t.Errorf("synced %+v, want port 8080", ports)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an event did not trigger a sync; the relay is still waiting on the poll")
	}
}

// TestRelayLoopNeverListsAnEngineThatIsNotServing: listing the ports dials the
// engine, and a dial to a stopped distro boots it through the socat fallback
// (#82). With the engine down, the relay must drop its listeners and ask
// nothing -- neither the listing nor the event stream.
func TestRelayLoopNeverListsAnEngineThatIsNotServing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var listed, watched atomic.Int32
	rec := newSyncRecorder()
	go relayLoop(ctx, relayDeps{
		scopeLAN: func() bool { return true },
		serving:  func() bool { return false },
		ports: func(context.Context) []doctor.PublishedPort {
			listed.Add(1)
			return onePort
		},
		watch: func(ctx context.Context, changed func()) error {
			watched.Add(1)
			return nil
		},
		interval: 10 * time.Millisecond,
		retry:    10 * time.Millisecond,
	}, rec.sync)

	for i := 0; i < 5; i++ {
		select {
		case ports := <-rec.got:
			if ports != nil {
				t.Fatalf("synced %+v with the engine down, want the listeners dropped", ports)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the relay stopped ticking")
		}
	}
	if n := listed.Load(); n != 0 {
		t.Errorf("listed the ports %d times with the engine down", n)
	}
	if n := watched.Load(); n != 0 {
		t.Errorf("subscribed to events %d times with the engine down", n)
	}
}

// TestServingDialerRefusesWithoutDialing is the gate at the dial itself, which
// is what also covers a pass that began just before an idle stop.
func TestServingDialerRefusesWithoutDialing(t *testing.T) {
	inner := &countingDialer{}
	d := &servingDialer{serving: func() bool { return false }, inner: inner}
	if _, err := d.Dial(context.Background()); err != errEngineNotServing {
		t.Errorf("Dial with the engine down: err=%v, want errEngineNotServing", err)
	}
	if inner.n.Load() != 0 {
		t.Error("the gate dialed the engine anyway")
	}
}

type countingDialer struct{ n atomic.Int32 }

func (d *countingDialer) Dial(context.Context) (io.ReadWriteCloser, error) {
	d.n.Add(1)
	return nil, io.EOF
}

// tcpDialer reaches an httptest server, standing in for the engine transport.
type tcpDialer struct{ addr string }

func (d tcpDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

// TestContainerEventsAsksForStartAndDieAndReportsEach drives the subscription
// against a server speaking the engine's stream shape: one JSON object per
// event, on a response that stays open.
func TestContainerEventsAsksForStartAndDieAndReportsEach(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("filters")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, `{"Type":"container","Action":"start","id":"a"}`+"\n")
		io.WriteString(w, `{"Type":"container","Action":"die","id":"a"}`+"\n")
	}))
	defer srv.Close()

	var changes atomic.Int32
	watch := containerEvents(tcpDialer{strings.TrimPrefix(srv.URL, "http://")})
	err := watch(context.Background(), func() { changes.Add(1) })
	if err == nil {
		t.Fatal("the stream ended and watch reported no error")
	}
	// One for the subscription itself, one per event.
	if n := changes.Load(); n != 3 {
		t.Errorf("changed called %d times, want 3 (subscribed + start + die)", n)
	}
	for _, want := range []string{`"type":["container"]`, `"start"`, `"die"`} {
		if !strings.Contains(query, want) {
			t.Errorf("filters %q do not contain %s", query, want)
		}
	}
}

// TestContainerEventsRefusedSubscriptionIsNotAChange: an engine that answers
// an error has not told us anything happened, so nothing may be re-synced on
// its account.
func TestContainerEventsRefusedSubscriptionIsNotAChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var changes atomic.Int32
	watch := containerEvents(tcpDialer{strings.TrimPrefix(srv.URL, "http://")})
	if err := watch(context.Background(), func() { changes.Add(1) }); err == nil {
		t.Error("a 500 from the engine was not reported")
	}
	if changes.Load() != 0 {
		t.Error("a refused subscription was reported as a change")
	}
}
