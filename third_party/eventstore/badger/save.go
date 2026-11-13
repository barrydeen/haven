package badger

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/dgraph-io/badger/v4"
	"github.com/fiatjaf/eventstore"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

func (b *BadgerBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	// sanity checking
	if evt.CreatedAt > math.MaxUint32 || evt.Kind > math.MaxUint16 {
		return fmt.Errorf("event with values out of expected boundaries")
	}

	return b.Update(func(txn *badger.Txn) error {
		// query event by id to ensure we don't save duplicates
		id, _ := hex.DecodeString(evt.ID)
		prefix := make([]byte, 1+8)
		prefix[0] = indexIdPrefix
		copy(prefix[1:], id)
		it := txn.NewIterator(badger.IteratorOptions{})
		defer it.Close()
		it.Seek(prefix)
		if it.ValidForPrefix(prefix) {
			// event exists
			return eventstore.ErrDupEvent
		}

		return b.save(txn, evt)
	})
}

func (b *BadgerBackend) save(txn *badger.Txn, evt *nostr.Event) error {
	// encode to binary
	bin, err := bin.Marshal(evt)
	if err != nil {
		return err
	}

	idx := b.Serial()
	// raw event store
	if err := txn.Set(idx, bin); err != nil {
		return err
	}

	for k := range b.getIndexKeysForEvent(evt, idx[1:]) {
		if err := txn.Set(k, nil); err != nil {
			return err
		}
	}

	return nil
}
