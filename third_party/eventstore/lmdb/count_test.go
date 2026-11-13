package lmdb

import (
	"context"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Counting must advance the cursor past every match: it used to count the
// first matching key and then spin on it forever.
func TestCountAdvancesPastMatches(t *testing.T) {
	db := &LMDBBackend{Path: t.TempDir(), EnableHLLCacheFor: func(kind int) (bool, bool) { return false, false }}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	sk := nostr.GeneratePrivateKey()
	target := "0000000000000000000000000000000000000000000000000000000000000001"
	for i := 0; i < 3; i++ {
		for _, kind := range []int{1, 7} {
			evt := &nostr.Event{CreatedAt: nostr.Timestamp(1000 + i), Kind: kind, Tags: nostr.Tags{{"e", target}}}
			if err := evt.Sign(sk); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveEvent(ctx, evt); err != nil {
				t.Fatal(err)
			}
		}
	}
	pk, _ := nostr.GetPublicKey(sk)

	within := func(t *testing.T, f func() int64) int64 {
		t.Helper()
		done := make(chan int64, 1)
		go func() { done <- f() }()
		select {
		case n := <-done:
			return n
		case <-time.After(10 * time.Second):
			t.Fatal("count did not return")
			return 0
		}
	}

	for _, tt := range []struct {
		name   string
		filter nostr.Filter
		want   int64
	}{
		{"index only", nostr.Filter{Kinds: []int{1}}, 3},
		{"checked against the event", nostr.Filter{Authors: []string{pk}, Kinds: []int{7}}, 3},
		{"checked against a tag", nostr.Filter{Kinds: []int{7}, Tags: nostr.TagMap{"e": {target}}}, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := within(t, func() int64 {
				n, err := db.CountEvents(ctx, tt.filter)
				if err != nil {
					t.Error(err)
				}
				return n
			})
			if got != tt.want {
				t.Errorf("CountEvents = %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("hll", func(t *testing.T) {
		got := within(t, func() int64 {
			n, _, err := db.CountEventsHLL(ctx, nostr.Filter{Kinds: []int{7}, Tags: nostr.TagMap{"e": {target}}}, 0)
			if err != nil {
				t.Error(err)
			}
			return n
		})
		if got != 3 {
			t.Errorf("CountEventsHLL = %d, want 3", got)
		}
	})
}
