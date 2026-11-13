package badger

import (
	"context"
	"encoding/binary"
	"log"

	"github.com/dgraph-io/badger/v4"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip45/hyperloglog"
)

func (b *BadgerBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	var count int64 = 0

	queries, extraFilter, since, err := prepareQueries(filter)
	if err != nil {
		return 0, err
	}

	err = b.View(func(txn *badger.Txn) error {
		// iterate only through keys and in reverse order
		opts := badger.IteratorOptions{
			Reverse: true,
		}

		// actually iterate
		for _, q := range queries {
			it := txn.NewIterator(opts)
			defer it.Close()

			for it.Seek(q.startingPoint); it.ValidForPrefix(q.prefix); it.Next() {
				item := it.Item()
				key := item.Key()

				idxOffset := len(key) - 4 // this is where the idx actually starts

				// "id" indexes don't contain a timestamp
				if !q.skipTimestamp {
					createdAt := binary.BigEndian.Uint32(key[idxOffset-4 : idxOffset])
					if createdAt < since {
						break
					}
				}

				idx := make([]byte, 5)
				idx[0] = rawEventStorePrefix
				copy(idx[1:], key[idxOffset:])

				if extraFilter == nil {
					count++
				} else {
					// fetch actual event
					item, err := txn.Get(idx)
					if err != nil {
						if err == badger.ErrDiscardedTxn {
							return err
						}
						log.Printf("badger: count (%v) failed to get %d from raw event store: %s\n", q, idx, err)
						return err
					}

					err = item.Value(func(val []byte) error {
						evt := &nostr.Event{}
						if err := bin.Unmarshal(val, evt); err != nil {
							return err
						}

						// check if this matches the other filters that were not part of the index
						if extraFilter.Matches(evt) {
							count++
						}

						return nil
					})
					if err != nil {
						log.Printf("badger: count value read error: %s\n", err)
					}
				}
			}
		}

		return nil
	})

	return count, err
}

func (b *BadgerBackend) CountEventsHLL(ctx context.Context, filter nostr.Filter, offset int) (int64, *hyperloglog.HyperLogLog, error) {
	var count int64 = 0

	queries, extraFilter, since, err := prepareQueries(filter)
	if err != nil {
		return 0, nil, err
	}

	hll := hyperloglog.New(offset)

	err = b.View(func(txn *badger.Txn) error {
		// iterate only through keys and in reverse order
		opts := badger.IteratorOptions{
			Reverse: true,
		}

		// actually iterate
		for _, q := range queries {
			it := txn.NewIterator(opts)
			defer it.Close()

			for it.Seek(q.startingPoint); it.ValidForPrefix(q.prefix); it.Next() {
				item := it.Item()
				key := item.Key()

				idxOffset := len(key) - 4 // this is where the idx actually starts

				// "id" indexes don't contain a timestamp
				if !q.skipTimestamp {
					createdAt := binary.BigEndian.Uint32(key[idxOffset-4 : idxOffset])
					if createdAt < since {
						break
					}
				}

				idx := make([]byte, 5)
				idx[0] = rawEventStorePrefix
				copy(idx[1:], key[idxOffset:])

				// fetch actual event
				item, err := txn.Get(idx)
				if err != nil {
					if err == badger.ErrDiscardedTxn {
						return err
					}
					log.Printf("badger: count (%v) failed to get %d from raw event store: %s\n", q, idx, err)
					return err
				}

				err = item.Value(func(val []byte) error {
					if extraFilter == nil {
						hll.AddBytes(val[32:64])
						count++
						return nil
					}

					evt := &nostr.Event{}
					if err := bin.Unmarshal(val, evt); err != nil {
						return err
					}
					if extraFilter.Matches(evt) {
						hll.Add(evt.PubKey)
						count++
						return nil
					}

					return nil
				})
				if err != nil {
					log.Printf("badger: count value read error: %s\n", err)
				}
			}
		}

		return nil
	})

	return count, hll, err
}
