package bluge

import (
	"context"

	"github.com/nbd-wtf/go-nostr"
)

func (b *BlugeBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	return b.writer.Delete(eventIdentifier(evt.ID))
}
