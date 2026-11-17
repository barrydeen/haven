package main

import (
	"context"
	"iter"
	"log"
	"net/http"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

//
// relay hooks
//
// The successor relay exposes every hook as a single function where the old
// khatru had slices of them. These helpers keep haven's policy lists readable
// and preserve the old ordering semantics: the first policy to reject wins,
// and a policy that passes falls through to the next one.

type requestPolicy = func(context.Context, nostr.Filter) (bool, string)
type eventPolicy = func(context.Context, *nostr.Event) (bool, string)
type connectionPolicy = func(*http.Request) bool

func composeRequestPolicies(ps ...requestPolicy) requestPolicy {
	return func(ctx context.Context, filter nostr.Filter) (bool, string) {
		for _, p := range ps {
			if p == nil {
				continue
			}
			if reject, msg := p(ctx, filter); reject {
				return reject, msg
			}
		}
		return false, ""
	}
}

// valueEventPolicy adapts one of the successor khatru's value-based event
// policies to haven's pointer-based eventPolicy.
func valueEventPolicy(p func(context.Context, nostr.Event) (bool, string)) eventPolicy {
	return func(ctx context.Context, evt *nostr.Event) (bool, string) {
		return p(ctx, *evt)
	}
}

func composeEventPolicies(ps ...eventPolicy) func(context.Context, nostr.Event) (bool, string) {
	// the successor's OnEvent hands the event by value; haven's pointer-based
	// policies only read it, so a pointer to the copy is enough
	return func(ctx context.Context, evt nostr.Event) (bool, string) {
		for _, p := range ps {
			if p == nil {
				continue
			}
			if reject, msg := p(ctx, &evt); reject {
				return reject, msg
			}
		}
		return false, ""
	}
}

// uploadPolicy matches the successor blossom RejectUpload hook: auth event,
// declared size, declared extension, and a reject with an HTTP status code.
// The successor only reads the reject; a non-rejecting policy's ext and size
// passthroughs are ignored, but returning them keeps the policies composable.
type uploadPolicy = func(context.Context, *nostr.Event, int, string) (bool, string, int)

func composeUploadPolicies(ps ...uploadPolicy) uploadPolicy {
	return func(ctx context.Context, auth *nostr.Event, size int, ext string) (bool, string, int) {
		for _, p := range ps {
			if p == nil {
				continue
			}
			if reject, msg, code := p(ctx, auth, size, ext); reject {
				return reject, msg, code
			}
		}
		return false, "", 0
	}
}

func composeConnectionPolicies(ps ...connectionPolicy) connectionPolicy {
	return func(r *http.Request) bool {
		for _, p := range ps {
			if p == nil {
				continue
			}
			if p(r) {
				return true
			}
		}
		return false
	}
}

// relayQueryMaxLimit caps what the relay hands to strangers, the way the old
// backends' MaxLimit did (1500 on lmdb). The successor has no per-backend cap
// — the limit is passed per query — so the relay's query hook picks it, and
// public REQ handling stays as narrow as it was.
const relayQueryMaxLimit = 1500

// relayStoreHooks wires one DBBackend onto the successor relay's storage
// hooks. The successor passes events by value and deletes by id; the adapter
// in store.go turns those back into haven's pointer-and-context surface.
func relayStoreHooks(rl *khatru.Relay, db DBBackend) {
	rl.StoreEvent = func(ctx context.Context, evt nostr.Event) error {
		return db.SaveEvent(ctx, &evt)
	}
	relayStoreHooksQueryOnly(rl, db)
}

// relayStoreHooksQueryOnly wires the read-side hooks; the caller keeps its own
// StoreEvent when storing has a side effect (the outbox also blasts).
func relayStoreHooksQueryOnly(rl *khatru.Relay, db DBBackend) {
	rl.ReplaceEvent = func(ctx context.Context, evt nostr.Event) error {
		return db.ReplaceEvent(ctx, &evt)
	}
	rl.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		return db.DeleteEvent(ctx, &nostr.Event{ID: id})
	}
	rl.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		n, err := db.CountEvents(ctx, filter)
		return uint32(n), err
	}
	rl.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return db.QueryEventsIter(ctx, filter, relayQueryMaxLimit)
	}
}

// relayPubKey parses a relay's configured npub into the pointer the successor
// NIP-11 document carries. nPubToPubkey already log-fatals on a bad npub, so a
// failure here is unreachable in practice; the guard keeps the linter honest.
func relayPubKey(label, nPub string) *nostr.PubKey {
	pk, err := nostr.PubKeyFromHex(nPubToPubkey(label, nPub))
	if err != nil {
		log.Fatalf("invalid pubkey for %s: %v", label, err)
	}
	return &pk
}
