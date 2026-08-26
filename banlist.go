package main

import (
	"context"
	"log"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"

	"github.com/nbd-wtf/go-nostr"
)

// KindBanList is the replaceable NIP-51 list the owner publishes to their outbox
// relay to ban pubkeys from writing to haven. Every "p" tag on it is a banned
// pubkey. Private (NIP-44 encrypted) entries are not supported: the relay has no
// key to read them with.
const KindBanList = 10084

// bannedPubKeys is the cached ban list, loaded from the outbox relay on startup
// and refreshed whenever the owner publishes a new version of the list.
var bannedPubKeys = &banList{}

type banList struct {
	state atomic.Pointer[banListState]
}

type banListState struct {
	createdAt nostr.Timestamp
	pubkeys   map[string]struct{}
}

// apply replaces the cached list with the one carried by event, unless the event
// isn't the owner's ban list or is older than what is already cached. It reports
// whether the cache changed.
func (bl *banList) apply(event *nostr.Event) bool {
	if event.Kind != KindBanList || event.PubKey != config.OwnerPubKey {
		return false
	}

	if cached := bl.state.Load(); cached != nil && event.CreatedAt <= cached.createdAt {
		slog.Debug("ℹ️ ignoring ban list older than the cached one", "event", event.ID)
		return false
	}

	pubkeys := make(map[string]struct{})
	for tag := range event.Tags.FindAll("p") {
		if len(tag) < 2 {
			continue
		}
		// banning the owner would lock them out of their own relay
		if tag[1] == config.OwnerPubKey {
			continue
		}
		pubkeys[tag[1]] = struct{}{}
	}

	bl.state.Store(&banListState{createdAt: event.CreatedAt, pubkeys: pubkeys})
	return true
}

func (bl *banList) has(pubKey string) bool {
	cached := bl.state.Load()
	if cached == nil {
		return false
	}
	_, ok := cached.pubkeys[pubKey]
	return ok
}

// list returns the pubkeys on the cached ban list, sorted, so the management
// API can report what is banned and say where each ban came from.
func (bl *banList) list() []string {
	cached := bl.state.Load()
	if cached == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(cached.pubkeys))
}

func (bl *banList) size() int {
	cached := bl.state.Load()
	if cached == nil {
		return 0
	}
	return len(cached.pubkeys)
}

// loadBanList caches the latest ban list the owner has published to their outbox
// relay. It must run before the relays start accepting events.
func loadBanList(ctx context.Context) {
	events, err := outboxDB.QueryEvents(ctx, nostr.Filter{
		Kinds:   []int{KindBanList},
		Authors: []string{config.OwnerPubKey},
		Limit:   1,
	})
	if err != nil {
		slog.Error("🚫 error loading the ban list", "error", err)
		return
	}

	for event := range events {
		bannedPubKeys.apply(event)
	}

	log.Println("🔨 Number of banned pubkeys:", bannedPubKeys.size())
}

// refreshBanList keeps the cache up to date as the owner edits their list.
func refreshBanList(_ context.Context, event *nostr.Event) {
	if bannedPubKeys.apply(event) {
		log.Println("🔨 Ban list updated, number of banned pubkeys:", bannedPubKeys.size())
	}
}
