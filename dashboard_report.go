package main

import (
	"context"
	"time"
)

//
// what the dashboard method answers with
//

// dashRange is one time window, already bucketed at the resolution the client
// should draw.
type dashRange struct {
	BucketSeconds int64               `json:"bucket_seconds"`
	Buckets       []int64             `json:"buckets"`
	Series        map[string][]*int64 `json:"series"`
}

// dashRelay is one relay's live state.
type dashRelay struct {
	Relay           string           `json:"relay"`
	Label           string           `json:"label"`
	LiveConnections int              `json:"live_connections"`
	SinceBoot       map[string]int64 `json:"since_boot"`
	Corpus          *corpusStats     `json:"corpus"`
}

// dashboardResult is the whole page in one answer.
//
// Anything that could not be produced cheaply is present but empty and stamped,
// rather than absent: a nil corpus with computed_at 0 tells the page to say "not
// counted yet", where a missing field would simply render as a confident zero.
type dashboardResult struct {
	GeneratedAt   int64    `json:"generated_at"`
	StartedAt     int64    `json:"started_at"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	Version       string   `json:"version"`
	Relays        []string `json:"relays"`

	// Ranges is keyed "24h", "7d", "30d". All three are returned together so the
	// range switch on the page costs nothing: re-fetching per range would make
	// every click a signer prompt.
	Ranges map[string]dashRange `json:"ranges"`

	Live []dashRelay `json:"live"`

	Events    map[string]int64 `json:"events"`
	CountedAt int64            `json:"counted_at"`

	Blobs *blobStats `json:"blobs"`

	// Analytics reports on the counters themselves: whether they are being
	// written anywhere, and how far back the history goes. Without this the page
	// would draw a cliff at the retention edge and call it a quiet week.
	Analytics dashAnalyticsInfo `json:"analytics"`

	Warning string `json:"warning,omitempty"`
}

type dashAnalyticsInfo struct {
	Enabled             bool   `json:"enabled"`
	Persisted           bool   `json:"persisted"`
	FirstBucket         int64  `json:"first_bucket"`
	HourlyRetentionDays int    `json:"hourly_retention_days"`
	DailyRetentionDays  int    `json:"daily_retention_days"`
	AggregatesAt        int64  `json:"aggregates_at"`
	Note                string `json:"note,omitempty"`
}

// dashRangeSpec describes one window: how far back, and at what resolution.
//
// 24h and 7d come from the hourly buckets; 30d comes from the daily ones. Seven
// hundred hourly points into a chart a few hundred pixels wide is more than one
// per pixel, so the rollup happens here rather than in the browser.
var dashRangeSpecs = []struct {
	name   string
	window time.Duration
	bucket time.Duration
	daily  bool
}{
	{"24h", 24 * time.Hour, time.Hour, false},
	{"7d", 7 * 24 * time.Hour, time.Hour, false},
	{"30d", 30 * 24 * time.Hour, 24 * time.Hour, true},
}

// dashSeriesNames are the counters the page plots. Everything else is still in
// the buckets on disk; this is only what rides on the wire.
var dashSeriesNames = []string{
	"event_stored",
	"event_attempts",
	"event_passed",
	"req_filters",
	"req_passed",
	"conn_opened",
	"uptime_seconds",
}

// buildRange lays the stored buckets onto a regular grid.
//
// A bucket that is not in the store becomes a null rather than a zero, and the
// difference matters: zero means the relay was up and nothing happened, null
// means there is no record — which is what an hour before analytics was switched
// on, or an hour the relay was down, actually is. Drawing those as zero would put
// a confident floor under a blackout.
func buildRange(now time.Time, spec int, hourly, daily map[string][]metricsBucket) dashRange {
	s := dashRangeSpecs[spec]
	source := hourly
	truncate := time.Hour
	if s.daily {
		source = daily
		truncate = 24 * time.Hour
	}

	end := now.UTC().Truncate(truncate)
	count := int(s.window / s.bucket)
	starts := make([]int64, 0, count)
	index := make(map[int64]int, count)
	for i := count - 1; i >= 0; i-- {
		at := end.Add(-time.Duration(i) * s.bucket).Unix()
		index[at] = len(starts)
		starts = append(starts, at)
	}

	// summed across relays: the page shows the relay breakdown from the per relay
	// series it builds itself, and every chart on it is a total
	series := make(map[string][]*int64, len(dashSeriesNames))
	for _, name := range dashSeriesNames {
		series[name] = make([]*int64, len(starts))
	}

	for _, buckets := range source {
		for _, bucket := range buckets {
			slot, ok := index[bucket.Start]
			if !ok {
				continue
			}
			for _, name := range dashSeriesNames {
				value := bucket.Counters[name]
				if series[name][slot] == nil {
					// a bucket exists for this slot, so it stops being null even if
					// the counter itself is zero
					zero := int64(0)
					series[name][slot] = &zero
				}
				*series[name][slot] += value
			}
		}
	}

	return dashRange{BucketSeconds: int64(s.bucket.Seconds()), Buckets: starts, Series: series}
}

// buildDashboard assembles the answer. It reads snapshots and caches only — it
// never starts a scan.
func buildDashboard(ctx context.Context) dashboardResult {
	now := time.Now()
	hourly, daily, persisted := metrics.snapshot()

	result := dashboardResult{
		GeneratedAt:   now.Unix(),
		StartedAt:     startedAt.Unix(),
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		Version:       config.RelayVersion,
		Relays:        []string{relayOutbox, relayPrivate, relayChat, relayInbox},
		Ranges:        map[string]dashRange{},
		Live:          []dashRelay{},
		Analytics: dashAnalyticsInfo{
			Enabled:             config.AnalyticsEnabled,
			Persisted:           persisted && config.AnalyticsEnabled,
			HourlyRetentionDays: config.AnalyticsHourlyRetentionDays,
			DailyRetentionDays:  config.AnalyticsDailyRetentionDays,
		},
	}
	if config.AnalyticsEnabled && !persisted {
		result.Analytics.Note = "the analytics file could not be read at startup, so counters are being kept in memory only and a restart will lose them"
	}
	if !config.AnalyticsEnabled {
		result.Analytics.Note = "analytics are switched off, so there is no traffic history to show"
	}

	for i := range dashRangeSpecs {
		result.Ranges[dashRangeSpecs[i].name] = buildRange(now, i, hourly, daily)
	}

	// the oldest bucket anywhere is where history actually begins; before it there
	// is no data, which is not the same as no traffic
	for _, buckets := range daily {
		for _, bucket := range buckets {
			if result.Analytics.FirstBucket == 0 || bucket.Start < result.Analytics.FirstBucket {
				result.Analytics.FirstBucket = bucket.Start
			}
		}
	}

	live := liveConnections()
	boot := sinceBoot()
	corpus := aggregateSnapshot()
	labels := map[string]string{
		relayOutbox: "Outbox", relayPrivate: "Private", relayChat: "Chat", relayInbox: "Inbox",
	}
	for _, name := range result.Relays {
		row := dashRelay{
			Relay:           name,
			Label:           labels[name],
			LiveConnections: live[name],
			SinceBoot:       boot[name],
			Corpus:          corpus[name],
		}
		if row.SinceBoot == nil {
			row.SinceBoot = map[string]int64{}
		}
		result.Live = append(result.Live, row)
		if row.Corpus != nil && row.Corpus.ComputedAt > result.Analytics.AggregatesAt {
			result.Analytics.AggregatesAt = row.Corpus.ComputedAt
		}
	}

	// the same cache the stats method reads, so the two never disagree — two
	// different event counts on two tabs is the fastest way to lose an operator's
	// trust in both
	counts, countedAt := eventCounts.get(ctx)
	result.Events = counts
	result.CountedAt = countedAt.Unix()

	// peeked, never rebuilt: buildBlobInventory walks every file on disk, and a
	// dashboard refresh must not be the thing that triggers it
	if blobs, ok := blobInventory.peek(); ok {
		result.Blobs = &blobs
	}

	if result.Analytics.AggregatesAt == 0 {
		result.Warning = "the first pass over the databases has not finished yet, so the kind mix, top authors and stored totals are still being counted"
	}
	return result
}
