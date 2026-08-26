package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nbd-wtf/go-nostr"
)

//
// browsing the events a relay has stored
//
// This is what the admin page's Notes view reads. It is deliberately built on
// the same honesty contract as the media browser: a caller is told how much was
// looked at, how much was returned, and which of the several different reasons
// an answer might be short actually applied.
//

const (
	// defaultEventsPerPage and maxEventsPerPage size one answer. NIP-86 has no
	// batching, so one page is one signer prompt: too small and scrolling the
	// feed is a prompt storm, too large and the owner waits for rows they will
	// never scroll to. Two hundred is roughly four screens.
	defaultEventsPerPage = 200
	maxEventsPerPage     = 1000

	// maxRowContent truncates content in the list. One kind 30023 long form post
	// is tens of kilobytes and a page of them would be megabytes decided by a
	// handful of rows; a kilobyte carries the whole of nearly every short note
	// and a readable opening of everything else, and content_truncated tells the
	// UI when the drawer is worth a second prompt.
	maxRowContent = 1024

	// maxRowTags truncates tags in the list. A kind 3 follow list carries
	// thousands of p tags and none of them are what a feed row is showing.
	maxRowTags = 64

	// maxEventsPerDelete caps a bulk delete for the same reason maxBlobsPerCall
	// does: maxManagementBody caps the request body at 64 KiB and a hex id costs
	// about 68 bytes of JSON, so a longer list comes back as a bare 413 with
	// nothing in it to read.
	maxEventsPerDelete = 500

	// filter breadth caps. These are not about query cost — the planners handle
	// a long author list well — but about that same 64 KiB body.
	maxFilterKinds   = 64
	maxFilterAuthors = 100
	maxFilterIDs     = 100
	maxSearchRunes   = 256

	// maxProfileLookups bounds the extra kind 0 query a page makes for its
	// authors' names. A page of 200 rows written by 200 different people is
	// already the worst case.
	maxProfileLookups = 200
)

//
// the cursor
//

// eventCursorPrefix marks a cursor as one of ours.
//
// The cursor exists because Until is inclusive: resuming a walk needs the ids
// already returned at the second the last page ended on, not just the second
// itself. That is a workaround for a backend trap rather than a protocol, so it
// is handed out opaque rather than as fields a client could assemble by hand and
// get subtly wrong, and the prefix means a cursor minted by an older haven is
// refused with a sentence instead of decoded into nonsense.
const eventCursorPrefix = "hv1."

// maxCursorSeen caps the ids carried across a page boundary. Two hundred events
// sharing one second is already unusual outside a restore; past that the cursor
// steps over the second, which loses whatever else was in it, and the answer
// says so. Full ids rather than prefixes: a prefix collision here would skip a
// real note.
const maxCursorSeen = 200

type eventCursor struct {
	Until int64    `json:"u,omitempty"`
	Seen  []string `json:"d,omitempty"`
}

func encodeEventCursor(c eventCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return eventCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeEventCursor(s string) (eventCursor, error) {
	var c eventCursor
	rest, ok := strings.CutPrefix(s, eventCursorPrefix)
	if !ok {
		return c, fmt.Errorf("that cursor was not issued by this relay")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return c, fmt.Errorf("that cursor is not readable")
	}
	if len(raw) > 64<<10 {
		return c, fmt.Errorf("that cursor is too large")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("that cursor is not readable")
	}
	return c, nil
}

//
// shapes
//

// eventListOptions is the decoded form of listevents' single object parameter.
type eventListOptions struct {
	Kinds   []int
	Authors []string
	IDs     []string
	Since   *nostr.Timestamp
	Until   *nostr.Timestamp
	Search  string
	Limit   int
	Cursor  string
}

// eventRow is one event as the feed sees it: the event's own fields plus what
// the UI would otherwise have to recompute, and nothing it can work out for
// itself.
//
// Content and tags are capped rather than complete, and the row says when they
// were. The alternative — trimmed rows plus a getevent call for every drawer —
// would spend a signer prompt on every note the owner so much as opens, which is
// the failure the media browser exists to avoid. Carrying the content means an
// ordinary note is complete in the list and its drawer opens for free; only a
// long form post or a follow list costs the second call.
type eventRow struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Class     string     `json:"class"`
	Content   string     `json:"content"`
	Tags      nostr.Tags `json:"tags"`
	Size      int        `json:"size"`
	// ContentSize is the untruncated length in bytes, so the UI can say how much
	// it is not showing without asking for it.
	ContentSize      int  `json:"content_size"`
	ContentTruncated bool `json:"content_truncated,omitempty"`
	TagCount         int  `json:"tag_count"`
	TagsTruncated    bool `json:"tags_truncated,omitempty"`
	// Banned is a map lookup, not a query. It is normally false, because
	// banevent deletes what it bans — but a JSONL restore writes straight to the
	// store with no ban check, so a true here means a restore brought back
	// something the owner had already thrown away, which is worth seeing.
	Banned bool `json:"banned,omitempty"`
}

// eventListResult follows blobListResult's shape on purpose: total against
// returned, an explicit truncated with a cursor saying where to resume, a
// counted_at on the cached figure, and a complete that means something different
// from truncated and is reported separately.
//
// Three different things can shorten this answer and they are three fields.
// Truncated: the page filled up, NextCursor resumes it. Complete false: the
// filter's range was not walked to its end because the scan budget ran out —
// Scanned against ScanBudget says by how much, and NextCursor resumes the scan
// from where it stopped rather than from the top. Warning: something was skipped
// outright and cannot be resumed.
type eventListResult struct {
	Relay      string                     `json:"relay"`
	Events     []eventRow                 `json:"events"`
	Profiles   map[string]json.RawMessage `json:"profiles"`
	Returned   int                        `json:"returned"`
	Total      int64                      `json:"total"`
	Scanned    int                        `json:"scanned"`
	ScanBudget int                        `json:"scan_budget"`
	Truncated  bool                       `json:"truncated"`
	Complete   bool                       `json:"complete"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	CountedAt  int64                      `json:"counted_at"`
	Warning    string                     `json:"warning,omitempty"`
}

// eventDetail is one whole event, untruncated, plus what the relay knows about
// where it stands.
type eventDetail struct {
	Relay     string       `json:"relay"`
	Event     *nostr.Event `json:"event"`
	Size      int          `json:"size"`
	Class     string       `json:"class"`
	Deleted   bool         `json:"deleted"`
	Banned    bool         `json:"banned"`
	BanReason string       `json:"ban_reason,omitempty"`
	Address   string       `json:"address,omitempty"`
	Permanent bool         `json:"permanent"`
}

// eventDeleteResult is one id's outcome. Deleted false with no error is not a
// failure: the owner's page may be a minute old, and an event its author already
// deleted through NIP-09 is simply gone.
type eventDeleteResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	Kind    int    `json:"kind,omitempty"`
	PubKey  string `json:"pubkey,omitempty"`
	Class   string `json:"class,omitempty"`
	// Permanent is whether anything stops this coming back. Deleting a stored
	// copy is not a NIP-09 request — haven holds no private key to sign one with
	// — so the usual answer is no: a whitelisted author, an import or a restore
	// will all put it straight back. It is true only when the id is on this
	// relay's banned list, or when the relay already holds a delete request
	// covering it, which is what MustNotBeDeleted reads.
	Permanent bool   `json:"permanent"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

// eventBulkResult is what a delete answers with, single or bulk. The per id rows
// are what lets the browser report honestly — a plain true could not say whether
// an event was already gone — and the totals save it counting them.
type eventBulkResult struct {
	Relay     string              `json:"relay"`
	Deleted   []eventDeleteResult `json:"deleted"`
	Removed   int                 `json:"removed"`
	Missing   int                 `json:"missing"`
	Errors    int                 `json:"errors"`
	Permanent int                 `json:"permanent"`
	Warning   string              `json:"warning,omitempty"`
}

//
// helpers
//

// eventClass names what NIP-01 says happens to an event of this kind, which is
// what the UI needs to explain a delete correctly: a replaceable event the owner
// removes comes back the moment its author publishes a newer version.
func eventClass(kind int) string {
	switch {
	case nostr.IsEphemeralKind(kind):
		return "ephemeral"
	case nostr.IsAddressableKind(kind):
		return "addressable"
	case nostr.IsReplaceableKind(kind):
		return "replaceable"
	default:
		return "regular"
	}
}

// truncateRunes cuts a string to at most n bytes without splitting a rune. A
// plain slice would leave a partial code point, which encoding/json turns into a
// replacement character in the middle of somebody's note.
func truncateRunes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func rowFor(relay string, evt *nostr.Event) eventRow {
	content, contentCut := truncateRunes(evt.Content, maxRowContent)

	tags := evt.Tags
	tagsCut := false
	if len(tags) > maxRowTags {
		tags = tags[:maxRowTags]
		tagsCut = true
	}

	return eventRow{
		ID:               evt.ID,
		PubKey:           evt.PubKey,
		CreatedAt:        int64(evt.CreatedAt),
		Kind:             evt.Kind,
		Class:            eventClass(evt.Kind),
		Content:          content,
		Tags:             tags,
		Size:             len(evt.String()),
		ContentSize:      len(evt.Content),
		ContentTruncated: contentCut,
		TagCount:         len(evt.Tags),
		TagsTruncated:    tagsCut,
		Banned:           isBannedEvent(relay, evt.ID),
	}
}

// resolveProfiles looks up the kind 0 metadata for a page's authors so the feed
// can show names instead of npubs.
//
// This is done here rather than in the browser because the admin page's CSP is
// connect-src 'self': it cannot fetch a profile from anywhere, and a second
// NIP-86 call would cost a second signature. One extra local query per page
// costs nothing and keeps the whole page to one prompt.
//
// Only what this relay already stores is used. An author with no kind 0 here is
// simply absent from the map and the UI falls back to a shortened npub — no
// attempt is made to go and find one, which would leak the owner's interest in a
// pubkey to whatever relay was asked.
func resolveProfiles(ctx context.Context, relay string, rows []eventRow) map[string]json.RawMessage {
	profiles := make(map[string]json.RawMessage)
	if len(rows) == 0 {
		return profiles
	}

	seen := make(map[string]struct{}, len(rows))
	authors := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PubKey]; ok {
			continue
		}
		seen[row.PubKey] = struct{}{}
		authors = append(authors, row.PubKey)
		if len(authors) >= maxProfileLookups {
			break
		}
	}

	// the relay being browsed first, then the outbox: a chat or inbox relay
	// rarely holds anybody's metadata, but the public outbox usually holds the
	// owner's and whoever they have imported
	sources := []DBBackend{}
	if db, ok := dbs[relay]; ok {
		sources = append(sources, db)
	}
	if relay != relayOutbox {
		sources = append(sources, outboxDB)
	}

	newest := make(map[string]nostr.Timestamp, len(authors))
	for _, db := range sources {
		filter := nostr.Filter{
			Kinds:   []int{nostr.KindProfileMetadata},
			Authors: authors,
			Limit:   len(authors),
		}
		_, _, err := eachEvent(ctx, db, filter, maxProfileLookups*4, func(evt *nostr.Event) bool {
			if at, ok := newest[evt.PubKey]; ok && at >= evt.CreatedAt {
				return true
			}
			newest[evt.PubKey] = evt.CreatedAt
			profiles[evt.PubKey] = json.RawMessage(evt.Content)
			return true
		})
		if err != nil {
			// a page without names is a page; this is not worth failing over
			return profiles
		}
	}
	return profiles
}

//
// the methods
//

// listEvents answers one page of one relay's store, newest first.
func listEvents(ctx context.Context, relay string, opts eventListOptions) (eventListResult, error) {
	db, ok := dbs[relay]
	if !ok {
		return eventListResult{}, fmt.Errorf("unknown relay %q", relay)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultEventsPerPage
	}
	if limit > maxEventsPerPage {
		limit = maxEventsPerPage
	}

	filter := nostr.Filter{}
	// built as nil rather than as empty slices on purpose: the query planners
	// branch on IDs != nil, so an empty but non-nil slice produces no queries at
	// all and an empty result that looks like an honest one
	if len(opts.Kinds) > 0 {
		filter.Kinds = opts.Kinds
	}
	if len(opts.Authors) > 0 {
		filter.Authors = opts.Authors
	}
	if len(opts.IDs) > 0 {
		filter.IDs = opts.IDs
	}
	filter.Since = opts.Since
	filter.Until = opts.Until

	result := eventListResult{Relay: relay, Events: []eventRow{}}

	skip := map[string]struct{}{}
	if opts.Cursor != "" {
		cursor, err := decodeEventCursor(opts.Cursor)
		if err != nil {
			return eventListResult{}, err
		}
		until := nostr.Timestamp(cursor.Until)
		filter.Until = &until
		for _, id := range cursor.Seen {
			skip[id] = struct{}{}
		}
	}

	needle := strings.ToLower(opts.Search)
	budget := maxEventScan

	var (
		tailAt  int64
		tailIDs []string
		crowded bool
	)

	scanned, complete, err := eachEvent(ctx, db, filter, budget, func(evt *nostr.Event) bool {
		at := int64(evt.CreatedAt)
		// the tail is where the *scan* stopped, not where the last match was:
		// a search that ran out of budget has to resume from the events it has
		// not looked at yet, or it would re-scan the same gap forever
		if tailAt == 0 || at < tailAt {
			tailAt, tailIDs = at, tailIDs[:0]
		}
		if at == tailAt {
			if len(tailIDs) < maxCursorSeen {
				tailIDs = append(tailIDs, evt.ID)
			} else {
				crowded = true
			}
		}

		if _, skipped := skip[evt.ID]; skipped {
			return true
		}
		if needle != "" && !strings.Contains(strings.ToLower(evt.Content), needle) {
			return true
		}

		result.Events = append(result.Events, rowFor(relay, evt))
		return len(result.Events) < limit
	})
	if err != nil {
		return eventListResult{}, err
	}

	result.Returned = len(result.Events)
	result.Scanned = scanned
	result.ScanBudget = budget
	result.Complete = complete
	result.Truncated = !complete && len(result.Events) >= limit

	if !complete && tailAt > 0 {
		cursor := eventCursor{Until: tailAt, Seen: tailIDs}
		if crowded {
			// more events share this second than the cursor can carry, so it has
			// to step over it and whatever else was in it is lost. The same trade
			// eachEvent makes, reported the same way.
			cursor = eventCursor{Until: tailAt - 1}
			result.Warning = "more events share one second than a page can carry, so some were skipped"
		}
		if encoded, err := encodeEventCursor(cursor); err == nil {
			result.NextCursor = encoded
		}
	}

	counts, countedAt := eventCounts.get(ctx)
	result.Total = counts[relay]
	result.CountedAt = countedAt.Unix()

	result.Profiles = resolveProfiles(ctx, relay, result.Events)
	return result, nil
}

// getEvent answers with one whole event and what the relay knows about it.
func getEvent(ctx context.Context, relay, id string) (eventDetail, error) {
	db, ok := dbs[relay]
	if !ok {
		return eventDetail{}, fmt.Errorf("unknown relay %q", relay)
	}

	found, err := findStoredEvents(ctx, db, []string{id})
	if err != nil {
		return eventDetail{}, err
	}
	evt, ok := found[id]
	if !ok {
		return eventDetail{}, fmt.Errorf("this relay is not storing that event")
	}

	detail := eventDetail{
		Relay: relay,
		Event: evt,
		Size:  len(evt.String()),
		Class: eventClass(evt.Kind),
		// one or two queries for one event, which is why this is here and not on
		// every row of the list
		Deleted: isDeleted(ctx, db, evt),
		Banned:  isBannedEvent(relay, id),
	}
	if detail.Banned {
		detail.BanReason = management.get().relayOrEmpty(relay).BannedEvents[id]
	}
	if !nostr.IsRegularKind(evt.Kind) {
		detail.Address = fmt.Sprintf("%d:%s:%s", evt.Kind, evt.PubKey, evt.Tags.GetD())
	}
	detail.Permanent = permanentlyGone(evt, detail.Banned, detail.Deleted)
	return detail, nil
}

// permanentlyGone reports whether anything would stop an event coming back after
// it is deleted from the store.
//
// A replaceable or addressable event is never permanent on the strength of a
// delete request alone: such a request only covers the versions that existed
// when it was made, so the author's next update walks straight back in.
func permanentlyGone(evt *nostr.Event, banned, deleted bool) bool {
	if banned {
		return true
	}
	return deleted && nostr.IsRegularKind(evt.Kind)
}

// deleteEvents removes stored events by id.
//
// The events are read back in one query and fully drained before anything is
// deleted: deleting inside the loop that is still reading would hold a read
// transaction open across every write, which is fine for the one event banEvent
// removes and is not a pattern to extend to five hundred.
func deleteEvents(ctx context.Context, relay string, ids []string) (eventBulkResult, error) {
	db, ok := dbs[relay]
	if !ok {
		return eventBulkResult{}, fmt.Errorf("unknown relay %q", relay)
	}

	result := eventBulkResult{Relay: relay, Deleted: make([]eventDeleteResult, 0, len(ids))}

	found, err := findStoredEvents(ctx, db, ids)
	if err != nil {
		return eventBulkResult{}, err
	}

	for _, id := range ids {
		evt, stored := found[id]
		if !stored {
			result.Missing++
			result.Deleted = append(result.Deleted, eventDeleteResult{ID: id})
			continue
		}

		banned := isBannedEvent(relay, id)
		permanent := permanentlyGone(evt, banned, isDeleted(ctx, db, evt))

		row := eventDeleteResult{
			ID:        id,
			Kind:      evt.Kind,
			PubKey:    evt.PubKey,
			Class:     eventClass(evt.Kind),
			Permanent: permanent,
		}
		switch {
		case banned:
			row.Reason = "this event id is on the relay's banned list, so it cannot be published again"
		case permanent:
			row.Reason = "the relay holds a delete request covering this event, so it will not be accepted again"
		}

		if err := db.DeleteEvent(ctx, evt); err != nil {
			row.Error = err.Error()
			result.Errors++
		} else {
			row.Deleted = true
			result.Removed++
			if permanent {
				result.Permanent++
			}
		}
		result.Deleted = append(result.Deleted, row)
	}

	if result.Removed > 0 {
		eventCounts.invalidate()
	}
	if result.Permanent < result.Removed {
		// the honest answer to "delete only, no ban". Said here rather than left
		// to the UI to remember, because it is the whole difference between this
		// and banevent.
		result.Warning = "this removed the stored copy only; nothing here stops the same event being published to this relay again, or being brought back by an import or a restore — ban it as well to keep it out"
	}
	return result, nil
}
