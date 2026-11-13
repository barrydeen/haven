package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/nbd-wtf/go-nostr"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"log"
)

var (
	ErrTooManyIDs       = errors.New("too many ids")
	ErrTooManyAuthors   = errors.New("too many authors")
	ErrTooManyKinds     = errors.New("too many kinds")
	ErrEmptyTagSet      = errors.New("empty tag set")
	ErrTooManyTagValues = errors.New("too many tag values")
)

func (m *MongoDBBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {

	conditions, projections, err := m.queryEvents(filter, false)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if filter.Limit < 1 || filter.Limit > m.QueryLimit {
		limit = m.QueryLimit
	}
	cursor, err := m.Client.Database("events").Collection("events").Find(ctx, conditions, options.Find().SetProjection(projections).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}

	ch := make(chan *nostr.Event)
	go func() {
		defer cursor.Close(ctx)
		defer close(ch)
		for cursor.Next(ctx) {
			var evt nostr.Event
			var raw bson.M
			if err := cursor.Decode(&raw); err != nil {
				log.Printf("Error decoding event: %v", err)
				return
			}
			evt.ID = raw["id"].(string)
			evt.Kind = int(raw["kind"].(int32))
			evt.Content = raw["content"].(string)
			evt.Sig = raw["sig"].(string)
			evt.PubKey = raw["pubkey"].(string)
			jsonData, err := json.Marshal(raw["tags"])
			if err != nil {
				log.Printf("Error encoding tags: %v", err)
				return
			}
			if err := evt.Tags.Scan(jsonData); err != nil {
				log.Printf("Error parsing tags: %v", err)
				return
			}
			evt.CreatedAt = nostr.Timestamp(raw["createdat"].(int64))
			select {
			case ch <- &evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (m *MongoDBBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	var count int64
	conditions, projection, err := m.queryEvents(filter, true)
	if err != nil {
		return 0, err
	}
	if projection == nil {
		count, err = m.Client.Database("events").Collection("events").CountDocuments(ctx, conditions)
		if err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (m *MongoDBBackend) queryEvents(filter nostr.Filter, doCount bool) (bson.D, bson.D, error) {
	var conditions bson.D
	if len(filter.IDs) > 0 {
		if len(filter.IDs) > m.QueryIDsLimit {
			// too many ids, fail everything
			return nil, nil, ErrTooManyIDs
		}
		conditions = append(conditions, bson.E{Key: "id", Value: bson.M{"$in": filter.IDs}})
	}
	if len(filter.Authors) > 0 {
		if len(filter.Authors) > m.QueryAuthorsLimit {
			// too many authors, fail everything
			return nil, nil, ErrTooManyAuthors
		}
		conditions = append(conditions, bson.E{Key: "pubkey", Value: bson.M{"$in": filter.Authors}})
	}

	if len(filter.Kinds) > 0 {
		if len(filter.Kinds) > m.QueryKindsLimit {
			// too many kinds, fail everything
			return nil, nil, ErrTooManyKinds
		}
		conditions = append(conditions, bson.E{Key: "kind", Value: bson.M{"$in": filter.Kinds}})
	}

	totalTags := 0
	for _, values := range filter.Tags {
		if len(values) == 0 {
			// any tag set to [] is wrong
			return nil, nil, ErrEmptyTagSet
		}
		var tags bson.A
		for _, tagValue := range values {
			tags = append(tags, tagValue)
		}

		conditions = append(conditions, bson.E{Key: "colors", Value: bson.M{"$in": tags}})

		totalTags += len(values)
		if totalTags > m.QueryTagsLimit {
			// too many tags, fail everything
			return nil, nil, ErrTooManyTagValues
		}
	}
	if filter.Since != nil {
		conditions = append(conditions, bson.E{Key: "since", Value: bson.M{"$gte": filter.Since}})
	}
	if filter.Until != nil {
		conditions = append(conditions, bson.E{Key: "until", Value: bson.M{"$lte": filter.Until}})
	}
	if filter.Search != "" {
		conditions = append(conditions, bson.E{Key: "search", Value: bson.M{"$regex": filter.Search}})
	}

	if len(conditions) == 0 {
		conditions = append(conditions, bson.E{})
	}

	var projections bson.D
	if doCount {
		projections = nil
	} else {
		projections = bson.D{{"id", 1}, {"pubkey", 1}, {"createdat", 1}, {"kind", 1}, {"tags", 1}, {"content", 1}, {"sig", 1}}
	}
	return conditions, projections, nil
}
