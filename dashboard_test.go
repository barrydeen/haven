package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// TestBuildDashboardShapeAndHonesty checks the two things the page depends on:
// that all three ranges arrive together (so switching between them costs no
// signature), and that a bucket with no record is null rather than zero.
func TestBuildDashboardShapeAndHonesty(t *testing.T) {
	db := withTestRelay(t, 50)
	seedNotes(t, db, 4, 1700000000)

	originalEnabled := config.AnalyticsEnabled
	config.AnalyticsEnabled = true
	t.Cleanup(func() { config.AnalyticsEnabled = originalEnabled })

	// a relay with some traffic already folded into the current hour
	m := metrics.relay(relayOutbox)
	m.c.add(eventStored, 7)
	m.c.add(reqFilters, 12)
	metrics.enabled = true
	metrics.fold(time.Now())

	result := buildDashboard(context.Background())

	for _, want := range []string{"24h", "7d", "30d"} {
		r, ok := result.Ranges[want]
		if !ok {
			t.Fatalf("range %q is missing; the page would have to fetch it and spend a signature", want)
		}
		if len(r.Buckets) == 0 {
			t.Errorf("range %q has no buckets", want)
		}
		for name, series := range r.Series {
			if len(series) != len(r.Buckets) {
				t.Errorf("range %q series %q has %d points for %d buckets", want, name, len(series), len(r.Buckets))
			}
		}
	}

	// the current hour has a record, so it is a number; the hour before this run
	// started has none, so it must be null and not a confident zero
	day := result.Ranges["24h"]
	stored := day.Series["event_stored"]
	if stored[len(stored)-1] == nil {
		t.Error("the current bucket is null despite traffic having been folded into it")
	} else if *stored[len(stored)-1] < 7 {
		t.Errorf("current bucket holds %d, want at least the 7 that were folded in", *stored[len(stored)-1])
	}
	nulls := 0
	for _, v := range stored {
		if v == nil {
			nulls++
		}
	}
	if nulls == 0 {
		t.Error("no bucket is null; an hour with no record must not be reported as an hour with no traffic")
	}

	if len(result.Live) != 4 {
		t.Errorf("live has %d relays, want 4", len(result.Live))
	}
	if result.Analytics.Enabled != true {
		t.Error("analytics reported as disabled")
	}
	if result.Warning == "" {
		t.Error("no warning that the first aggregate pass has not run yet")
	}

	// it must survive JSON, because that is how it reaches the page
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("the dashboard result does not marshal: %v", err)
	}
	if len(raw) < 200 {
		t.Errorf("the dashboard result is suspiciously small: %d bytes", len(raw))
	}
	t.Logf("dashboard payload: %d bytes", len(raw))
}

// TestScanCorpusSummarises covers the aggregate pass the dashboard's kind mix and
// top authors come from.
func TestScanCorpusSummarises(t *testing.T) {
	db := withTestRelay(t, 50)

	sk := nostr.GeneratePrivateKey()
	for i, kind := range []int{1, 1, 7, 1, 7, 0} {
		evt := &nostr.Event{
			CreatedAt: nostr.Timestamp(1700000000 + i*3600),
			Kind:      kind,
			Tags:      nostr.Tags{},
			Content:   "content here",
		}
		if err := evt.Sign(sk); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveEvent(context.Background(), evt); err != nil {
			t.Fatal(err)
		}
	}

	stats := scanCorpus(context.Background(), relayOutbox, db)
	if stats.Events != 6 {
		t.Errorf("events = %d, want 6", stats.Events)
	}
	if !stats.Complete {
		t.Error("a six event store was reported incomplete")
	}
	if stats.Kinds["1"] != 3 || stats.Kinds["7"] != 2 || stats.Kinds["0"] != 1 {
		t.Errorf("kind mix wrong: %v", stats.Kinds)
	}
	if len(stats.Authors) != 1 || stats.Authors[0].Events != 6 {
		t.Errorf("authors wrong: %+v", stats.Authors)
	}
	if stats.Bytes <= 0 || stats.AvgBytes <= 0 {
		t.Errorf("byte totals not computed: bytes=%d avg=%d", stats.Bytes, stats.AvgBytes)
	}
	if len(stats.Daily) == 0 {
		t.Error("no daily growth series")
	}
	if stats.Oldest == 0 || stats.Newest <= stats.Oldest {
		t.Errorf("time range wrong: oldest=%d newest=%d", stats.Oldest, stats.Newest)
	}
}

// TestMetricsBucketsSurviveAReload is the point of persisting them at all.
func TestMetricsBucketsSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/metrics.json"

	originalEnabled := config.AnalyticsEnabled
	originalPath := config.AnalyticsStateFile
	config.AnalyticsEnabled = true
	config.AnalyticsStateFile = path
	t.Cleanup(func() {
		config.AnalyticsEnabled = originalEnabled
		config.AnalyticsStateFile = originalPath
	})

	saved := metrics
	t.Cleanup(func() { metrics = saved })

	metrics = &metricsStore{
		live: map[string]*relayMetrics{}, state: newMetricsState(),
		last: map[string]sample{}, lastKinds: map[string]map[int]int64{},
		enabled: true, path: path,
	}
	m := metrics.relay(relayOutbox)
	m.c.add(eventStored, 5)
	m.recordKind(1)
	metrics.fold(time.Now())
	metrics.fold(time.Now().Add(time.Second))
	metrics.persist()

	// a fresh store reading the file back
	metrics = &metricsStore{
		live: map[string]*relayMetrics{}, state: newMetricsState(),
		last: map[string]sample{}, lastKinds: map[string]map[int]int64{},
	}
	loadMetricsStore()

	hourly, _, persisted := metrics.snapshot()
	if !persisted {
		t.Fatal("the store froze on a file it had just written")
	}
	buckets := hourly[relayOutbox]
	if len(buckets) == 0 {
		t.Fatal("no hourly buckets survived the reload")
	}
	total := int64(0)
	for _, b := range buckets {
		total += b.Counters["event_stored"]
	}
	if total != 5 {
		t.Errorf("event_stored survived as %d, want 5", total)
	}
}

// TestMetricsFreezesOnAnUnreadableFile: history is the one thing here that cannot
// be recreated, so a file that could not be parsed must never be overwritten.
func TestMetricsFreezesOnAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/metrics.json"
	if err := writeFileString(path, "{ this is not json"); err != nil {
		t.Fatal(err)
	}

	originalEnabled := config.AnalyticsEnabled
	originalPath := config.AnalyticsStateFile
	config.AnalyticsEnabled = true
	config.AnalyticsStateFile = path
	saved := metrics
	t.Cleanup(func() {
		config.AnalyticsEnabled = originalEnabled
		config.AnalyticsStateFile = originalPath
		metrics = saved
	})

	metrics = &metricsStore{
		live: map[string]*relayMetrics{}, state: newMetricsState(),
		last: map[string]sample{}, lastKinds: map[string]map[int]int64{},
	}
	loadMetricsStore()

	if _, _, persisted := metrics.snapshot(); persisted {
		t.Error("the store did not freeze on a file it could not parse")
	}

	// and a persist must leave the broken file exactly as it was
	metrics.relay(relayOutbox).c.add(eventStored, 1)
	metrics.fold(time.Now())
	metrics.persist()
	if got := readFileString(t, path); got != "{ this is not json" {
		t.Errorf("the unreadable file was overwritten with %q", got)
	}
}

func writeFileString(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
