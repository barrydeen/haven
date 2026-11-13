package badger

import (
	"context"
	"encoding/hex"
	"log"

	"github.com/dgraph-io/badger/v4"
	"github.com/nbd-wtf/go-nostr"
)

var serialDelete uint32 = 0

func (b *BadgerBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	deletionHappened := false

	err := b.Update(func(txn *badger.Txn) error {
		var err error
		deletionHappened, err = b.delete(txn, evt)
		return err
	})
	if err != nil {
		return err
	}

	// after deleting, run garbage collector (sometimes)
	if deletionHappened {
		serialDelete = (serialDelete + 1) % 256
		if serialDelete == 0 {
			if err := b.RunValueLogGC(0.8); err != nil && err != badger.ErrNoRewrite {
				log.Println("badger gc errored:" + err.Error())
			}
		}
	}

	return nil
}

func (b *BadgerBackend) delete(txn *badger.Txn, evt *nostr.Event) (bool, error) {
	idx := make([]byte, 1, 5)
	idx[0] = rawEventStorePrefix

	// query event by id to get its idx
	idPrefix8, _ := hex.DecodeString(evt.ID[0 : 8*2])
	prefix := make([]byte, 1+8)
	prefix[0] = indexIdPrefix
	copy(prefix[1:], idPrefix8)
	opts := badger.IteratorOptions{
		PrefetchValues: false,
	}
	it := txn.NewIterator(opts)
	it.Seek(prefix)
	if it.ValidForPrefix(prefix) {
		idx = append(idx, it.Item().Key()[1+8:]...)
	}
	it.Close()

	// if no idx was found, end here, this event doesn't exist
	if len(idx) == 1 {
		return false, nil
	}

	// calculate all index keys we have for this event and delete them
	for k := range b.getIndexKeysForEvent(evt, idx[1:]) {
		if err := txn.Delete(k); err != nil {
			return false, err
		}
	}

	// delete the raw event
	return true, txn.Delete(idx)
}
