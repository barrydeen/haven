package lmdb

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/fiatjaf/eventstore"
)

var _ eventstore.Store = (*LMDBBackend)(nil)

type LMDBBackend struct {
	Path               string
	MaxLimit           int
	MaxLimitNegentropy int
	MapSize            int64

	lmdbEnv    *lmdb.Env
	extraFlags uint // (for debugging and testing)

	settingsStore   lmdb.DBI
	rawEventStore   lmdb.DBI
	indexCreatedAt  lmdb.DBI
	indexId         lmdb.DBI
	indexKind       lmdb.DBI
	indexPubkey     lmdb.DBI
	indexPubkeyKind lmdb.DBI
	indexTag        lmdb.DBI
	indexTag32      lmdb.DBI
	indexTagAddr    lmdb.DBI
	indexPTagKind   lmdb.DBI

	hllCache          lmdb.DBI
	EnableHLLCacheFor func(kind int) (useCache bool, skipSavingActualEvent bool)

	lastId atomic.Uint32
}

func (b *LMDBBackend) Init() error {
	if b.MaxLimit != 0 {
		b.MaxLimitNegentropy = b.MaxLimit
	} else {
		b.MaxLimit = 1500
		if b.MaxLimitNegentropy == 0 {
			b.MaxLimitNegentropy = 16777216
		}
	}

	// create directory if it doesn't exist and open it
	if err := os.MkdirAll(b.Path, 0755); err != nil {
		return err
	}

	return b.initialize()
}

func (b *LMDBBackend) Close() {
	b.lmdbEnv.Close()
}

func (b *LMDBBackend) Serial() []byte {
	v := b.lastId.Add(1)
	vb := make([]byte, 4)
	binary.BigEndian.PutUint32(vb[:], uint32(v))
	return vb
}

// Compact can only be called when the database is not being used because it will overwrite everything.
// It will temporarily move the database to a new location, then move it back.
// If something goes wrong crash the process and look for the copy of the data on tmppath.
func (b *LMDBBackend) Compact(tmppath string) error {
	if err := os.MkdirAll(tmppath, 0755); err != nil {
		return err
	}

	if err := b.lmdbEnv.Copy(tmppath); err != nil {
		return fmt.Errorf("failed to copy: %w", err)
	}

	if err := b.lmdbEnv.Close(); err != nil {
		return err
	}
	if err := os.RemoveAll(b.Path); err != nil {
		return err
	}
	if err := os.Rename(tmppath, b.Path); err != nil {
		return err
	}

	return b.initialize()
}

func (b *LMDBBackend) initialize() error {
	env, err := lmdb.NewEnv()
	if err != nil {
		return err
	}

	env.SetMaxDBs(12)
	env.SetMaxReaders(1000)
	if b.MapSize == 0 {
		env.SetMapSize(1 << 38) // ~273GB
	} else {
		env.SetMapSize(b.MapSize)
	}

	if err := env.Open(b.Path, lmdb.NoTLS|lmdb.WriteMap|b.extraFlags, 0644); err != nil {
		return err
	}
	b.lmdbEnv = env

	var multiIndexCreationFlags uint = lmdb.Create | lmdb.DupSort | lmdb.DupFixed

	// open each db
	if err := b.lmdbEnv.Update(func(txn *lmdb.Txn) error {
		if dbi, err := txn.OpenDBI("settings", lmdb.Create); err != nil {
			return err
		} else {
			b.settingsStore = dbi
		}
		if dbi, err := txn.OpenDBI("raw", lmdb.Create); err != nil {
			return err
		} else {
			b.rawEventStore = dbi
		}
		if dbi, err := txn.OpenDBI("created_at", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexCreatedAt = dbi
		}
		if dbi, err := txn.OpenDBI("id", lmdb.Create); err != nil {
			return err
		} else {
			b.indexId = dbi
		}
		if dbi, err := txn.OpenDBI("kind", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexKind = dbi
		}
		if dbi, err := txn.OpenDBI("pubkey", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexPubkey = dbi
		}
		if dbi, err := txn.OpenDBI("pubkeyKind", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexPubkeyKind = dbi
		}
		if dbi, err := txn.OpenDBI("tag", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexTag = dbi
		}
		if dbi, err := txn.OpenDBI("tag32", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexTag32 = dbi
		}
		if dbi, err := txn.OpenDBI("tagaddr", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexTagAddr = dbi
		}
		if dbi, err := txn.OpenDBI("ptagKind", multiIndexCreationFlags); err != nil {
			return err
		} else {
			b.indexPTagKind = dbi
		}
		if dbi, err := txn.OpenDBI("hllCache", lmdb.Create); err != nil {
			return err
		} else {
			b.hllCache = dbi
		}
		return nil
	}); err != nil {
		return err
	}

	// get lastId
	if err := b.lmdbEnv.View(func(txn *lmdb.Txn) error {
		txn.RawRead = true
		cursor, err := txn.OpenCursor(b.rawEventStore)
		if err != nil {
			return err
		}
		defer cursor.Close()
		k, _, err := cursor.Get(nil, nil, lmdb.Last)
		if lmdb.IsNotFound(err) {
			// nothing found, so we're at zero
			return nil
		}
		if err != nil {
			return err
		}
		b.lastId.Store(binary.BigEndian.Uint32(k))

		return nil
	}); err != nil {
		return err
	}

	return b.runMigrations()
}
