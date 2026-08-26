package main

import (
	"context"
	"errors"
	"log/slog"
	"math"

	"github.com/nbd-wtf/go-nostr"
)

//
// walking an event store
//
// Every trap this file works around is a property of the eventstore backends
// rather than of nostr, and each one fails silently rather than loudly, which is
// why they are written down here instead of being rediscovered:
//
//   1. A filter with Limit 0, or above the backend's MaxLimit, is not clamped to
//      the cap — it is dropped to a quarter of it. queryPageSize is the answer.
//   2. Until is inclusive, so consecutive pages overlap at the cursor second and
//      the overlap has to be deduplicated by event id.
//   3. The query channels are unbuffered and the backends never look at the
//      context, so an abandoned channel strands a goroutine and, on lmdb, an
//      open read transaction. Every page is drained to the end.
//   4. A short page does not mean the end of the walk: a lowered MaxLimit makes
//      every page short.
//   5. Since and Until are narrowed to uint32 by both query planners, so a bound
//      outside that range wraps and the query comes back empty with no error.
//   6. Setting Search does not perform a search and does not report that it
//      cannot — it closes the channel and returns nothing.
//

const (
	// maxEventScan bounds how many events one interactive call may examine.
	// Without a search string a listing examines roughly one event per row it
	// returns and this never bites; with one it is the only bound there is.
	// Because the cursor records where the scan stopped rather than where the
	// last match was, a search that runs out of budget resumes from there
	// instead of starting again.
	maxEventScan = 20000

	// maxAggregateScan bounds a whole-database walk — events by kind, top
	// authors, bytes stored. It is far larger than maxEventScan because it runs
	// on a timer rather than on a click, and because an aggregate that stops at
	// twenty thousand events is not an aggregate. Past this the caller reports
	// an incomplete result with the count it did manage, the way
	// buildBlobInventory already does.
	maxAggregateScan = 500000
)

// eventScanGate caps how many walks run at once.
//
// Every caller here is the authenticated owner, but NIP-86 requests are answered
// in dynamicRelayHandler before khatru's rate limiters ever see them, so nothing
// else stands between a browser tab retrying in a loop and one goroutine per
// click sitting on the databases — and because the backends run their queries on
// goroutines that never check the context, those do not go away when the
// requests do. Two at a time leaves the relay's own traffic somewhere to run.
var eventScanGate = make(chan struct{}, 2)

// errStoreCannotSearch is what a filter carrying a Search string earns. It is an
// error rather than a silent strip because the failure it prevents is invisible:
// both backends answer such a filter by closing the channel with nothing in it,
// and neither of their CountEvents implementations looks at the field at all, so
// the result would be an empty page reported beside a total in the thousands.
var errStoreCannotSearch = errors.New("this event store cannot search; the caller has to scan for itself")

// clampFilterTimestamps keeps the cursor bounds inside the range the backends
// can represent. Both query planners narrow to uint32, so a timestamp past 2106
// wraps to a small number and a negative one wraps to a huge one — and either
// way the query comes back empty with nothing to say why. Clamping here makes an
// out-of-range bound behave like no bound.
func clampFilterTimestamps(filter *nostr.Filter) {
	const maxTimestamp = int64(math.MaxUint32)
	if filter.Since != nil {
		if v := int64(*filter.Since); v < 0 || v > maxTimestamp {
			filter.Since = nil
		}
	}
	if filter.Until != nil {
		if v := int64(*filter.Until); v < 0 || v > maxTimestamp {
			filter.Until = nil
		}
	}
}

// drainEventPage runs one query and reads its channel to the end. Both backends
// answer from a goroutine that writes to an unbuffered channel and never
// consults the context, so a caller that walks away early strands that goroutine
// — and on lmdb strands an open read transaction with it. The channel is drained
// even when the caller has already given up.
func drainEventPage(ctx context.Context, db DBBackend, filter nostr.Filter) ([]*nostr.Event, error) {
	events, err := db.QueryEvents(ctx, filter)
	if err != nil {
		return nil, err
	}
	var page []*nostr.Event
	for evt := range events {
		page = append(page, evt)
	}
	return page, nil
}

// eachEvent walks every event matching filter, newest first, hands each one to
// visit exactly once, and reports how many distinct events it saw and whether it
// reached the end of the range.
//
// visit returns false to stop the walk; that is how a page fills up without
// scanning the rest of the database. complete is true only when the range was
// walked to its end, so a budget that ran out, a context that was cancelled and
// a visit that asked to stop all report false and the caller has to say which.
//
// The page size comes from the backend rather than from a constant, and
// filter.Limit as the caller set it is ignored: paging is this function's
// business, and a caller's guess at a page size is the trap described at the top
// of this file.
//
// The set of seen ids is deliberately not the whole walk. Until is inclusive, so
// the only events that can arrive twice are the ones sitting exactly on the
// cursor second — everything above it is outside the next query's range. Keeping
// only those bounds this by the busiest single second in the database instead of
// by the database: remembering every id of a two million event walk would be a
// quarter of a gigabyte held on the owner's request.
func eachEvent(
	ctx context.Context,
	db DBBackend,
	filter nostr.Filter,
	budget int,
	visit func(*nostr.Event) bool,
) (scanned int, complete bool, err error) {
	if filter.Search != "" {
		return 0, false, errStoreCannotSearch
	}

	select {
	case eventScanGate <- struct{}{}:
		defer func() { <-eventScanGate }()
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}

	clampFilterTimestamps(&filter)

	// An id query is not paged and cannot be truncated: the planners ignore the
	// cursor bounds entirely when IDs is set, so a cursor walk would loop
	// forever refetching the same page, and nostr.GetTheoreticalLimit pins the
	// limit to len(IDs) in place of MaxLimit.
	if len(filter.IDs) > 0 {
		page, err := drainEventPage(ctx, db, filter)
		if err != nil {
			return 0, false, err
		}
		for _, evt := range page {
			scanned++
			if !visit(evt) {
				return scanned, false, nil
			}
		}
		return scanned, true, nil
	}

	filter.Limit = queryPageSize(db)

	// ids seen at exactly the cursor second, which is the only place a duplicate
	// can come from
	seen := make(map[string]struct{})

	// The page size asked for is not necessarily the one served: a backend whose
	// MaxLimit is lower quietly serves a quarter of its own cap instead. The
	// largest page seen so far is therefore the only honest measure of whether a
	// page was cut short.
	widest := 0

	for {
		// the backends never check the context themselves, so this is the only
		// cancellation there is: a caller that hung up costs one more page
		if err := ctx.Err(); err != nil {
			return scanned, false, err
		}

		page, err := drainEventPage(ctx, db, filter)
		if err != nil {
			return scanned, false, err
		}
		if len(page) == 0 {
			return scanned, true, nil
		}
		if len(page) > widest {
			widest = len(page)
		}

		// computed rather than taken from the last element: nothing here should
		// depend on the order the backend happened to return
		oldest := page[0].CreatedAt
		for _, evt := range page {
			if evt.CreatedAt < oldest {
				oldest = evt.CreatedAt
			}
		}

		fresh := 0
		// rebuilt as the page is walked so it carries only the cursor second
		next := make(map[string]struct{})
		for _, evt := range page {
			if evt.CreatedAt == oldest {
				next[evt.ID] = struct{}{}
			}
			if _, dupe := seen[evt.ID]; dupe {
				continue
			}
			fresh++
			scanned++
			if !visit(evt) {
				return scanned, false, nil
			}
			if budget > 0 && scanned >= budget {
				return scanned, false, nil
			}
		}

		if oldest <= 0 {
			return scanned, true, nil
		}

		if fresh == 0 {
			// Until is inclusive, so the last page of any walk comes back full of
			// events already seen. When that page was not cut short it is also
			// proof there is nothing older: the query asked for everything at or
			// below the cursor and did not fill a page, so this is the end.
			if len(page) < widest {
				return scanned, true, nil
			}

			// A full page of nothing new is the other story: more events share
			// this one second than a page can hold, and created_at is the finest
			// cursor there is. The only way to make progress is to step over that
			// second and lose whatever else was in it.
			slog.Warn("⚠️ more events share one second than a page holds, so some are being skipped",
				"until", int64(oldest), "page", len(page))
			oldest--
			if oldest <= 0 {
				return scanned, true, nil
			}
			// the cursor moved past the crowded second, so nothing carries over
			next = make(map[string]struct{})
		}

		seen = next

		// terminating on a short page would be wrong: a lowered MaxLimit makes
		// every page short, and this loop would stop after the first one
		until := oldest
		filter.Until = &until
	}
}

// findStoredEvents reads events back out of one database by id, keyed by id and
// missing whatever was not there.
//
// It is one query rather than one per id: the planner builds a separate id-index
// lookup per id inside a single read transaction, and nostr.GetTheoreticalLimit
// pins the limit to len(ids), so MaxLimit cannot cut this short the way it cuts
// every other query short.
//
// The id on each event is checked against what was asked for. The id index is
// keyed on the first eight bytes and neither backend verifies the rest, so a
// prefix collision would hand back the wrong event — which is not something that
// happens, but this is the read a delete is built on and a string compare is a
// small price for never deleting the wrong note.
//
// The whole event is returned rather than its id because both backends work out
// which index keys to remove from the event's own fields: deleting a stand-in
// carrying only the id would leave the indexes inconsistent.
func findStoredEvents(ctx context.Context, db DBBackend, ids []string) (map[string]*nostr.Event, error) {
	found := make(map[string]*nostr.Event, len(ids))
	if len(ids) == 0 {
		return found, nil
	}

	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}

	// Limit is set for the same reason it is set everywhere else in this file,
	// even though GetTheoreticalLimit makes it moot for an id query: a filter
	// leaving it zero is the trap, and not writing the trap is cheaper than
	// remembering which queries are exempt from it.
	_, _, err := eachEvent(ctx, db, nostr.Filter{IDs: ids, Limit: len(ids)}, 0, func(evt *nostr.Event) bool {
		if _, ok := wanted[evt.ID]; ok {
			found[evt.ID] = evt
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// countStoredEvents counts the events matching a filter, and says whether the
// number is exact.
//
// It exists because one of the two backends cannot count. See
// backendCountsSafely: eventstore v0.17.5's lmdb CountEvents never advances its
// cursor once it finds a match, so it spins forever on a goroutine that ignores
// the context. Calling it on an lmdb store — which is what DB_ENGINE defaults to
// — hangs the caller permanently and burns a core doing it.
//
// Where CountEvents is safe it is used, because counting index keys is far
// cheaper than decoding every event. Where it is not, this walks with eachEvent
// instead: slower, bounded by the budget, and it terminates. exact is false when
// the walk ran out of budget, so a caller can say "at least N" rather than
// reporting a floor as a total.
func countStoredEvents(ctx context.Context, db DBBackend, filter nostr.Filter, budget int) (int64, bool, error) {
	if backendCountsSafely(db) {
		n, err := db.CountEvents(ctx, filter)
		return n, err == nil, err
	}

	var n int64
	_, complete, err := eachEvent(ctx, db, filter, budget, func(*nostr.Event) bool {
		n++
		return true
	})
	if err != nil {
		return 0, false, err
	}
	return n, complete, nil
}
