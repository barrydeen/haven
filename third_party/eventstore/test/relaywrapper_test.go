package test

import (
	"context"
	"testing"
	"time"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/slicestore"
	"github.com/nbd-wtf/go-nostr"
	"github.com/stretchr/testify/require"
)

var sk = "486d5f6d4891f4ce3cd5f4d6b62d184ec8ea10db455830ab7918ca43d4d7ad24"

func TestRelayWrapper(t *testing.T) {
	ctx := context.Background()

	s := &slicestore.SliceStore{}
	s.Init()
	defer s.Close()

	w := eventstore.RelayWrapper{Store: s}

	evt1 := nostr.Event{
		Kind:      3,
		CreatedAt: 0,
		Tags:      nostr.Tags{},
		Content:   "first",
	}
	evt1.Sign(sk)

	evt2 := nostr.Event{
		Kind:      3,
		CreatedAt: 1,
		Tags:      nostr.Tags{},
		Content:   "second",
	}
	evt2.Sign(sk)

	for range 200 {
		go w.Publish(ctx, evt1)
		go w.Publish(ctx, evt1)
	}
	time.Sleep(time.Millisecond * 200)

	evts, _ := w.QuerySync(ctx, nostr.Filter{Kinds: []int{3}})
	require.Len(t, evts, 1)
}
