package main

import (
	"context"
	"fmt"
	"log/slog"

	"fiatjaf.com/nostr"
)

// isDeleted reports whether db holds a NIP-09 delete request covering the event.
//
// khatru runs this check itself before storing an event, but its filters ask for
// the tag keys "#e" and "#a" where go-nostr expects "e" and "a", so they never
// match anything and a deleted event comes straight back on the next publish.
// The import paths write to the databases directly and don't check at all.
func isDeleted(ctx context.Context, db DBBackend, event *nostr.Event) bool {
	// only the author of an event and the relay owner can delete it, so a delete
	// request from anybody else doesn't count
	ownerKey, err := nostr.PubKeyFromHex(config.OwnerPubKey)
	if err != nil {
		slog.Error("🚫 owner pubkey is not valid hex; delete requests cannot be matched", "error", err)
		return false
	}
	authors := []nostr.PubKey{event.PubKey, ownerKey}

	filters := []nostr.Filter{{
		Kinds:   []nostr.Kind{nostr.KindDeletion},
		Authors: authors,
		Tags:    nostr.TagMap{"e": []string{event.ID.Hex()}},
		Limit:   1,
	}}

	// replaceable and addressable events are also deleted by address, and such a
	// request only covers the versions that existed when it was made
	if !event.Kind.IsRegular() {
		address := fmt.Sprintf("%d:%s:%s", event.Kind, event.PubKey.Hex(), event.Tags.GetD())
		filters = append(filters, nostr.Filter{
			Kinds:   []nostr.Kind{nostr.KindDeletion},
			Authors: authors,
			Tags:    nostr.TagMap{"a": []string{address}},
			Since:   event.CreatedAt,
			Limit:   1,
		})
	}

	for _, filter := range filters {
		ch, err := db.QueryEvents(ctx, filter)
		if err != nil {
			slog.Error("🚫 error looking for delete requests", "event", event.ID, "error", err)
			continue
		}

		deleted := false
		for range ch {
			deleted = true
		}
		if deleted {
			return true
		}
	}

	return false
}
