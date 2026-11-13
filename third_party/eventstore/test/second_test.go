package test

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
	"github.com/stretchr/testify/require"
)

func runSecondTestOn(t *testing.T, db eventstore.Store) {
	db.Init()

	for i := 0; i < 10000; i++ {
		eTag := make([]byte, 32)
		binary.BigEndian.PutUint16(eTag, uint16(i))

		ref, _ := nostr.GetPublicKey(sk3)
		if i%3 == 0 {
			ref, _ = nostr.GetPublicKey(sk4)
		}

		evt := &nostr.Event{
			CreatedAt: nostr.Timestamp(i*10 + 2),
			Content:   fmt.Sprintf("hello %d", i),
			Tags: nostr.Tags{
				{"t", fmt.Sprintf("t%d", i)},
				{"e", hex.EncodeToString(eTag)},
				{"p", ref},
			},
			Kind: i % 10,
		}
		sk := sk3
		if i%3 == 0 {
			sk = sk4
		}
		evt.Sign(sk)
		err := db.SaveEvent(ctx, evt)
		require.NoError(t, err)
	}

	w := eventstore.RelayWrapper{Store: db}
	pk3, _ := nostr.GetPublicKey(sk3)
	pk4, _ := nostr.GetPublicKey(sk4)
	eTags := make([]string, 20)
	for i := 0; i < 20; i++ {
		eTag := make([]byte, 32)
		binary.BigEndian.PutUint16(eTag, uint16(i))
		eTags[i] = hex.EncodeToString(eTag)
	}

	filters := make([]nostr.Filter, 0, 10)
	filters = append(filters, nostr.Filter{Kinds: []int{1, 4, 8, 16}})
	filters = append(filters, nostr.Filter{Authors: []string{pk3, nostr.GeneratePrivateKey()}})
	filters = append(filters, nostr.Filter{Authors: []string{pk3, nostr.GeneratePrivateKey()}, Kinds: []int{3, 4}})
	filters = append(filters, nostr.Filter{})
	filters = append(filters, nostr.Filter{Limit: 20})
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3}}})
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3, pk4}}})
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3, pk4}}})
	filters = append(filters, nostr.Filter{Kinds: []int{9}, Tags: nostr.TagMap{"e": eTags}})
	filters = append(filters, nostr.Filter{Kinds: []int{5}, Tags: nostr.TagMap{"e": eTags, "t": []string{"t5"}}})
	filters = append(filters, nostr.Filter{Tags: nostr.TagMap{"e": eTags}})
	filters = append(filters, nostr.Filter{Tags: nostr.TagMap{"e": eTags}, Limit: 50})

	t.Run("filter", func(t *testing.T) {
		for q, filter := range filters {
			q := q
			filter := filter
			label := fmt.Sprintf("filter %d: %s", q, filter)

			t.Run(fmt.Sprintf("q-%d", q), func(t *testing.T) {
				results, err := w.QuerySync(ctx, filter)
				require.NoError(t, err, filter)
				require.NotEmpty(t, results, label)
			})
		}
	})
}
