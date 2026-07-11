package wot

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"github.com/puzpuzpuz/xsync/v4"
)

const DefaultWotLevel = 3

type SimpleInMemory struct {
	pubkeys atomic.Pointer[map[string]bool]

	// Dependencies for Refresh
	Pool               *nostr.Pool
	WhitelistedPubKeys map[string]struct{}
	SeedRelays         []string
	WotDepth           int
	MinFollowers       int
	WotFetchTimeout    int
}

func NewSimpleInMemory(pool *nostr.Pool, whitelistedPubKeys map[string]struct{}, seedRelays []string, wotDepth int, minFollowers int, wotFetchTimeout int) *SimpleInMemory {
	return &SimpleInMemory{
		Pool:               pool,
		WhitelistedPubKeys: whitelistedPubKeys,
		SeedRelays:         seedRelays,
		WotDepth:           wotDepth,
		MinFollowers:       minFollowers,
		WotFetchTimeout:    wotFetchTimeout,
	}
}

func (wt *SimpleInMemory) Has(_ context.Context, pubKey string) bool {
	if wt.WotDepth == 0 {
		return true
	}
	m := wt.pubkeys.Load()
	if m == nil {
		return false
	}
	return (*m)[pubKey]
}

func (wt *SimpleInMemory) Init(ctx context.Context) {
	switch wt.WotDepth {
	case 0:
		slog.Info("Web of Trust Level 0 -> Disabled (Public Relay)")
	case 1:
		slog.Info("Web of Trust Level 1 -> Private Relay for the Owner")
	case 2:
		slog.Info("Web of Trust Level 2 -> Only pubkeys that the relay Owner is following directly can write to Inbox and Chat relays")
	case 3:
		slog.Info("Web of Trust Level 3 -> Connection of Connections (owner, follows, and their follows) with", "minFollowers", wt.MinFollowers)
	default:
		slog.Error("🚫 Web of Trust level not supported, must be between 0 and 3", "level", wt.WotDepth)
		slog.Info("Using default Web of Trust Level")
		wt.WotDepth = DefaultWotLevel
		slog.Info("Web of Trust Level 3 -> Connection of Connections (owner, follows, and their follows) with", "minFollowers", wt.MinFollowers)

	}
	wt.Refresh(ctx)
}

func (wt *SimpleInMemory) Refresh(ctx context.Context) {
	if wt.WotDepth == 0 {
		return
	}

	var eventsAnalysed atomic.Int64
	pubkeyFollowers := xsync.NewMap[string, *atomic.Int64]()
	relaysDiscovered := xsync.NewMap[string, bool]()
	oneHopNetwork := make(map[string]bool)
	newWot := make(map[string]bool)

	if wt.WotDepth >= 1 {
		for pubkey := range wt.WhitelistedPubKeys {
			newWot[pubkey] = true
		}
	}

	if wt.WotDepth == 1 {
		wt.pubkeys.Store(&newWot)
		return
	}

	timeout := time.Duration(wt.WotFetchTimeout) * time.Second
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindFollowList},
	}
	for pubkeyHex := range wt.WhitelistedPubKeys {
		filter.Authors = append(filter.Authors, nostr.MustPubKeyFromHex(pubkeyHex))
	}

	slog.Info("🛜 fetching Nostr events to build WoT")

	events := wt.Pool.FetchMany(timeoutCtx, wt.SeedRelays, filter, nostr.SubscriptionOptions{})
	for ev := range latestEventByKindAndPubkey(timeoutCtx, events, &eventsAnalysed) {
		for contact := range ev.Tags.FindAll("p") {
			if len(contact) > 1 {
				followers, _ := pubkeyFollowers.LoadOrStore(contact[1], &atomic.Int64{})
				followers.Add(1)
				oneHopNetwork[contact[1]] = true
				newWot[contact[1]] = true
			}
		}
	}

	if wt.WotDepth == 2 {
		slog.Info("🕸️ analysed Nostr events", "count", eventsAnalysed.Load())
		slog.Info("📈 direct followers in import relays", "🫂pubkeys", len(newWot), "🔗relays", len(wt.SeedRelays))
		wt.pubkeys.Store(&newWot)
		return
	}

	slog.Info("🕸️ analysing Nostr events", "count", eventsAnalysed.Load())

	processBatch := func(pubkeys []string) {
		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		done := make(chan struct{})
		authors := make([]nostr.PubKey, 0, len(pubkeys))
		for _, pubkey := range pubkeys {
			authors = append(authors, nostr.MustPubKeyFromHex(pubkey))
		}

		filter := nostr.Filter{
			Authors: authors,
			Kinds:   []nostr.Kind{nostr.KindFollowList, nostr.KindRelayListMetadata},
		}

		go func() {
			defer cancel()

			events := wt.Pool.FetchMany(timeoutCtx, wt.SeedRelays, filter, nostr.SubscriptionOptions{})
			for ev := range latestEventByKindAndPubkey(timeoutCtx, events, &eventsAnalysed) {
				for contact := range ev.Tags.FindAll("p") {
					if len(contact) > 1 {
						followers, _ := pubkeyFollowers.LoadOrStore(contact[1], &atomic.Int64{})
						followers.Add(1)
					}
				}

				for relay := range ev.Tags.FindAll("r") {
					relaysDiscovered.Store(relay[1], true)
				}
			}
			close(done)
		}()

		select {
		case <-done:
			slog.Info("🕸️ analysing Nostr events", "count", eventsAnalysed.Load())
		case <-timeoutCtx.Done():
			slog.Error("🚫 timeout while fetching events, moving to the next batch")
		}
	}

	// Split analysis into batches of 100 pubkeys
	keys := slices.Collect(maps.Keys(oneHopNetwork))
	for batch := range slices.Chunk(keys, 100) {
		processBatch(batch)
	}

	slog.Info("📈 totals", "🫂pubkeys", pubkeyFollowers.Size(), "🔗relays", relaysDiscovered.Size())

	// Log Top N pubkeys by follower count for debugging purposes
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		type pubkeyCount struct {
			pubkey string
			count  int
		}
		const topN = 20

		h := make([]pubkeyCount, 0, topN+1)

		pubkeyFollowers.Range(func(pubkey string, followers *atomic.Int64) bool {
			count := int(followers.Load())
			if len(h) < topN {
				h = append(h, pubkeyCount{pubkey, count})
				if len(h) == topN {
					slices.SortFunc(h, func(a, b pubkeyCount) int {
						if n := cmp.Compare(a.count, b.count); n != 0 {
							return n
						}
						return cmp.Compare(b.pubkey, a.pubkey)
					})
				}
			} else if count > h[0].count || (count == h[0].count && pubkey < h[0].pubkey) {
				h[0] = pubkeyCount{pubkey, count}
				// Keep it sorted or use a proper heap. For a small value of N, keeping it sorted is simple.
				// Since we only replaced the smallest element, we can just "bubble up" that element to restore order.
				for i := 0; i < len(h)-1; i++ {
					if h[i].count > h[i+1].count || (h[i].count == h[i+1].count && h[i].pubkey < h[i+1].pubkey) {
						h[i], h[i+1] = h[i+1], h[i]
					} else {
						break
					}
				}
			}
			return true
		})

		slices.Reverse(h)

		slog.Debug(fmt.Sprintf("📊 WoT top %d pubkeys by follower count", topN))
		for _, c := range h {
			slog.Debug("👤", "pubkey", c.pubkey, "count", c.count)
		}
	}

	// Filter out pubkeys with less than minimum followers
	minimumFollowers := int64(wt.MinFollowers)
	pubkeyFollowers.Range(func(pubkey string, followers *atomic.Int64) bool {
		if followers.Load() >= minimumFollowers {
			newWot[pubkey] = true
		}
		return true
	})

	slog.Info("🫥 pruned pubkeys without minimum common followers", "🚧minimum", minimumFollowers, "🫂kept", len(newWot), "🗑️eliminated", pubkeyFollowers.Size()-len(newWot))

	wt.pubkeys.Store(&newWot)
}

func latestEventByKindAndPubkey(ctx context.Context, events <-chan nostr.RelayEvent, counter *atomic.Int64) <-chan nostr.RelayEvent {
	ch := make(chan nostr.RelayEvent)
	go func() {
		defer close(ch)
		latestEvents := make(map[string]nostr.RelayEvent)
		for ev := range events {
			select {
			case <-ctx.Done():
				return
			default:
				counter.Add(1)
				key := fmt.Sprintf("%d:%s", ev.Kind, ev.PubKey)
				if old, ok := latestEvents[key]; !ok || ev.CreatedAt > old.CreatedAt {
					latestEvents[key] = ev
				}
			}
		}
		for _, ev := range latestEvents {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}()
	return ch
}
