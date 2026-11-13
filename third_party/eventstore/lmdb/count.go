package lmdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"

	"github.com/PowerDNS/lmdb-go/lmdb"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip45"
	"github.com/nbd-wtf/go-nostr/nip45/hyperloglog"
	"golang.org/x/exp/slices"
)

func (b *LMDBBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	var count int64 = 0

	queries, extraAuthors, extraKinds, extraTagKey, extraTagValues, since, err := b.prepareQueries(filter)
	if err != nil {
		return 0, err
	}

	err = b.lmdbEnv.View(func(txn *lmdb.Txn) error {
		// actually iterate
		for _, q := range queries {
			cursor, err := txn.OpenCursor(q.dbi)
			if err != nil {
				continue
			}

			it := &iterator{cursor: cursor}
			it.seek(q.startingPoint)

			for {
				// we already have a k and a v and an err from the cursor setup, so check and use these
				if it.err != nil ||
					len(it.key) != q.keySize ||
					!bytes.HasPrefix(it.key, q.prefix) {
					// either iteration has errored or we reached the end of this prefix
					break // stop this cursor and move to the next one
				}

				// "id" indexes don't contain a timestamp
				if q.timestampSize == 4 {
					createdAt := binary.BigEndian.Uint32(it.key[len(it.key)-4:])
					if createdAt < since {
						break
					}
				}

				if extraAuthors == nil && extraKinds == nil && extraTagValues == nil {
					count++
				} else {
					// fetch actual event
					val, err := txn.Get(b.rawEventStore, it.valIdx)
					if err != nil {
						panic(err)
					}

					// check it against pubkeys without decoding the entire thing.
					// a nil slice means this filter has no extra author constraint;
					// slices.Contains(nil, x) is false and would reject every row.
					if extraAuthors != nil && !slices.Contains(extraAuthors, [32]byte(val[32:64])) {
						it.next()
						continue
					}

					// check it against kinds without decoding the entire thing
					if extraKinds != nil && !slices.Contains(extraKinds, [2]byte(val[132:134])) {
						it.next()
						continue
					}

					evt := &nostr.Event{}
					if err := bin.Unmarshal(val, evt); err != nil {
						it.next()
						continue
					}

					// if there is still a tag to be checked, do it now
					if extraTagValues != nil && !evt.Tags.ContainsAny(extraTagKey, extraTagValues) {
						it.next()
						continue
					}

					count++
				}

				// Reject branches already advanced. A match must too, or this
				// loop repeats the same row forever.
				it.next()
			}
		}

		return nil
	})

	return count, err
}

// CountEventsHLL is like CountEvents, but it will build a hyperloglog value while iterating through results, following NIP-45
func (b *LMDBBackend) CountEventsHLL(ctx context.Context, filter nostr.Filter, offset int) (int64, *hyperloglog.HyperLogLog, error) {
	if useCache, _ := b.EnableHLLCacheFor(filter.Kinds[0]); useCache {
		return b.countEventsHLLCached(filter)
	}

	var count int64 = 0

	// this is different than CountEvents because some of these extra checks are not applicable in HLL-valid filters
	queries, _, extraKinds, extraTagKey, extraTagValues, since, err := b.prepareQueries(filter)
	if err != nil {
		return 0, nil, err
	}

	hll := hyperloglog.New(offset)

	err = b.lmdbEnv.View(func(txn *lmdb.Txn) error {
		// actually iterate
		for _, q := range queries {
			cursor, err := txn.OpenCursor(q.dbi)
			if err != nil {
				continue
			}

			it := &iterator{cursor: cursor}
			it.seek(q.startingPoint)

			for {
				// we already have a k and a v and an err from the cursor setup, so check and use these
				if it.err != nil ||
					len(it.key) != q.keySize ||
					!bytes.HasPrefix(it.key, q.prefix) {
					// either iteration has errored or we reached the end of this prefix
					break // stop this cursor and move to the next one
				}

				// "id" indexes don't contain a timestamp
				if q.timestampSize == 4 {
					createdAt := binary.BigEndian.Uint32(it.key[len(it.key)-4:])
					if createdAt < since {
						break
					}
				}

				// fetch actual event (we need it regardless because we need the pubkey for the hll)
				val, err := txn.Get(b.rawEventStore, it.valIdx)
				if err != nil {
					panic(err)
				}

				if extraKinds == nil && extraTagValues == nil {
					// nothing extra to check
					count++
					hll.AddBytes(val[32:64])
				} else {
					// check it against kinds without decoding the entire thing.
					// a nil slice means this filter has no extra kind constraint.
					if extraKinds != nil && !slices.Contains(extraKinds, [2]byte(val[132:134])) {
						it.next()
						continue
					}

					evt := &nostr.Event{}
					if err := bin.Unmarshal(val, evt); err != nil {
						it.next()
						continue
					}

					// if there is still a tag to be checked, do it now
					if extraTagValues != nil && !evt.Tags.ContainsAny(extraTagKey, extraTagValues) {
						it.next()
						continue
					}

					count++
					hll.Add(evt.PubKey)
				}

				// Reject branches already advanced. A match must too, or this
				// loop repeats the same row forever.
				it.next()
			}
		}

		return nil
	})

	return count, hll, err
}

// countEventsHLLCached will just return a cached value from disk (and presumably we don't even have the events required to compute this anymore).
func (b *LMDBBackend) countEventsHLLCached(filter nostr.Filter) (int64, *hyperloglog.HyperLogLog, error) {
	cacheKey := make([]byte, 2+8)
	binary.BigEndian.PutUint16(cacheKey[0:2], uint16(filter.Kinds[0]))
	switch filter.Kinds[0] {
	case 3:
		hex.Decode(cacheKey[2:2+8], []byte(filter.Tags["p"][0][0:8*2]))
	case 7:
		hex.Decode(cacheKey[2:2+8], []byte(filter.Tags["e"][0][0:8*2]))
	case 1111:
		hex.Decode(cacheKey[2:2+8], []byte(filter.Tags["E"][0][0:8*2]))
	}

	var count int64
	var hll *hyperloglog.HyperLogLog

	err := b.lmdbEnv.View(func(txn *lmdb.Txn) error {
		val, err := txn.Get(b.hllCache, cacheKey)
		if err != nil {
			if lmdb.IsNotFound(err) {
				return nil
			}
			return err
		}
		hll = hyperloglog.NewWithRegisters(val, 0) // offset doesn't matter here
		count = int64(hll.Count())
		return nil
	})

	return count, hll, err
}

func (b *LMDBBackend) updateHyperLogLogCachedValues(txn *lmdb.Txn, evt *nostr.Event) error {
	cacheKey := make([]byte, 2+8)
	binary.BigEndian.PutUint16(cacheKey[0:2], uint16(evt.Kind))

	for ref, offset := range nip45.HyperLogLogEventPubkeyOffsetsAndReferencesForEvent(evt) {
		// setup cache key (reusing buffer)
		hex.Decode(cacheKey[2:2+8], []byte(ref[0:8*2]))

		// fetch hll value from cache db
		hll := hyperloglog.New(offset)
		val, err := txn.Get(b.hllCache, cacheKey)
		if err == nil {
			hll.SetRegisters(val)
		} else if !lmdb.IsNotFound(err) {
			return err
		}

		// add this event
		hll.Add(evt.PubKey)

		// save values back again
		if err := txn.Put(b.hllCache, cacheKey, hll.GetRegisters(), 0); err != nil {
			return err
		}
	}

	return nil
}
