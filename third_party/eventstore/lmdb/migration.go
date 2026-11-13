package lmdb

import (
	"encoding/binary"
	"fmt"
	"log"

	"github.com/PowerDNS/lmdb-go/lmdb"
	bin "github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

const (
	DB_VERSION byte = 'v'
)

func (b *LMDBBackend) runMigrations() error {
	return b.lmdbEnv.Update(func(txn *lmdb.Txn) error {
		var version uint16
		v, err := txn.Get(b.settingsStore, []byte{DB_VERSION})
		if err != nil {
			if lmdb.IsNotFound(err) {
				version = 0
			} else if v == nil {
				return fmt.Errorf("failed to read database version: %w", err)
			}
		} else {
			version = binary.BigEndian.Uint16(v)
		}

		// all previous migrations are useless because we will just reindex everything
		if version < 9 {
			log.Println("[lmdb] migration 9: reindex everything")

			if err := txn.Drop(b.indexId, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexCreatedAt, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexKind, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexPTagKind, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexPubkey, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexPubkeyKind, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexTag, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexTag32, false); err != nil {
				return err
			}
			if err := txn.Drop(b.indexTagAddr, false); err != nil {
				return err
			}

			cursor, err := txn.OpenCursor(b.rawEventStore)
			if err != nil {
				return fmt.Errorf("failed to open cursor in migration 9: %w", err)
			}
			defer cursor.Close()

			idx, val, err := cursor.Get(nil, nil, lmdb.First)
			for err == nil {
				evt := &nostr.Event{}
				if err := bin.Unmarshal(val, evt); err != nil {
					return fmt.Errorf("error decoding event %x on migration 5: %w", idx, err)
				}

				for key := range b.getIndexKeysForEvent(evt) {
					if err := txn.Put(key.dbi, key.key, idx, 0); err != nil {
						return fmt.Errorf("failed to save index %s for event %s (%v) on migration 9: %w",
							b.keyName(key), evt.ID, idx, err)
					}
				}

				// next
				idx, val, err = cursor.Get(nil, nil, lmdb.Next)
			}
			if lmdbErr, ok := err.(*lmdb.OpError); ok && lmdbErr.Errno != lmdb.NotFound {
				// exited the loop with an error different from NOTFOUND
				return err
			}

			// bump version
			if err := b.setVersion(txn, 9); err != nil {
				return err
			}
		}

		return nil
	})
}

func (b *LMDBBackend) setVersion(txn *lmdb.Txn, version uint16) error {
	buf, err := txn.PutReserve(b.settingsStore, []byte{DB_VERSION}, 4, 0)
	binary.BigEndian.PutUint16(buf, version)
	return err
}
