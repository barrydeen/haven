package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

//
// the history that survives a restart
//
// Hourly buckets in a JSON file, written with the same temp-file, fsync, rename
// dance management.go uses, and frozen on a file that could not be read.
//
// A JSON file rather than a fifth event store, deliberately:
//
//   - The whole dataset is a couple of hundred kilobytes and stays in memory, so
//     serving the dashboard needs no scan, no cursor and no cancellation story.
//   - A fifth lmdb environment would reserve another 256GiB of address space for
//     a quarter of a megabyte of counters, and would fail outright on 32-bit.
//   - eventstore is an *event* store. Keeping a time series in it would mean
//     synthesising unsigned events and inheriting every trap eventscan.go exists
//     to work around, for no benefit.
//   - The operator already knows management.json. metrics.json sits beside it
//     under the same rules.
//

const metricsStateVersion = 1

// metricsFoldInterval is how often the running counters are folded into the
// current bucket. It is a constant rather than a setting because folding is a
// hundred atomic loads and costs nothing; the only thing the interval buys is
// accuracy at the hour boundary, where a fold landing at 12:00:07 attributes up
// to fifteen seconds of 11:59 to 12:00. That is well under a pixel on an hourly
// chart.
const metricsFoldInterval = 15 * time.Second

// metricsBucket is one hour, or one day, of one relay.
//
// The counters are a map keyed by name rather than a positional array: a
// positional file would silently re-label every historical bucket the first time
// somebody inserted a counter in the middle of the enum. This way a name haven no
// longer knows is dropped on load instead of shifting everything after it.
//
// Zero counters are omitted, in both directions: a counter that is not there is
// zero.
type metricsBucket struct {
	Start    int64            `json:"start"`
	Counters map[string]int64 `json:"counters,omitempty"`
	Kinds    map[string]int64 `json:"kinds,omitempty"`
}

func (b *metricsBucket) add(name string, n int64) {
	if n == 0 {
		return
	}
	if b.Counters == nil {
		b.Counters = make(map[string]int64)
	}
	b.Counters[name] += n
}

func (b *metricsBucket) addKind(kind int, n int64) {
	if n == 0 {
		return
	}
	if b.Kinds == nil {
		b.Kinds = make(map[string]int64)
	}
	b.Kinds[strconv.Itoa(kind)] += n
}

// metricsState is everything that survives a restart. Both slices are sorted
// ascending by Start and are only ever appended to at the tail or trimmed at the
// head, so no index has to be maintained.
type metricsState struct {
	Version int                        `json:"version"`
	SavedAt int64                      `json:"saved_at"`
	Hourly  map[string][]metricsBucket `json:"hourly"`
	Daily   map[string][]metricsBucket `json:"daily"`
}

func newMetricsState() *metricsState {
	return &metricsState{
		Version: metricsStateVersion,
		Hourly:  map[string][]metricsBucket{},
		Daily:   map[string][]metricsBucket{},
	}
}

// metricsStore owns everything.
//
// Unlike managementStore this uses a plain mutex rather than an atomic.Pointer
// snapshot, and the difference is deliberate: management state is read on every
// event and every connection, so its readers must never block. This is read when
// the owner opens the dashboard. The hot path never touches this lock — it only
// does atomic adds on relayMetrics — so a mutex costs nothing here and avoids
// cloning a thousand buckets every fifteen seconds just to publish a pointer.
type metricsStore struct {
	path    string
	enabled bool

	live map[string]*relayMetrics // built once, never written to afterwards

	mu          sync.Mutex
	frozen      bool
	state       *metricsState
	last        map[string]sample
	lastKinds   map[string]map[int]int64
	foldedAt    time.Time
	persistedAt time.Time
	dirty       bool
}

var metrics = &metricsStore{
	live:      map[string]*relayMetrics{},
	state:     newMetricsState(),
	last:      map[string]sample{},
	lastKinds: map[string]map[int]int64{},
}

// relay returns one relay's live counters, creating them on first use.
//
// It is called from initRelays before any traffic can arrive and never
// concurrently after, which is what makes the unguarded map safe.
func (s *metricsStore) relay(name string) *relayMetrics {
	if m, ok := s.live[name]; ok {
		return m
	}
	m := newRelayMetrics(name)
	s.live[name] = m
	return m
}

func hourOf(t time.Time) int64 { return t.UTC().Truncate(time.Hour).Unix() }
func dayOf(t time.Time) int64  { return t.UTC().Truncate(24 * time.Hour).Unix() }

// bucketAt finds or appends the bucket covering start. The slices are append-only
// at the tail in normal running, so the common case is the last element.
func bucketAt(buckets []metricsBucket, start int64) ([]metricsBucket, int) {
	for i := len(buckets) - 1; i >= 0; i-- {
		if buckets[i].Start == start {
			return buckets, i
		}
		if buckets[i].Start < start {
			break
		}
	}
	buckets = append(buckets, metricsBucket{Start: start})
	sort.Slice(buckets, func(a, b int) bool { return buckets[a].Start < buckets[b].Start })
	for i := range buckets {
		if buckets[i].Start == start {
			return buckets, i
		}
	}
	return buckets, len(buckets) - 1
}

// fold moves whatever the counters have accumulated since the last call into the
// bucket for now.
//
// The counters are monotonic and are never reset, so there is no window in which
// an increment can land between a read and a zeroing and be lost. The cost is one
// subtraction per counter per fold.
//
// The elapsed wall time is charged to uptime_seconds, which is what lets a reader
// tell an idle hour apart from an hour haven was not running. Without it a
// restart is indistinguishable from a quiet night and the chart draws a confident
// zero over a blackout.
func (s *metricsStore) fold(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.foldLocked(now)
}

func (s *metricsStore) foldLocked(now time.Time) {
	if !s.enabled {
		return
	}
	elapsed := int64(0)
	if !s.foldedAt.IsZero() {
		if d := now.Sub(s.foldedAt); d > 0 && d < time.Hour {
			elapsed = int64(d.Seconds())
		}
	}
	s.foldedAt = now

	hour, day := hourOf(now), dayOf(now)
	for name, m := range s.live {
		current := m.c.sample()
		// A relay with no previous reading is diffed against zero rather than
		// skipped: the counters start at zero when the process does, so the whole
		// of the current value is genuinely new. Skipping the first fold instead
		// would silently drop everything that happened before it — which on a busy
		// relay is the first fifteen seconds of every restart.
		previous := s.last[name]
		s.last[name] = current

		hb, hi := bucketAt(s.state.Hourly[name], hour)
		db, di := bucketAt(s.state.Daily[name], day)

		for k := counter(0); k < counterCount; k++ {
			delta := current[k] - previous[k]
			if delta <= 0 {
				continue
			}
			hb[hi].add(counterNames[k], delta)
			db[di].add(counterNames[k], delta)
			s.dirty = true
		}
		if elapsed > 0 {
			hb[hi].add(counterNames[uptimeSeconds], elapsed)
			db[di].add(counterNames[uptimeSeconds], elapsed)
			s.dirty = true
		}

		kinds := m.kindSnapshot()
		prevKinds := s.lastKinds[name]
		for kind, total := range kinds {
			delta := total
			if prevKinds != nil {
				delta -= prevKinds[kind]
			}
			if delta <= 0 {
				continue
			}
			hb[hi].addKind(kind, delta)
			db[di].addKind(kind, delta)
			s.dirty = true
		}
		s.lastKinds[name] = kinds

		s.state.Hourly[name] = hb
		s.state.Daily[name] = db
	}
}

// prune drops what has aged out. Hourly resolution is kept for a shorter window
// than daily because a thirty day chart draws thirty points, not seven hundred:
// hourly detail past a week is data nobody renders and bytes everybody pays for.
func (s *metricsStore) pruneLocked(now time.Time) {
	hourCut := now.Add(-time.Duration(config.AnalyticsHourlyRetentionDays) * 24 * time.Hour).Unix()
	dayCut := now.Add(-time.Duration(config.AnalyticsDailyRetentionDays) * 24 * time.Hour).Unix()

	for name, buckets := range s.state.Hourly {
		kept := buckets[:0]
		for _, b := range buckets {
			if b.Start >= hourCut {
				kept = append(kept, b)
			}
		}
		s.state.Hourly[name] = kept
	}
	for name, buckets := range s.state.Daily {
		kept := buckets[:0]
		for _, b := range buckets {
			if b.Start >= dayCut {
				kept = append(kept, b)
			}
		}
		s.state.Daily[name] = kept
	}
}

// snapshot copies the buckets for a reader. The copy is what lets the dashboard
// build a response without holding the lock across JSON encoding.
func (s *metricsStore) snapshot() (map[string][]metricsBucket, map[string][]metricsBucket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := func(in map[string][]metricsBucket) map[string][]metricsBucket {
		out := make(map[string][]metricsBucket, len(in))
		for name, buckets := range in {
			out[name] = append([]metricsBucket(nil), buckets...)
		}
		return out
	}
	return clone(s.state.Hourly), clone(s.state.Daily), !s.frozen
}

// persist writes the file. The marshal happens under the lock and the write
// outside it, so a slow disk cannot block the folder or a dashboard request.
func (s *metricsStore) persist() {
	s.mu.Lock()
	if !s.enabled || s.frozen || !s.dirty {
		s.mu.Unlock()
		return
	}
	s.state.Version = metricsStateVersion
	s.state.SavedAt = time.Now().Unix()
	raw, err := json.Marshal(s.state)
	s.dirty = false
	s.persistedAt = time.Now()
	path := s.path
	s.mu.Unlock()

	if err != nil {
		slog.Error("🚫 error encoding the analytics file", "error", err)
		return
	}

	// temp file, fsync, rename: the same dance management.go does, so a crash
	// mid-write leaves the previous file rather than half of this one
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Error("🚫 error opening the analytics file", "path", tmp, "error", err)
		return
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		slog.Error("🚫 error writing the analytics file", "path", tmp, "error", err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		slog.Error("🚫 error syncing the analytics file", "path", tmp, "error", err)
		return
	}
	if err := f.Close(); err != nil {
		slog.Error("🚫 error closing the analytics file", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Error("🚫 error replacing the analytics file", "path", path, "error", err)
	}
}

// loadMetricsStore reads the history a previous run left behind.
//
// A missing file is a first boot. A file that cannot be read, cannot be parsed,
// or was written by a newer haven leaves the store FROZEN: counting continues in
// memory and the dashboard keeps working, but nothing is written to that path
// again. The alternative is overwriting a file the operator may have been about
// to look at, and history is the one thing here that cannot be recreated. Every
// response says so, so the owner finds out now rather than in three weeks.
func loadMetricsStore() {
	metrics.enabled = config.AnalyticsEnabled
	metrics.path = config.AnalyticsStateFile
	if !metrics.enabled {
		log.Println("📊 Analytics are disabled")
		return
	}

	raw, err := os.ReadFile(metrics.path)
	if os.IsNotExist(err) {
		slog.Debug("no analytics file yet", "path", metrics.path)
		return
	}
	if err != nil {
		metrics.frozen = true
		slog.Error("🚫 the analytics file could not be read, so nothing will be written to it", "path", metrics.path, "error", err)
		return
	}

	var state metricsState
	if err := json.Unmarshal(raw, &state); err != nil {
		metrics.frozen = true
		slog.Error("🚫 the analytics file could not be parsed, so nothing will be written to it", "path", metrics.path, "error", err)
		return
	}
	if state.Version > metricsStateVersion {
		metrics.frozen = true
		slog.Error("🚫 the analytics file was written by a newer haven, so nothing will be written to it",
			"path", metrics.path, "file", state.Version, "supported", metricsStateVersion)
		return
	}
	if state.Hourly == nil {
		state.Hourly = map[string][]metricsBucket{}
	}
	if state.Daily == nil {
		state.Daily = map[string][]metricsBucket{}
	}
	// a hand edited file should not produce a broken chart
	for _, buckets := range state.Hourly {
		sort.Slice(buckets, func(a, b int) bool { return buckets[a].Start < buckets[b].Start })
	}
	for _, buckets := range state.Daily {
		sort.Slice(buckets, func(a, b int) bool { return buckets[a].Start < buckets[b].Start })
	}

	metrics.state = &state
	log.Printf("📊 Analytics loaded: %d hourly and %d daily buckets from %s",
		countBuckets(state.Hourly), countBuckets(state.Daily), metrics.path)
}

func countBuckets(in map[string][]metricsBucket) int {
	n := 0
	for _, buckets := range in {
		n += len(buckets)
	}
	return n
}

// runMetrics is the one goroutine that writes. Everything else only ever adds to
// an atomic.
//
// It folds on a short tick so the current, partial hour is always up to date — an
// owner opening the dashboard at five past sees the five minutes that have
// happened, not an empty bar — and writes to disk on a much longer one, because
// the file is a crash backstop rather than the source of truth.
func runMetrics(ctx context.Context) {
	if !config.AnalyticsEnabled {
		return
	}
	fold := time.NewTicker(metricsFoldInterval)
	defer fold.Stop()

	flushEvery := time.Duration(config.AnalyticsFlushMinutes) * time.Minute
	hour := hourOf(time.Now())
	lastFlush := time.Now()

	for {
		select {
		case <-ctx.Done():
			// best effort on the way out: fold what has happened and write it
			metrics.fold(time.Now())
			metrics.persist()
			return
		case now := <-fold.C:
			metrics.fold(now)
			if h := hourOf(now); h != hour {
				// an hour just closed, which is the only moment at which losing the
				// in-memory copy would cost a whole bucket rather than a few minutes
				metrics.mu.Lock()
				metrics.pruneLocked(now)
				metrics.mu.Unlock()
				metrics.persist()
				hour = h
				lastFlush = now
				continue
			}
			if now.Sub(lastFlush) >= flushEvery {
				metrics.persist()
				lastFlush = now
			}
		}
	}
}

// liveConnections reports the current connection count per relay, derived from
// the connection set rather than from a counter, so it cannot drift out of step
// with it.
func liveConnections() map[string]int {
	out := make(map[string]int, len(metrics.live))
	for name, m := range metrics.live {
		out[name] = m.conns.Size()
	}
	return out
}

// sinceBoot reports the running totals, which are not the same as the sum of the
// buckets: the buckets survive a restart and these do not.
func sinceBoot() map[string]map[string]int64 {
	out := make(map[string]map[string]int64, len(metrics.live))
	for name, m := range metrics.live {
		s := m.c.sample()
		row := make(map[string]int64, counterCount)
		for k := counter(0); k < counterCount; k++ {
			if s[k] != 0 {
				row[counterNames[k]] = s[k]
			}
		}
		out[name] = row
	}
	return out
}

var _ = fmt.Sprintf
