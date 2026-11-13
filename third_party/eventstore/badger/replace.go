package badger

import (
	"context"
	"fmt"
	"math"

	"github.com/dgraph-io/badger/v4"
	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
)

func (b *BadgerBackend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	// sanity checking
	if evt.CreatedAt > math.MaxUint32 || evt.Kind > math.MaxUint16 {
		return fmt.Errorf("event with values out of expected boundaries")
	}

	return b.Update(func(txn *badger.Txn) error {
		filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
		if nostr.IsAddressableKind(evt.Kind) {
			// when addressable, add the "d" tag to the filter
			filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
		}

		// now we fetch the past events, whatever they are, delete them and then save the new
		results, err := b.query(txn, filter, 10) // in theory limit could be just 1 and this should work
		if err != nil {
			return fmt.Errorf("failed to query past events with %s: %w", filter, err)
		}

		shouldStore := true
		for _, previous := range results {
			if internal.IsOlder(previous.Event, evt) {
				if _, err := b.delete(txn, previous.Event); err != nil {
					return fmt.Errorf("failed to delete event %s for replacing: %w", previous.Event.ID, err)
				}
			} else {
				// there is a newer event already stored, so we won't store this
				shouldStore = false
			}
		}
		if shouldStore {
			return b.save(txn, evt)
		}

		return nil
	})
}
