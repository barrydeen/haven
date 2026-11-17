package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/blossom"
	"github.com/puzpuzpuz/xsync/v4"
)

//
// counting what the relay does
//
// The whole design rests on one property of khatru: RejectEvent, RejectFilter and
// RejectConnection are each run in order and return at the first hook that
// rejects (adding.go:26, responding.go:30, handlers.go:57). So a counter placed
// at index 0 counts everything that was offered, a counter appended at the end
// counts what survived every policy, and the difference is the rejections —
// without wrapping, reordering or otherwise touching a single existing policy.
//
// The counters are monotonic for the life of the process and are never reset.
// The flusher keeps the previous reading and stores the difference, which removes
// the entire class of bug where a reset races with an increment.
//

type counter int

// Entries may only ever be APPENDED. The name is what goes on disk and on the
// wire, so inserting one in the middle would silently re-label every bucket
// already written.
const (
	uptimeSeconds counter = iota

	connAttempts
	connAllowed
	connOpened
	connClosed
	connSeconds

	eventAttempts
	eventPassed
	eventStored
	eventBytes
	eventImported

	reqFilters
	reqSubscribeOnly
	reqPassed
	countFilters

	blobUploadAttempts
	blobUploadAllowed
	blobStored
	blobBytes
	blobServed
	blobDeleted

	counterCount
)

var counterNames = [counterCount]string{
	uptimeSeconds:      "uptime_seconds",
	connAttempts:       "conn_attempts",
	connAllowed:        "conn_allowed",
	connOpened:         "conn_opened",
	connClosed:         "conn_closed",
	connSeconds:        "conn_seconds",
	eventAttempts:      "event_attempts",
	eventPassed:        "event_passed",
	eventStored:        "event_stored",
	eventBytes:         "event_bytes",
	eventImported:      "event_imported",
	reqFilters:         "req_filters",
	reqSubscribeOnly:   "req_subscribe_only",
	reqPassed:          "req_passed",
	countFilters:       "count_filters",
	blobUploadAttempts: "blob_upload_attempts",
	blobUploadAllowed:  "blob_upload_allowed",
	blobStored:         "blob_stored",
	blobBytes:          "blob_bytes",
	blobServed:         "blob_served",
	blobDeleted:        "blob_deleted",
}

// counters is one relay's running totals. A plain atomic.Int64 rather than
// xsync.Counter: a personal relay does tens of events a second and one atomic add
// is about twenty nanoseconds, where xsync.Counter would spend 64 bytes per CPU
// per counter buying contention relief that will never be needed. If that ever
// changes, xsync.Counter is the drop-in.
type counters [counterCount]atomic.Int64

type sample [counterCount]int64

func (c *counters) add(k counter, n int64) { c[k].Add(n) }

func (c *counters) sample() sample {
	var s sample
	for i := range c {
		s[i] = c[i].Load()
	}
	return s
}

// relayMetrics is everything counted for one relay.
type relayMetrics struct {
	name       string
	c          counters
	kinds      *xsync.Map[int, *atomic.Int64]
	kindsOther atomic.Int64

	// conns is the live connection set, keyed on the websocket rather than being
	// a counter, because khatru calls OnDisconnect TWICE for every connection:
	// kill() is deferred in both the reader goroutine and the ping goroutine
	// (handlers.go:108 and :417), and the first one cancels the context the
	// second is waiting on. A LoadAndDelete makes the second call a no-op; a
	// naive Add(-1) would drift negative within minutes.
	conns *xsync.Map[*khatru.WebSocket, time.Time]
}

func newRelayMetrics(name string) *relayMetrics {
	return &relayMetrics{
		name:  name,
		kinds: xsync.NewMap[int, *atomic.Int64](),
		conns: xsync.NewMap[*khatru.WebSocket, time.Time](),
	}
}

func (m *relayMetrics) recordKind(kind int) {
	if n, ok := m.kinds.Load(kind); ok {
		n.Add(1)
		return
	}
	// the cap stops somebody publishing ten thousand distinct kinds from turning
	// this map into the relay's memory profile. It is read without a lock, so it
	// can be overshot by a few under concurrency: it is a bound, not an invariant.
	if m.kinds.Size() >= config.AnalyticsMaxKinds {
		m.kindsOther.Add(1)
		return
	}
	n, _ := m.kinds.LoadOrStore(kind, &atomic.Int64{})
	n.Add(1)
}

func (m *relayMetrics) kindSnapshot() map[int]int64 {
	out := make(map[int]int64)
	m.kinds.Range(func(kind int, n *atomic.Int64) bool {
		out[kind] = n.Load()
		return true
	})
	return out
}

// approxEventBytes estimates what one event costs to store.
//
// It is an estimate and is labelled as one everywhere it surfaces: the hook is
// handed a parsed event rather than the bytes that arrived, and serialising every
// accepted event purely to measure it would allocate a second copy of the relay's
// entire write traffic. It is never added to a figure measured from disk.
func approxEventBytes(evt *nostr.Event) int64 {
	// id, pubkey and sig are fixed width hex; the rest is JSON punctuation and
	// the kind and timestamp
	n := int64(len(evt.Content)) + 220
	for _, tag := range evt.Tags {
		for _, item := range tag {
			n += int64(len(item)) + 3
		}
	}
	return n
}

// instrument wires one relay into the metrics store.
//
// It MUST be called last in that relay's block in initRelays, after every policy
// has been appended. Called earlier, the "passed" counters would sit in front of
// policies that had not been registered yet and would count rejections as
// acceptances.
func instrument(relay *khatru.Relay, name string) {
	if !config.AnalyticsEnabled {
		return
	}
	m := metrics.relay(name)

	// The successor relay exposes every hook once, so the counters wrap the
	// hook init.go has already assigned rather than joining a slice.

	prevRejectConnection := relay.RejectConnection
	relay.RejectConnection = func(r *http.Request) bool {
		m.c.add(connAttempts, 1)
		if prevRejectConnection != nil && prevRejectConnection(r) {
			return true
		}
		// conn_allowed means "passed every connection policy", which is not the
		// same as connected: the websocket upgrade itself can still fail.
		m.c.add(connAllowed, 1)
		return false
	}

	prevOnConnect := relay.OnConnect
	relay.OnConnect = func(ctx context.Context) {
		if prevOnConnect != nil {
			prevOnConnect(ctx)
		}
		ws := khatru.GetConnection(ctx)
		if ws == nil {
			return
		}
		m.conns.Store(ws, time.Now())
		m.c.add(connOpened, 1)
	}

	prevOnDisconnect := relay.OnDisconnect
	relay.OnDisconnect = func(ctx context.Context) {
		ws := khatru.GetConnection(ctx)
		if ws != nil {
			// see relayMetrics.conns: khatru runs this twice per connection
			if at, ok := m.conns.LoadAndDelete(ws); ok {
				m.c.add(connClosed, 1)
				m.c.add(connSeconds, int64(time.Since(at).Seconds()))
			}
		}
		if prevOnDisconnect != nil {
			prevOnDisconnect(ctx)
		}
	}

	prevOnEvent := relay.OnEvent
	relay.OnEvent = func(ctx context.Context, evt nostr.Event) (bool, string) {
		m.c.add(eventAttempts, 1)
		if prevOnEvent != nil {
			if reject, msg := prevOnEvent(ctx, evt); reject {
				return reject, msg
			}
		}
		m.c.add(eventPassed, 1)
		return false, ""
	}

	// event_passed is not event_stored, and the gap is not an error: khatru runs
	// its own deleted-by-id check after the policy, a duplicate is answered OK and
	// never written, and an ephemeral event is never written at all. OnEventSaved
	// fires once per event that actually reached the database, covering both the
	// StoreEvent and the ReplaceEvent branch.
	prevOnEventSaved := relay.OnEventSaved
	relay.OnEventSaved = func(ctx context.Context, evt nostr.Event) {
		m.c.add(eventStored, 1)
		e := evt
		m.c.add(eventBytes, approxEventBytes(&e))
		m.recordKind(int(evt.Kind))
		if prevOnEventSaved != nil {
			prevOnEventSaved(ctx, evt)
		}
	}

	// Deliberately NOT hooked: OnEphemeralEvent. The successor answers an
	// ephemeral event nobody was listening for with OK:false only while no
	// handler is registered, so registering a counter there would silently
	// change what the relay tells its clients.

	// OnRequest runs before the LimitZero early return in khatru's REQ handler,
	// so one hook covers both counters. It is handed the filter by value and
	// must never write through anything.
	prevOnRequest := relay.OnRequest
	relay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		m.c.add(reqFilters, 1)
		if filter.LimitZero {
			m.c.add(reqSubscribeOnly, 1)
		}
		if prevOnRequest != nil {
			if reject, msg := prevOnRequest(ctx, filter); reject {
				return reject, msg
			}
		}
		m.c.add(reqPassed, 1)
		return false, ""
	}

	// COUNT: OnCount is the successor's count-reject hook, and this is the only
	// visibility there is into NIP-45 traffic. Appending a hook that never
	// rejects changes nothing about who is allowed to count.
	prevOnCount := relay.OnCount
	relay.OnCount = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		m.c.add(countFilters, 1)
		if prevOnCount != nil {
			return prevOnCount(ctx, filter)
		}
		return false, ""
	}
}

// instrumentBlossom counts media traffic. Blossom hangs off the outbox relay
// alone, so all of this lands in the outbox relay's counters.
func instrumentBlossom(bl *blossom.BlossomServer, name string) {
	if !config.AnalyticsEnabled {
		return
	}
	m := metrics.relay(name)

	// The successor blossom hooks are single functions, so the counters wrap
	// the hooks init.go has already assigned.

	// RejectUpload runs for HEAD /upload as well as the real PUT, so this counts
	// attempts including the check a well behaved client makes before sending
	// anything.
	prevRejectUpload := bl.RejectUpload
	bl.RejectUpload = func(ctx context.Context, auth *nostr.Event, size int, ext string) (bool, string, int) {
		m.c.add(blobUploadAttempts, 1)
		if prevRejectUpload != nil {
			reject, msg, code := prevRejectUpload(ctx, auth, size, ext)
			if reject {
				return reject, msg, code
			}
		}
		m.c.add(blobUploadAllowed, 1)
		return false, "", 0
	}

	// wrapped around haven's own hook, so it only counts once the file is
	// written: the successor calls StoreBlob once and stops on its error
	prevStoreBlob := bl.StoreBlob
	bl.StoreBlob = func(ctx context.Context, sha256 string, ext string, body []byte) error {
		if prevStoreBlob != nil {
			if err := prevStoreBlob(ctx, sha256, ext, body); err != nil {
				return err
			}
		}
		m.c.add(blobStored, 1)
		m.c.add(blobBytes, int64(len(body)))
		return nil
	}

	// LoadBlob no longer stops at the first reader in the successor — the hook
	// is called once — so the counter just wraps it.
	prevLoadBlob := bl.LoadBlob
	bl.LoadBlob = func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, *url.URL, error) {
		m.c.add(blobServed, 1)
		if prevLoadBlob != nil {
			return prevLoadBlob(ctx, sha256, ext)
		}
		return nil, nil, nil
	}

	prevDeleteBlob := bl.DeleteBlob
	bl.DeleteBlob = func(ctx context.Context, sha256 string, ext string) error {
		err := error(nil)
		if prevDeleteBlob != nil {
			err = prevDeleteBlob(ctx, sha256, ext)
		}
		if err == nil {
			m.c.add(blobDeleted, 1)
		}
		return err
	}
}
