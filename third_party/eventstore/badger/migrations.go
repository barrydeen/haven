package badger

import (
	"encoding/binary"
	"fmt"
	"log"

	"github.com/dgraph-io/badger/v4"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

func (b *BadgerBackend) runMigrations() error {
	return b.Update(func(txn *badger.Txn) error {
		var version uint16

		item, err := txn.Get([]byte{dbVersionKey})
		if err == badger.ErrKeyNotFound {
			version = 0
		} else if err != nil {
			return err
		} else {
			item.Value(func(val []byte) error {
				version = binary.BigEndian.Uint16(val)
				return nil
			})
		}

		// do the migrations in increasing steps (there is no rollback)
		//

		// the 5 first migrations go to trash because on version 5 we need to export and import all the data anyway
		if version < 5 {
			log.Println("[badger] migration 5: delete all indexes and recreate them")

			// delete all index entries
			prefixes := []byte{
				indexIdPrefix,
				indexCreatedAtPrefix,
				indexKindPrefix,
				indexPubkeyPrefix,
				indexPubkeyKindPrefix,
				indexTagPrefix,
				indexTag32Prefix,
				indexTagAddrPrefix,
			}

			for _, prefix := range prefixes {
				it := txn.NewIterator(badger.IteratorOptions{
					PrefetchValues: false,
					Prefix:         []byte{prefix},
				})
				defer it.Close()

				var keysToDelete [][]byte
				for it.Seek([]byte{prefix}); it.ValidForPrefix([]byte{prefix}); it.Next() {
					key := it.Item().Key()
					keyCopy := make([]byte, len(key))
					copy(keyCopy, key)
					keysToDelete = append(keysToDelete, keyCopy)
				}

				for _, key := range keysToDelete {
					if err := txn.Delete(key); err != nil {
						return fmt.Errorf("failed to delete index key %x: %w", key, err)
					}
				}
			}

			// iterate through all events and recreate indexes
			it := txn.NewIterator(badger.IteratorOptions{
				PrefetchValues: true,
				Prefix:         []byte{rawEventStorePrefix},
			})
			defer it.Close()

			for it.Seek([]byte{rawEventStorePrefix}); it.ValidForPrefix([]byte{rawEventStorePrefix}); it.Next() {
				item := it.Item()
				idx := item.Key()

				err := item.Value(func(val []byte) error {
					evt := &nostr.Event{}
					if err := bin.Unmarshal(val, evt); err != nil {
						return fmt.Errorf("error decoding event %x on migration 5: %w", idx, err)
					}

					for key := range b.getIndexKeysForEvent(evt, idx[1:]) {
						if err := txn.Set(key, nil); err != nil {
							return fmt.Errorf("failed to save index for event %s on migration 5: %w", evt.ID, err)
						}
					}

					return nil
				})
				if err != nil {
					return err
				}
			}

			// bump version
			if err := b.bumpVersion(txn, 5); err != nil {
				return err
			}
		}

		return nil
	})
}

func (b *BadgerBackend) bumpVersion(txn *badger.Txn, version uint16) error {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, version)
	return txn.Set([]byte{dbVersionKey}, buf)
}
