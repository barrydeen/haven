package main

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

//
// the dashboard
//
// One NIP-86 method answers the whole page, because every call is a signer
// prompt: asking for counters, then a time series, then aggregates would cost the
// owner three. It also returns every time range at once, so the 24h/7d/30d switch
// is client side and free.
//
// Nothing expensive is computed on the request path. The aggregates below need a
// full walk of every database, and the backends never check the context, so doing
// that inside the request would let one dashboard load pin a core with no way to
// cancel it — exactly the hazard the one minute cache on event counts exists to
// contain. A goroutine recomputes them on a timer and the method serves whatever
// snapshot is current, stamped with when it was taken.
//

// authorCount is one author's share of a relay's stored events.
type authorCount struct {
	PubKey string `json:"pubkey"`
	Events int64  `json:"events"`
	Bytes  int64  `json:"bytes"`
	Name   string `json:"name,omitempty"`
}

// dayCount is one day of stored events, by the events' own created_at rather than
// by when they arrived — which is what makes a growth chart possible for history
// that predates analytics being switched on at all.
type dayCount struct {
	Start  int64 `json:"start"`
	Events int64 `json:"events"`
	Bytes  int64 `json:"bytes"`
}

// corpusStats is what one full pass over one relay's database found.
//
// It is the only place the dashboard can learn what is already *stored*, as
// opposed to what has arrived since haven last restarted, and it costs a walk of
// every event to produce. Every field is stamped with when, and with whether the
// walk finished.
type corpusStats struct {
	Relay      string `json:"relay"`
	ComputedAt int64  `json:"computed_at"`
	DurationMs int64  `json:"duration_ms"`

	Events   int64 `json:"events"`
	Complete bool  `json:"complete"`
	// Bytes is the summed serialized size of the events, which is the number an
	// operator means by "how much is my relay holding". It is deliberately not a
	// filesystem measurement: badger preallocates sparse value logs, so a walk of
	// db/ reports gigabytes for a relay holding a handful of events.
	Bytes    int64 `json:"bytes"`
	AvgBytes int64 `json:"avg_bytes"`

	Kinds      map[string]int64 `json:"kinds"`
	KindsOther int64            `json:"kinds_other,omitempty"`

	Authors          []authorCount `json:"authors"`
	AuthorsTotal     int64         `json:"authors_total"`
	AuthorsTruncated bool          `json:"authors_truncated,omitempty"`

	Oldest int64      `json:"oldest"`
	Newest int64      `json:"newest"`
	Daily  []dayCount `json:"daily"`

	Warning string `json:"warning,omitempty"`
}

// aggregates is the last completed pass over every database, published as one
// immutable snapshot the way banlist.go and management.go publish theirs.
var aggregates atomic.Pointer[map[string]*corpusStats]

func aggregateSnapshot() map[string]*corpusStats {
	if p := aggregates.Load(); p != nil {
		return *p
	}
	return map[string]*corpusStats{}
}

// scanCorpus walks one database and summarises it.
//
// maxAggregateScan bounds the walk. Past it the result says complete:false with
// the count it did manage, the way buildBlobInventory already does, rather than
// presenting a floor as a total.
func scanCorpus(ctx context.Context, name string, db DBBackend) *corpusStats {
	started := time.Now()
	stats := &corpusStats{
		Relay:   name,
		Kinds:   map[string]int64{},
		Authors: []authorCount{},
		Daily:   []dayCount{},
	}

	kinds := map[int]int64{}
	authors := map[string]*authorCount{}
	daily := map[int64]*dayCount{}

	// the author map is the one unbounded structure here. On a busy inbox it
	// could be a hundred thousand entries, so it stops growing well before that
	// and says the top N is a top N of what it tracked.
	authorCap := config.AnalyticsMaxAuthors * 50

	visited, complete, err := eachEvent(ctx, db, nostr.Filter{}, maxAggregateScan, func(evt *nostr.Event) bool {
		size := int64(len(evt.String()))
		stats.Bytes += size

		if len(kinds) < config.AnalyticsMaxKinds {
			kinds[evt.Kind]++
		} else if _, known := kinds[evt.Kind]; known {
			kinds[evt.Kind]++
		} else {
			stats.KindsOther++
		}

		if a, ok := authors[evt.PubKey]; ok {
			a.Events++
			a.Bytes += size
		} else if len(authors) < authorCap {
			authors[evt.PubKey] = &authorCount{PubKey: evt.PubKey, Events: 1, Bytes: size}
		} else {
			stats.AuthorsTruncated = true
		}

		at := int64(evt.CreatedAt)
		if stats.Oldest == 0 || at < stats.Oldest {
			stats.Oldest = at
		}
		if at > stats.Newest {
			stats.Newest = at
		}
		day := time.Unix(at, 0).UTC().Truncate(24 * time.Hour).Unix()
		if d, ok := daily[day]; ok {
			d.Events++
			d.Bytes += size
		} else {
			daily[day] = &dayCount{Start: day, Events: 1, Bytes: size}
		}
		return true
	})
	if err != nil {
		slog.Error("🚫 error walking a relay database for the dashboard", "relay", name, "error", err)
		stats.Warning = "this relay's database could not be read to the end, so these numbers are partial"
	}

	stats.Events = int64(visited)
	stats.Complete = complete && err == nil
	if !stats.Complete && stats.Warning == "" {
		stats.Warning = "the scan stopped at its budget, so these numbers cover only the newest events"
	}
	if stats.Events > 0 {
		stats.AvgBytes = stats.Bytes / stats.Events
	}

	for kind, n := range kinds {
		stats.Kinds[itoa(kind)] = n
	}

	stats.AuthorsTotal = int64(len(authors))
	ranked := make([]authorCount, 0, len(authors))
	for _, a := range authors {
		ranked = append(ranked, *a)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Events != ranked[j].Events {
			return ranked[i].Events > ranked[j].Events
		}
		// a stable tiebreak, so the list does not shuffle between scans
		return ranked[i].PubKey < ranked[j].PubKey
	})
	if len(ranked) > config.AnalyticsMaxAuthors {
		ranked = ranked[:config.AnalyticsMaxAuthors]
	}
	stats.Authors = ranked

	days := make([]dayCount, 0, len(daily))
	for _, d := range daily {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Start < days[j].Start })
	// only the window the dashboard draws; a relay with five years of imported
	// notes would otherwise ship two thousand points nobody renders
	if len(days) > config.AnalyticsDailyRetentionDays {
		days = days[len(days)-config.AnalyticsDailyRetentionDays:]
	}
	stats.Daily = days

	stats.ComputedAt = time.Now().Unix()
	stats.DurationMs = time.Since(started).Milliseconds()
	return stats
}

func itoa(n int) string {
	// strconv without the import churn in this file; kinds are small and positive
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// runAggregates recomputes the corpus snapshot on a timer.
//
// One database at a time with a pause between them: this is a full walk of every
// event the relay holds, and it must never be the reason a REQ is slow.
func runAggregates(ctx context.Context) {
	if !config.AnalyticsEnabled {
		return
	}
	// a short delay so the first pass is not competing with the import and the
	// web of trust refresh that start at the same moment
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	interval := time.Duration(config.AnalyticsAggregateMinutes) * time.Minute
	for {
		next := map[string]*corpusStats{}
		for name, db := range dbs {
			if ctx.Err() != nil {
				return
			}
			next[name] = scanCorpus(ctx, name, db)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		aggregates.Store(&next)
		slog.Debug("📊 dashboard aggregates refreshed", "databases", len(next))

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
