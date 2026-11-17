package main

import (
	"context"
	"errors"
	"math"

	"fiatjaf.com/nostr"
)

//
// walking an event store
//
// The successor eventstore removed most of the traps this file used to work
// around, so the walk is now a single pass rather than a paged cursor:
//
//   - QueryEvents takes an explicit maxLimit and honours it as a hard cap;
//     the quarter-limit behaviour that made page sizes a guess is gone.
//   - The query is a pull iterator: stopping early — by returning false from
//     the yield — releases the backend's read transaction cleanly, and a
//     cancelled context does the same. Nothing strands a goroutine any more,
//     which is why the old drain-every-page machinery is gone too.
//   - CountEvents advances its cursor correctly in both branches, so it can
//     be trusted on every backend (backendCountsSafely documented the old
//     lmdb spin-forever bug; that bug no longer exists).
//
// What remains true: a Search filter is not supported by the stores (they
// return nothing rather than searching), and the timestamp wrap in the index
// keys is still worth clamping defensively.

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
// click sitting on the databases. Two at a time leaves the relay's own traffic
// somewhere to run.
var eventScanGate = make(chan struct{}, 2)

// errStoreCannotSearch is what a filter carrying a Search string earns. It is an
// error rather than a silent strip because the failure it prevents is invisible:
// the store answers such a filter with an empty sequence and no error, and
// CountEvents does not look at the field either, so the result would be an
// empty page reported beside a total in the thousands.
var errStoreCannotSearch = errors.New("this event store cannot search; the caller has to scan for itself")

// clampFilterTimestamps keeps the cursor bounds inside the range the index
// keys can represent (32-bit seconds). A timestamp past 2106 or a negative
// one wraps and the query comes back empty with nothing to say why. Clamping
// here makes an out-of-range bound behave like no bound; 0 is the successor
// filter's "no bound".
func clampFilterTimestamps(filter *nostr.Filter) {
	const maxTimestamp = int64(math.MaxUint32)
	if v := int64(filter.Since); v < 0 || v > maxTimestamp {
		filter.Since = 0
	}
	if v := int64(filter.Until); v < 0 || v > maxTimestamp {
		filter.Until = 0
	}
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
// The walk is one query: the successor store honours the budget as a hard cap,
// so the old page-and-cursor loop is gone. budget also IS the maxLimit handed
// to the backend, so a walk can never exceed what the caller asked for.
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

	if budget <= 0 || budget > maxAggregateScan {
		budget = maxAggregateScan
	}

	complete = true
	var scanErr error
	db.QueryEventsIter(ctx, filter, budget)(func(evt nostr.Event) bool {
		if err := ctx.Err(); err != nil {
			complete = false
			scanErr = err
			return false
		}
		e := evt
		scanned++
		if !visit(&e) {
			complete = false
			return false
		}
		return true
	})
	// the budget is the store's hard cap, so a walk that filled it cannot prove
	// it reached the end of the range — report incomplete, the conservative
	// direction every caller treats as "do not delete anything"
	if budget > 0 && scanned >= budget {
		complete = false
	}
	return scanned, complete, scanErr
}

// findStoredEvents reads events back out of one database by id, keyed by id and
// missing whatever was not there.
//
// It is one query rather than one per id: the planner builds a separate id-index
// lookup per id inside a single read transaction.
//
// The id on each event is checked against what was asked for. The id index is
// keyed on the first eight bytes and neither backend verifies the rest, so a
// prefix collision would hand back the wrong event — which is not something that
// happens, but this is the read a delete is built on and a string compare is a
// small price for never deleting the wrong note.
//
// The whole event is returned rather than its id because the backends work out
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

	filter := nostr.Filter{IDs: make([]nostr.ID, 0, len(ids))}
	for _, id := range ids {
		parsed, err := nostr.IDFromHex(id)
		if err != nil {
			return nil, err
		}
		filter.IDs = append(filter.IDs, parsed)
	}

	if _, _, err := eachEvent(ctx, db, filter, len(ids), func(evt *nostr.Event) bool {
		if _, ok := wanted[evt.ID.Hex()]; ok {
			found[evt.ID.Hex()] = evt
		}
		return true
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// countStoredEvents counts the events matching a filter, and says whether the
// number is exact.
//
// The old backend could not count (eventstore v0.17.5's lmdb CountEvents never
// advanced its cursor once it found a match, so it spun forever); the successor
// count advances in both branches, so it is used everywhere and the walk
// fallback is kept only for an unexpected error.
func countStoredEvents(ctx context.Context, db DBBackend, filter nostr.Filter, budget int) (int64, bool, error) {
	n, err := db.CountEvents(ctx, filter)
	if err != nil {
		// walking is slower, bounded by the budget, and it terminates — the
		// one case where a count that failed is worth redoing the hard way
		var walked int64
		_, complete, walkErr := eachEvent(ctx, db, filter, budget, func(*nostr.Event) bool {
			walked++
			return true
		})
		if walkErr != nil {
			return 0, false, err
		}
		return walked, complete, nil
	}
	return n, true, nil
}
