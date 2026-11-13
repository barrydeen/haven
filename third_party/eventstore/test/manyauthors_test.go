package test

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"

	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
	"github.com/stretchr/testify/require"
)

func manyAuthorsTest(t *testing.T, db eventstore.Store) {
	db.Init()

	const total = 10000
	const limit = 500
	const authors = 1700
	kinds := []int{6, 7, 8}

	bigfilter := nostr.Filter{
		Authors: make([]string, authors),
		Kinds:   kinds,
		Limit:   limit,
	}
	for i := 0; i < authors; i++ {
		sk := make([]byte, 32)
		binary.BigEndian.PutUint32(sk, uint32(i%(total/5))+1)
		pk, _ := nostr.GetPublicKey(hex.EncodeToString(sk))
		bigfilter.Authors[i] = pk
	}

	ordered := make([]*nostr.Event, 0, total)
	for i := 0; i < total; i++ {
		sk := make([]byte, 32)
		binary.BigEndian.PutUint32(sk, uint32(i%(total/5))+1)

		evt := &nostr.Event{
			CreatedAt: nostr.Timestamp(i*i) / 4,
			Content:   fmt.Sprintf("lots of stuff %d", i),
			Tags:      nostr.Tags{},
			Kind:      i % 10,
		}
		err := evt.Sign(hex.EncodeToString(sk))
		require.NoError(t, err)

		err = db.SaveEvent(ctx, evt)
		require.NoError(t, err)

		if bigfilter.Matches(evt) {
			ordered = append(ordered, evt)
		}
	}

	w := eventstore.RelayWrapper{Store: db}

	res, err := w.QuerySync(ctx, bigfilter)

	require.NoError(t, err)
	require.Len(t, res, limit)
	require.True(t, slices.IsSortedFunc(res, nostr.CompareEventPtrReverse))
	slices.SortFunc(ordered, nostr.CompareEventPtrReverse)
	require.Equal(t, ordered[0], res[0])
	require.Equal(t, ordered[limit-1], res[limit-1])
	require.Equal(t, ordered[0:limit], res)
}
