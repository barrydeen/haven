package skipevent

import (
	"context"

	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
)

type Wrapper struct {
	eventstore.Store

	Skip func(ctx context.Context, evt *nostr.Event) bool
}

var _ eventstore.Store = (*Wrapper)(nil)

func (w Wrapper) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	if w.Skip(ctx, evt) {
		return nil
	}

	return w.Store.SaveEvent(ctx, evt)
}
