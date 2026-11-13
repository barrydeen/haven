package badger

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"

	"github.com/dgraph-io/badger/v4"
	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
)

const (
	dbVersionKey          byte = 255
	rawEventStorePrefix   byte = 0
	indexCreatedAtPrefix  byte = 1
	indexIdPrefix         byte = 2
	indexKindPrefix       byte = 3
	indexPubkeyPrefix     byte = 4
	indexPubkeyKindPrefix byte = 5
	indexTagPrefix        byte = 6
	indexTag32Prefix      byte = 7
	indexTagAddrPrefix    byte = 8
)

var _ eventstore.Store = (*BadgerBackend)(nil)

type BadgerBackend struct {
	Path                  string
	MaxLimit              int
	MaxLimitNegentropy    int
	BadgerOptionsModifier func(badger.Options) badger.Options

	// Experimental
	SkipIndexingTag func(event *nostr.Event, tagName string, tagValue string) bool
	// Experimental
	IndexLongerTag func(event *nostr.Event, tagName string, tagValue string) bool

	*badger.DB

	serial atomic.Uint32
}

func (b *BadgerBackend) Init() error {
	opts := badger.DefaultOptions(b.Path)
	if b.BadgerOptionsModifier != nil {
		opts = b.BadgerOptionsModifier(opts)
	}

	db, err := badger.Open(opts)
	if err != nil {
		return err
	}
	b.DB = db

	if err := b.runMigrations(); err != nil {
		return fmt.Errorf("error running migrations: %w", err)
	}

	if b.MaxLimit != 0 {
		b.MaxLimitNegentropy = b.MaxLimit
	} else {
		b.MaxLimit = 1000
		if b.MaxLimitNegentropy == 0 {
			b.MaxLimitNegentropy = 16777216
		}
	}

	if err := b.DB.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{
			Prefix:  []byte{0},
			Reverse: true,
		})
		it.Seek([]byte{1})
		if it.Valid() {
			key := it.Item().Key()
			idx := key[1:]
			serial := binary.BigEndian.Uint32(idx)
			b.serial.Store(serial)
		}
		it.Close()
		return nil
	}); err != nil {
		return fmt.Errorf("error initializing serial: %w", err)
	}

	return nil
}

func (b *BadgerBackend) Close() {
	b.DB.Close()
}

func (b *BadgerBackend) Serial() []byte {
	next := b.serial.Add(1)
	vb := make([]byte, 5)
	vb[0] = rawEventStorePrefix
	binary.BigEndian.PutUint32(vb[1:], next)
	return vb
}
