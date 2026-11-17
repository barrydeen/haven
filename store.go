package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/lmdb"
)

// DBBackend is haven's storage surface, unchanged from the eventstore the
// relay was built against: channels and context-aware calls. The successor
// store's API is iterator-based with no context and no error channel, so
// storeBackend below is the bridge, and it preserves the invariant that made
// the old backends safe to use: a query is always drained to its end, because
// the backend releases its read transaction only when its sequence finishes.
type DBBackend interface {
	Init() error
	Close()
	CountEvents(ctx context.Context, filter nostr.Filter) (int64, error)
	DeleteEvent(ctx context.Context, evt *nostr.Event) error
	QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error)
	QueryEventsIter(ctx context.Context, filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]
	SaveEvent(ctx context.Context, evt *nostr.Event) error
	ReplaceEvent(ctx context.Context, evt *nostr.Event) error

	// Raw exposes the successor store underneath, for code that needs the
	// successor's own interface (the blossom blob index wrapper).
	Raw() eventstore.Store
}

// storeBackend wraps one successor eventstore.Store.
type storeBackend struct {
	store eventstore.Store
}

var _ DBBackend = storeBackend{}

func (b storeBackend) Init() error { return b.store.Init() }
func (b storeBackend) Close()      { b.store.Close() }
func (b storeBackend) Raw() eventstore.Store {
	return b.store
}

// QueryEventsIter is the native shape: maxLimit is honoured as a hard cap by
// the successor backends, and stopping the iteration early — by returning
// false from yield — releases the backend's read transaction cleanly. A
// cancelled context stops the same way.
func (b storeBackend) QueryEventsIter(
	ctx context.Context,
	filter nostr.Filter,
	maxLimit int,
) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		b.store.QueryEvents(filter, maxLimit)(func(evt nostr.Event) bool {
			if ctx.Err() != nil {
				return false
			}
			return yield(evt)
		})
	}
}

// QueryEvents keeps the old channel shape for callers that drain fully. The
// feeding goroutine cannot know when a caller stops reading, so a caller that
// walks away must cancel the context — which stops the iterator cleanly — or
// read to the end. It must not simply stop reading: that strands the
// goroutine and the backend's open read transaction exactly the way the old
// backends' unbuffered channels did.
func (b storeBackend) QueryEvents(
	ctx context.Context,
	filter nostr.Filter,
) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)
	go func() {
		b.QueryEventsIter(ctx, filter, math.MaxInt32)(func(evt nostr.Event) bool {
			select {
			case ch <- &evt:
				return true
			case <-ctx.Done():
				return false
			}
		})
		close(ch)
	}()
	return ch, nil
}

func (b storeBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.store.CountEvents(filter)
	return int64(n), err
}

func (b storeBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.store.SaveEvent(*evt)
}

// ReplaceEvent reports the events it deleted; haven's callers treat a replace
// as fire-and-forget, so the deleted list is logged rather than threaded
// through the old single-error interface.
func (b storeBackend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deleted, err := b.store.ReplaceEvent(*evt)
	for range deleted {
		slog.Debug("🔁 replaced an event", "id", evt.ID)
	}
	return err
}

func (b storeBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.store.DeleteEvent(evt.ID)
}

// publishToDB is the RelayWrapper.Publish behaviour the importer relied on:
// ephemeral events are not stored, regular events are saved (a duplicate is
// not an error), and replaceable/addressable events are replaced.
func publishToDB(ctx context.Context, db DBBackend, evt nostr.Event) error {
	if evt.Kind.IsEphemeral() {
		return nil
	}
	if evt.Kind.IsRegular() {
		if err := db.SaveEvent(ctx, &evt); err != nil && !errors.Is(err, eventstore.ErrDupEvent) {
			return fmt.Errorf("failed to save: %w", err)
		}
		return nil
	}
	return db.ReplaceEvent(ctx, &evt)
}

// newDBBackend builds one store. DB_ENGINE picks the engine; the successor
// eventstore ships lmdb (cgo) and boltdb (pure Go) and has dropped badger.
func newDBBackend(path string) DBBackend {
	switch config.DBEngine {
	case "lmdb":
		return storeBackend{store: newLMDBBackend(path)}
	case "bolt", "boltdb":
		return storeBackend{store: &boltdb.BoltBackend{
			Path: path,
		}}
	case "badger":
		// not a silent fallback: lmdb would open a fresh, empty database at
		// the same path and everything in the badger files would look gone.
		panic("DB_ENGINE=badger is no longer supported (the khatru successor dropped the badger backend); " +
			"use lmdb (the default) or boltdb, and re-import the events")
	default:
		return storeBackend{store: newLMDBBackend(path)}
	}
}

// newBlobDBBackend used to raise the backend's own query cap for the blob
// index; the successor backends have no per-backend cap — the limit is passed
// per query, and the blob walks pass blobIndexMaxLimit where they used to
// depend on this.
func newBlobDBBackend(path string) DBBackend {
	return newDBBackend(path)
}

// newLMDBBackend keeps haven's sizing knob; Path and MapSize are the same
// fields the successor backend carries.
func newLMDBBackend(path string) *lmdb.LMDBBackend {
	return &lmdb.LMDBBackend{
		Path:    path,
		MapSize: config.LmdbMapSize,
	}
}
