package lmdb

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/fiatjaf/eventstore"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

func (b *LMDBBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	// sanity checking
	if evt.CreatedAt > math.MaxUint32 || evt.Kind > math.MaxUint16 {
		return fmt.Errorf("event with values out of expected boundaries")
	}

	return b.lmdbEnv.Update(func(txn *lmdb.Txn) error {
		if b.EnableHLLCacheFor != nil {
			// modify hyperloglog caches relative to this
			useCache, skipSaving := b.EnableHLLCacheFor(evt.Kind)

			if useCache {
				err := b.updateHyperLogLogCachedValues(txn, evt)
				if err != nil {
					return fmt.Errorf("failed to update hll cache: %w", err)
				}
				if skipSaving {
					return nil
				}
			}
		}

		// check if we already have this id
		id, _ := hex.DecodeString(evt.ID)
		_, err := txn.Get(b.indexId, id[0:8])
		if operr, ok := err.(*lmdb.OpError); ok && operr.Errno != lmdb.NotFound {
			// we will only proceed if we get a NotFound
			return eventstore.ErrDupEvent
		}

		return b.save(txn, evt)
	})
}

func (b *LMDBBackend) save(txn *lmdb.Txn, evt *nostr.Event) error {
	// encode to binary form so we'll save it
	bin, err := bin.Marshal(evt)
	if err != nil {
		return err
	}

	idx := b.Serial()
	// raw event store
	if err := txn.Put(b.rawEventStore, idx, bin, 0); err != nil {
		return err
	}

	// put indexes
	for k := range b.getIndexKeysForEvent(evt) {
		err := txn.Put(k.dbi, k.key, idx, 0)
		if err != nil {
			return err
		}
	}

	return nil
}
