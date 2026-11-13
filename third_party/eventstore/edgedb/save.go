package edgedb

import (
	"context"
	"encoding/json"

	"github.com/edgedb/edgedb-go"
	"github.com/nbd-wtf/go-nostr"
)

func (b *EdgeDBBackend) SaveEvent(ctx context.Context, event *nostr.Event) error {
	tagsBytes := [][]byte{}
	for _, t := range event.Tags {
		tagBytes, err := json.Marshal(t)
		if err != nil {
			return err
		}
		tagsBytes = append(tagsBytes, tagBytes)
	}
	query := "INSERT events::Event { eventId := <str>$eventId, pubkey := <str>$pubkey, createdAt := <datetime>$createdAt, kind := <int64>$kind, tags := <array<json>>$tags, content := <str>$content, sig := <str>$sig }"
	args := map[string]interface{}{
		"eventId":   event.ID,
		"pubkey":    event.PubKey,
		"createdAt": edgedb.NewOptionalDateTime(event.CreatedAt.Time()),
		"kind":      int64(event.Kind),
		"tags":      tagsBytes,
		"content":   event.Content,
		"sig":       event.Sig,
	}
	return b.Client.QuerySingle(ctx, query, &Event{}, args)
}
