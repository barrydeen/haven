package bluge

import (
	"context"
	"fmt"
	"strconv"

	"github.com/blugelabs/bluge"
	"github.com/nbd-wtf/go-nostr"
)

func (b *BlugeBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	id := eventIdentifier(evt.ID)
	doc := &bluge.Document{
		bluge.NewKeywordFieldBytes(id.Field(), id.Term()).Sortable().StoreValue(),
	}

	doc.AddField(bluge.NewTextField(contentField, evt.Content))
	doc.AddField(bluge.NewTextField(kindField, strconv.Itoa(evt.Kind)))
	doc.AddField(bluge.NewTextField(pubkeyField, evt.PubKey[56:]))
	doc.AddField(bluge.NewNumericField(createdAtField, float64(evt.CreatedAt)))

	if err := b.writer.Update(doc.ID(), doc); err != nil {
		return fmt.Errorf("failed to write '%s' document: %w", evt.ID, err)
	}

	return nil
}
