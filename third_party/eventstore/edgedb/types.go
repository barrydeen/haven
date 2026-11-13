package edgedb

import (
	"encoding/json"

	"github.com/edgedb/edgedb-go"
	"github.com/nbd-wtf/go-nostr"
)

type Event struct {
	ID        edgedb.UUID             `edgedb:"id"`
	EventID   string                  `edgedb:"eventId"`
	Pubkey    string                  `edgedb:"pubkey"`
	CreatedAt edgedb.OptionalDateTime `edgedb:"createdAt"`
	Kind      int64                   `edgedb:"kind"`
	Tags      [][]byte                `edgedb:"tags"`
	Content   edgedb.OptionalStr      `edgedb:"content"`
	Sig       string                  `edgedb:"sig"`
}

// NostrEventToEdgeDBEvent converts the event from the nostr.Event datatype to edgedb.Event
func NostrEventToEdgeDBEvent(event *nostr.Event) (Event, error) {
	tagsBytes := [][]byte{}
	for _, t := range event.Tags {
		tagBytes, err := json.Marshal(t)
		if err != nil {
			return Event{}, err
		}
		tagsBytes = append(tagsBytes, tagBytes)
	}
	return Event{
		EventID:   event.ID,
		Pubkey:    event.PubKey,
		CreatedAt: edgedb.NewOptionalDateTime(event.CreatedAt.Time()),
		Kind:      int64(event.Kind),
		Tags:      tagsBytes, // NewOptionalTags(tagsBytes),
		Content:   edgedb.NewOptionalStr(event.Content),
		Sig:       event.Sig,
	}, nil
}

// EdgeDBEventToNostrEvent converts the event from the edgedb.Event datatype to nostr.Event
func EdgeDBEventToNostrEvent(event Event) (*nostr.Event, error) {
	tags := nostr.Tags{}
	for _, tagBytes := range event.Tags {
		var tag nostr.Tag
		if err := json.Unmarshal(tagBytes, &tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	createdAt, _ := event.CreatedAt.Get()
	content, _ := event.Content.Get()
	return &nostr.Event{
		ID:        event.EventID,
		PubKey:    event.Pubkey,
		CreatedAt: nostr.Timestamp(createdAt.Unix()),
		Kind:      int(event.Kind),
		Tags:      tags,
		Content:   content,
		Sig:       event.Sig,
	}, nil
}
