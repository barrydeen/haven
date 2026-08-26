package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/nbd-wtf/go-nostr"
)

// newTestDB builds a real lmdb store in a temp directory with a deliberately
// tiny page limit, so a few dozen events exercise the same multi-page cursor
// walk that a real database only reaches in the tens of thousands.
func newTestDB(t *testing.T, maxLimit int) DBBackend {
	t.Helper()
	db := &lmdb.LMDBBackend{
		Path: t.TempDir(),
		// small, because the default reserves 256GiB of address space and a test
		// has no use for it
		MapSize:  1 << 28,
		MaxLimit: maxLimit,
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init lmdb: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seedEvents stores one signed event per entry of createdAt and returns the ids
// in the order they were written.
func seedEvents(t *testing.T, db DBBackend, createdAt []nostr.Timestamp) []string {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	ids := make([]string, 0, len(createdAt))
	for i, ts := range createdAt {
		evt := &nostr.Event{
			CreatedAt: ts,
			Kind:      nostr.KindTextNote,
			Tags:      nostr.Tags{},
			Content:   string(rune('a'+i%26)) + "-note",
		}
		if err := evt.Sign(sk); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := db.SaveEvent(context.Background(), evt); err != nil {
			t.Fatalf("save: %v", err)
		}
		ids = append(ids, evt.ID)
	}
	return ids
}

// TestEachEventVisitsEveryEventExactlyOnce is the test the whole file exists
// for. Until is inclusive, so every page after the first re-delivers whatever
// sat on the cursor second; a walk that forgets to deduplicate reports events
// twice, and one that steps the cursor back a second to avoid the overlap loses
// them instead. Both failures are silent.
func TestEachEventVisitsEveryEventExactlyOnce(t *testing.T) {
	db := newTestDB(t, 10)

	// spread over distinct seconds, so this is purely about page boundaries
	stamps := make([]nostr.Timestamp, 0, 95)
	for i := range 95 {
		stamps = append(stamps, nostr.Timestamp(1700000000+i))
	}
	want := seedEvents(t, db, stamps)

	counts := map[string]int{}
	scanned, complete, err := eachEvent(context.Background(), db, nostr.Filter{}, 0, func(evt *nostr.Event) bool {
		counts[evt.ID]++
		return true
	})
	if err != nil {
		t.Fatalf("eachEvent: %v", err)
	}
	if !complete {
		t.Error("walk reported incomplete over a range it walked to the end")
	}
	if scanned != len(want) {
		t.Errorf("scanned = %d, want %d", scanned, len(want))
	}
	for _, id := range want {
		switch counts[id] {
		case 1:
		case 0:
			t.Errorf("event %s was never visited", id[:8])
		default:
			t.Errorf("event %s was visited %d times", id[:8], counts[id])
		}
	}
}

// TestEachEventHandlesACrowdedSecond covers the case the walker warns about:
// more events sharing one second than a page can hold. created_at is the finest
// cursor there is, so the walk has to step over that second to make progress and
// some events are knowingly lost — but it must terminate, and it must not loop
// forever refetching the same page.
func TestEachEventHandlesACrowdedSecond(t *testing.T) {
	db := newTestDB(t, 4)

	stamps := make([]nostr.Timestamp, 0, 30)
	for range 20 {
		stamps = append(stamps, nostr.Timestamp(1700000500)) // all one second
	}
	for i := range 10 {
		stamps = append(stamps, nostr.Timestamp(1700000600+i))
	}
	seedEvents(t, db, stamps)

	seen := map[string]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, err := eachEvent(context.Background(), db, nostr.Filter{}, 0, func(evt *nostr.Event) bool {
			seen[evt.ID]++
			return true
		})
		if err != nil {
			t.Errorf("eachEvent: %v", err)
		}
	}()

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("eachEvent did not terminate on a crowded second")
	}

	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s visited %d times", id[:8], n)
		}
	}
	// the ten uncrowded events are above the crowded second and must all survive
	if len(seen) < 10 {
		t.Errorf("only %d distinct events visited; the events above the crowded second should all be there", len(seen))
	}
}

// TestEachEventStopsWhenVisitSaysSo is how a page fills up without scanning the
// rest of the database, so "stopped early" must not be reported as "reached the
// end".
func TestEachEventStopsWhenVisitSaysSo(t *testing.T) {
	db := newTestDB(t, 10)
	stamps := make([]nostr.Timestamp, 0, 40)
	for i := range 40 {
		stamps = append(stamps, nostr.Timestamp(1700000000+i))
	}
	seedEvents(t, db, stamps)

	n := 0
	scanned, complete, err := eachEvent(context.Background(), db, nostr.Filter{}, 0, func(*nostr.Event) bool {
		n++
		return n < 7
	})
	if err != nil {
		t.Fatalf("eachEvent: %v", err)
	}
	if complete {
		t.Error("complete was true after visit asked to stop")
	}
	if scanned != 7 || n != 7 {
		t.Errorf("scanned = %d, visits = %d, want 7 and 7", scanned, n)
	}
}

// TestEachEventRespectsTheBudget: the budget is the only thing bounding a
// content search, so running out of it must be reported as not-complete rather
// than as an honest end of range.
func TestEachEventRespectsTheBudget(t *testing.T) {
	db := newTestDB(t, 10)
	stamps := make([]nostr.Timestamp, 0, 40)
	for i := range 40 {
		stamps = append(stamps, nostr.Timestamp(1700000000+i))
	}
	seedEvents(t, db, stamps)

	scanned, complete, err := eachEvent(context.Background(), db, nostr.Filter{}, 12, func(*nostr.Event) bool {
		return true
	})
	if err != nil {
		t.Fatalf("eachEvent: %v", err)
	}
	if complete {
		t.Error("complete was true after the budget ran out")
	}
	if scanned != 12 {
		t.Errorf("scanned = %d, want the budget of 12", scanned)
	}
}

// TestEachEventRefusesToSearch guards the trap that motivated errStoreCannotSearch:
// the backends answer a filter carrying Search by closing the channel with
// nothing in it, so passing one through would report an empty result as a real
// one.
func TestEachEventRefusesToSearch(t *testing.T) {
	db := newTestDB(t, 10)
	seedEvents(t, db, []nostr.Timestamp{1700000000})

	_, _, err := eachEvent(context.Background(), db, nostr.Filter{Search: "anything"}, 0, func(*nostr.Event) bool {
		t.Error("visit ran for a filter carrying a search string")
		return true
	})
	if !errors.Is(err, errStoreCannotSearch) {
		t.Errorf("err = %v, want errStoreCannotSearch", err)
	}
}

// TestClampFilterTimestamps: both query planners narrow these to uint32, so a
// bound outside that range wraps and the query comes back empty with no error.
// An out of range bound has to behave like no bound.
func TestClampFilterTimestamps(t *testing.T) {
	tooBig := nostr.Timestamp(99999999999)
	negative := nostr.Timestamp(-1)
	fine := nostr.Timestamp(1700000000)

	f := nostr.Filter{Until: &tooBig, Since: &negative}
	clampFilterTimestamps(&f)
	if f.Until != nil {
		t.Errorf("Until = %v, want nil for an out of range bound", *f.Until)
	}
	if f.Since != nil {
		t.Errorf("Since = %v, want nil for a negative bound", *f.Since)
	}

	f = nostr.Filter{Until: &fine}
	clampFilterTimestamps(&f)
	if f.Until == nil || *f.Until != fine {
		t.Error("a bound inside the range was cleared")
	}
}

// TestFindStoredEventsReturnsWholeEvents: a delete needs the event the store
// holds, because both backends derive the index keys to remove from its own
// fields. It must also not invent entries for ids that were not there.
func TestFindStoredEventsReturnsWholeEvents(t *testing.T) {
	db := newTestDB(t, 10)
	ids := seedEvents(t, db, []nostr.Timestamp{1700000001, 1700000002, 1700000003})

	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	found, err := findStoredEvents(context.Background(), db, append(append([]string{}, ids...), missing))
	if err != nil {
		t.Fatalf("findStoredEvents: %v", err)
	}
	if len(found) != len(ids) {
		t.Errorf("found %d events, want %d", len(found), len(ids))
	}
	for _, id := range ids {
		evt, ok := found[id]
		if !ok {
			t.Errorf("event %s was not found", id[:8])
			continue
		}
		if evt.ID != id {
			t.Errorf("event keyed %s carries id %s", id[:8], evt.ID[:8])
		}
		if evt.Sig == "" || evt.PubKey == "" {
			t.Errorf("event %s came back without its signature or author", id[:8])
		}
	}
	if _, ok := found[missing]; ok {
		t.Error("an id that was never stored came back")
	}
}

// TestCountStoredEventsTerminatesOnEveryBackend is a regression test for an
// upstream bug, not for haven's own logic.
//
// eventstore v0.17.5's lmdb CountEvents advances its cursor only on the paths
// where an event is rejected by an extra author, kind or tag check; both paths
// that increment the counter fall through to the top of the loop without calling
// it.next(). So a filter that matches nothing returns zero correctly and a filter
// that matches anything never returns at all — on a goroutine that never checks
// the context, holding whatever lock the caller took.
//
// DB_ENGINE defaults to lmdb and the stats method counts every database with an
// empty filter, so this is the difference between a working relay console and one
// that hangs on first use. If this test ever starts failing because the walk is
// no longer needed, check the upstream fix before deleting countStoredEvents.
func TestCountStoredEventsTerminatesOnEveryBackend(t *testing.T) {
	db := newTestDB(t, 10)
	seedEvents(t, db, []nostr.Timestamp{1700000001, 1700000002, 1700000003})

	done := make(chan struct{})
	var (
		got   int64
		exact bool
		err   error
	)
	go func() {
		defer close(done)
		got, exact, err = countStoredEvents(context.Background(), db, nostr.Filter{}, maxAggregateScan)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("countStoredEvents did not return; the CountEvents workaround is not in place")
	}

	if err != nil {
		t.Fatalf("countStoredEvents: %v", err)
	}
	if got != 3 {
		t.Errorf("counted %d, want 3", got)
	}
	if !exact {
		t.Error("a three event store was reported as an inexact count")
	}
}

// TestBackendCountsSafelyKnowsAboutLmdb pins the decision itself, so flipping it
// by accident fails here rather than by hanging a production relay.
func TestBackendCountsSafelyKnowsAboutLmdb(t *testing.T) {
	if backendCountsSafely(newTestDB(t, 10)) {
		t.Error("lmdb was reported as safe to count with; its CountEvents does not terminate")
	}
}
