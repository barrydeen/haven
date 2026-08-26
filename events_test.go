package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// withTestRelay swaps the package's dbs map for one holding a single temp store
// under the outbox name, and puts the real one back afterwards.
//
// The whole map is replaced rather than one key added because listEvents asks
// eventCounts for a total, and that walks every database in dbs — including the
// four real ones, which a test never calls Init on. The outbox name is used
// because resolveProfiles falls back to outboxDB by name for any other relay.
func withTestRelay(t *testing.T, maxLimit int) DBBackend {
	t.Helper()
	db := newTestDB(t, maxLimit)
	original := dbs
	dbs = map[string]DBBackend{relayOutbox: db}
	eventCounts.invalidate()
	t.Cleanup(func() {
		dbs = original
		eventCounts.invalidate()
	})
	return db
}

func seedNotes(t *testing.T, db DBBackend, n int, base nostr.Timestamp) []string {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	ids := make([]string, 0, n)
	for i := range n {
		evt := &nostr.Event{
			CreatedAt: base + nostr.Timestamp(i),
			Kind:      nostr.KindTextNote,
			Tags:      nostr.Tags{},
			Content:   "note number " + string(rune('a'+i%26)),
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

// TestListEventsPagesWithoutLosingOrRepeating walks a store a page at a time the
// way the browser does and checks the two failures that matter: an event
// returned twice, and an event never returned at all.
func TestListEventsPagesWithoutLosingOrRepeating(t *testing.T) {
	db := withTestRelay(t, 10)
	want := seedNotes(t, db, 47, 1700000000)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > 20 {
			t.Fatal("paging did not terminate")
		}
		res, err := listEvents(context.Background(), relayOutbox, eventListOptions{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatalf("listEvents: %v", err)
		}
		for _, row := range res.Events {
			seen[row.ID]++
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	if len(seen) != len(want) {
		t.Errorf("saw %d distinct events, want %d", len(seen), len(want))
	}
	for _, id := range want {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("event %s never came back", id[:8])
		default:
			t.Errorf("event %s came back %d times", id[:8], seen[id])
		}
	}
}

// TestListEventsReportsTotalAndOrder: newest first is the feed order, and total
// is what tells the owner how much they have not seen.
func TestListEventsReportsTotalAndOrder(t *testing.T) {
	db := withTestRelay(t, 100)
	seedNotes(t, db, 25, 1700000000)

	res, err := listEvents(context.Background(), relayOutbox, eventListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if res.Returned != 5 {
		t.Errorf("returned %d, want 5", res.Returned)
	}
	if res.Total != 25 {
		t.Errorf("total = %d, want 25", res.Total)
	}
	if res.NextCursor == "" {
		t.Error("no cursor was offered despite there being more to read")
	}
	for i := 1; i < len(res.Events); i++ {
		if res.Events[i-1].CreatedAt < res.Events[i].CreatedAt {
			t.Fatalf("events are not newest first at index %d", i)
		}
	}
	if res.Events[0].Class != "regular" {
		t.Errorf("kind 1 classed as %q, want regular", res.Events[0].Class)
	}
	if res.Events[0].Size <= 0 {
		t.Error("event size was not reported")
	}
}

// TestListEventsSearchIsHonest: the substring scan must find matches and must
// report how far it looked, because a bounded search that stops quietly is a
// lie. It must also never set Filter.Search, which would return nothing at all.
func TestListEventsSearchIsHonest(t *testing.T) {
	db := withTestRelay(t, 50)

	sk := nostr.GeneratePrivateKey()
	for i, content := range []string{"find me please", "something else", "FIND ME TOO", "nope"} {
		evt := &nostr.Event{
			CreatedAt: nostr.Timestamp(1700000000 + i),
			Kind:      nostr.KindTextNote,
			Tags:      nostr.Tags{},
			Content:   content,
		}
		if err := evt.Sign(sk); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := db.SaveEvent(context.Background(), evt); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	res, err := listEvents(context.Background(), relayOutbox, eventListOptions{Search: "find me"})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if res.Returned != 2 {
		t.Errorf("returned %d matches, want 2 (the search must be case insensitive)", res.Returned)
	}
	if res.Scanned != 4 {
		t.Errorf("scanned = %d, want 4 — the whole store was searched", res.Scanned)
	}
	if !res.Complete {
		t.Error("complete was false after searching a store small enough to finish")
	}
	for _, row := range res.Events {
		if !strings.Contains(strings.ToLower(row.Content), "find me") {
			t.Errorf("a non matching event came back: %q", row.Content)
		}
	}
}

// TestListEventsFiltersByKind proves the server side filter reaches the query
// rather than being dropped.
func TestListEventsFiltersByKind(t *testing.T) {
	db := withTestRelay(t, 50)
	sk := nostr.GeneratePrivateKey()
	for i, kind := range []int{1, 7, 1, 30023, 7} {
		evt := &nostr.Event{
			CreatedAt: nostr.Timestamp(1700000000 + i),
			Kind:      kind,
			Tags:      nostr.Tags{nostr.Tag{"d", "x"}},
			Content:   "c",
		}
		if err := evt.Sign(sk); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := db.SaveEvent(context.Background(), evt); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	res, err := listEvents(context.Background(), relayOutbox, eventListOptions{Kinds: []int{7}})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if res.Returned != 2 {
		t.Fatalf("returned %d kind 7 events, want 2", res.Returned)
	}
	for _, row := range res.Events {
		if row.Kind != 7 {
			t.Errorf("kind %d came back from a kind 7 filter", row.Kind)
		}
	}

	res, err = listEvents(context.Background(), relayOutbox, eventListOptions{Kinds: []int{30023}})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if res.Returned != 1 || res.Events[0].Class != "addressable" {
		t.Errorf("kind 30023 classed as %q, want addressable", res.Events[0].Class)
	}
}

// TestListEventsTruncatesContentOnARuneBoundary: a naive slice would leave half a
// code point, which encoding/json turns into a replacement character in the
// middle of somebody's note.
func TestListEventsTruncatesContentOnARuneBoundary(t *testing.T) {
	db := withTestRelay(t, 50)
	sk := nostr.GeneratePrivateKey()
	evt := &nostr.Event{
		CreatedAt: 1700000000,
		Kind:      nostr.KindTextNote,
		Tags:      nostr.Tags{},
		// multi byte runes straddling the cut
		Content: strings.Repeat("é", maxRowContent),
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := db.SaveEvent(context.Background(), evt); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := listEvents(context.Background(), relayOutbox, eventListOptions{})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	row := res.Events[0]
	if !row.ContentTruncated {
		t.Fatal("a long note was not reported as truncated")
	}
	if !utf8ValidString(row.Content) {
		t.Error("truncation split a rune")
	}
	if row.ContentSize != len(evt.Content) {
		t.Errorf("content_size = %d, want the untruncated %d", row.ContentSize, len(evt.Content))
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestDeleteEventsRemovesAndReportsHonestly covers the decision the user made:
// delete only, no ban, and the answer has to say the event can come back.
func TestDeleteEventsRemovesAndReportsHonestly(t *testing.T) {
	db := withTestRelay(t, 50)
	ids := seedNotes(t, db, 3, 1700000000)

	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	res, err := deleteEvents(context.Background(), relayOutbox, append(append([]string{}, ids...), missing))
	if err != nil {
		t.Fatalf("deleteEvents: %v", err)
	}
	if res.Removed != 3 {
		t.Errorf("removed = %d, want 3", res.Removed)
	}
	if res.Missing != 1 {
		t.Errorf("missing = %d, want 1", res.Missing)
	}
	if res.Errors != 0 {
		t.Errorf("errors = %d, want 0", res.Errors)
	}
	if res.Permanent != 0 {
		t.Errorf("permanent = %d, want 0 — a plain delete is not a ban", res.Permanent)
	}
	if res.Warning == "" {
		t.Error("no warning that the events can be published again")
	}

	after, err := listEvents(context.Background(), relayOutbox, eventListOptions{})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if after.Returned != 0 {
		t.Errorf("%d events survived the delete", after.Returned)
	}
	if after.Total != 0 {
		t.Errorf("total = %d after deleting everything; the count cache was not invalidated", after.Total)
	}
}

// TestGetEventReturnsTheWholeEvent: the drawer's raw JSON view is only honest if
// it is the real event, signature and all.
func TestGetEventReturnsTheWholeEvent(t *testing.T) {
	db := withTestRelay(t, 50)
	ids := seedNotes(t, db, 1, 1700000000)

	detail, err := getEvent(context.Background(), relayOutbox, ids[0])
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if detail.Event == nil || detail.Event.ID != ids[0] {
		t.Fatal("getEvent returned the wrong event")
	}
	if detail.Event.Sig == "" {
		t.Error("the event came back without its signature")
	}
	if detail.Class != "regular" || detail.Permanent {
		t.Errorf("class = %q permanent = %v, want regular and false", detail.Class, detail.Permanent)
	}

	if _, err := getEvent(context.Background(), relayOutbox, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("getEvent invented an event that was never stored")
	}
}

// TestEventCursorRoundTrip: the cursor is opaque to clients, so a stale or
// forged one has to be refused with a sentence rather than decoded into nonsense.
func TestEventCursorRoundTrip(t *testing.T) {
	in := eventCursor{Until: 1700000000, Seen: []string{"a", "b"}}
	encoded, err := encodeEventCursor(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeEventCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Until != in.Until || len(out.Seen) != 2 {
		t.Errorf("round trip lost data: %+v", out)
	}
	for _, bad := range []string{"", "nonsense", "hv1.!!!!", "hv2.abcd"} {
		if _, err := decodeEventCursor(bad); err == nil {
			t.Errorf("decodeEventCursor accepted %q", bad)
		}
	}
}
