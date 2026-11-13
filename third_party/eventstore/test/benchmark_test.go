package test

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/eventstore/slicestore"
	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/nbd-wtf/go-nostr"
)

func BenchmarkSliceStore(b *testing.B) {
	s := &slicestore.SliceStore{}
	s.Init()
	runBenchmarkOn(b, s)
}

func BenchmarkLMDB(b *testing.B) {
	os.RemoveAll(dbpath + "lmdb")
	l := &lmdb.LMDBBackend{Path: dbpath + "lmdb"}
	l.Init()

	runBenchmarkOn(b, l)
}

func BenchmarkBadger(b *testing.B) {
	d := &badger.BadgerBackend{Path: dbpath + "badger"}
	d.Init()
	runBenchmarkOn(b, d)
}

func BenchmarkSQLite(b *testing.B) {
	os.RemoveAll(dbpath + "sqlite")
	q := &sqlite3.SQLite3Backend{DatabaseURL: dbpath + "sqlite", QueryTagsLimit: 50}
	q.Init()

	runBenchmarkOn(b, q)
}

func runBenchmarkOn(b *testing.B, db eventstore.Store) {
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
		db.SaveEvent(ctx, evt)
	}

	filters := make([]nostr.Filter, 0, 10)
	filters = append(filters, nostr.Filter{Kinds: []int{1, 4, 8, 16}})
	pk3, _ := nostr.GetPublicKey(sk3)
	filters = append(filters, nostr.Filter{Authors: []string{pk3, nostr.GeneratePrivateKey()}})
	filters = append(filters, nostr.Filter{Authors: []string{pk3, nostr.GeneratePrivateKey()}, Kinds: []int{3, 4}})
	filters = append(filters, nostr.Filter{})
	filters = append(filters, nostr.Filter{Limit: 20})
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3}}})
	pk4, _ := nostr.GetPublicKey(sk4)
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3, pk4}}})
	filters = append(filters, nostr.Filter{Kinds: []int{8, 9}, Tags: nostr.TagMap{"p": []string{pk3, pk4}}})
	eTags := make([]string, 20)
	for i := 0; i < 20; i++ {
		eTag := make([]byte, 32)
		binary.BigEndian.PutUint16(eTag, uint16(i))
		eTags[i] = hex.EncodeToString(eTag)
	}
	filters = append(filters, nostr.Filter{Kinds: []int{9}, Tags: nostr.TagMap{"e": eTags}})
	filters = append(filters, nostr.Filter{Kinds: []int{5}, Tags: nostr.TagMap{"e": eTags, "t": []string{"t5"}}})
	filters = append(filters, nostr.Filter{Tags: nostr.TagMap{"e": eTags}})
	filters = append(filters, nostr.Filter{Tags: nostr.TagMap{"e": eTags}, Limit: 50})

	b.Run("filter", func(b *testing.B) {
		for q, filter := range filters {
			b.Run(fmt.Sprintf("q-%d", q), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					_, _ = db.QueryEvents(ctx, filter)
				}
			})
		}
	})

	b.Run("insert", func(b *testing.B) {
		evt := &nostr.Event{Kind: 788, CreatedAt: nostr.Now(), Content: "blergh", Tags: nostr.Tags{{"t", "spam"}}}
		evt.Sign(sk4)
		for i := 0; i < b.N; i++ {
			db.SaveEvent(ctx, evt)
		}
	})
}
