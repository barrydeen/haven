package mongo

import (
	"context"
	"github.com/nbd-wtf/go-nostr"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (m *MongoDBBackend) DeleteEvent(ctx context.Context, event *nostr.Event) error {
	_, err := m.Client.Database("events").Collection("events").DeleteOne(ctx, bson.M{
		"id": event.ID,
	})
	return err
}
