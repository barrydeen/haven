package dynamodb

import (
	"context"
	"fmt"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
)

func (d *DynamoDBBackend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	d.Lock()
	defer d.Unlock()

	filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
	if nostr.IsAddressableKind(evt.Kind) {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	ch, err := d.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to query before replacing: %w", err)
	}

	shouldStore := true
	for previous := range ch {
		if internal.IsOlder(previous, evt) {
			if err := d.DeleteEvent(ctx, previous); err != nil {
				return fmt.Errorf("failed to delete event for replacing: %w", err)
			}
		} else {
			shouldStore = false
		}
	}

	if shouldStore {
		if err := d.SaveEvent(ctx, evt); err != nil && err != eventstore.ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	return nil
}
