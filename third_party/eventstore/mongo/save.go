package mongo

import (
	"context"
	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
)

func (m *MongoDBBackend) SaveEvent(ctx context.Context, event *nostr.Event) error {
	result, err := m.Client.Database("events").Collection("events").InsertOne(ctx, event)
	if err != nil {
		return err
	}
	if result.InsertedID == nil {
		return eventstore.ErrDupEvent
	}
	return err
}
