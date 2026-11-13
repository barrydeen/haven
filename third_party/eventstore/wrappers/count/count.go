package count

import (
	"context"

	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
)

type Wrapper struct {
	eventstore.Store
}

var _ eventstore.Store = (*Wrapper)(nil)

func (w Wrapper) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	if counter, ok := w.Store.(eventstore.Counter); ok {
		return counter.CountEvents(ctx, filter)
	}

	ch, err := w.Store.QueryEvents(ctx, filter)
	if err != nil {
		return 0, err
	}
	if ch == nil {
		return 0, nil
	}

	var count int64
	for range ch {
		count++
	}
	return count, nil
}
