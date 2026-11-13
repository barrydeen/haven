package test

import (
	"github.com/nbd-wtf/go-nostr"
)

func getTimestamps(events []*nostr.Event) []nostr.Timestamp {
	res := make([]nostr.Timestamp, len(events))
	for i, evt := range events {
		res[i] = evt.CreatedAt
	}
	return res
}
