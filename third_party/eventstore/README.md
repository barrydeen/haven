<div>
<p><b>This repository is in maintenance mode and adventurous programmers are encouraged to try <a href="https://pkg.go.dev/fiatjaf.com/nostr/eventstore"><code>fiatjaf.com/nostr/eventstore@master</code></a> instead.</b></p>

<p>The new library integrates better with <code>fiatjaf.com/nostr/khatru</code> and the new <i>bbolt</i> and <i>LMDB</i> backends are better for streaming large numbers of events and migrating, making them more future-proof. But some backends were removed from the core library and are now encouraged to be ported and maintained as their own standalone modules.</p>
</div>

---

# eventstore

A collection of reusable database connectors, wrappers and schemas that store Nostr events and expose a simple Go interface:

```go
type Store interface {
	// Init is called at the very beginning by [Server.Start], after [Relay.Init],
	// allowing a storage to initialize its internal resources.
	Init() error

	// Close must be called after you're done using the store, to free up resources and so on.
	Close()

	// QueryEvents is invoked upon a client's REQ as described in NIP-01.
	// it should return a channel with the events as they're recovered from a database.
	// the channel should be closed after the events are all delivered.
	QueryEvents(context.Context, nostr.Filter) (chan *nostr.Event, error)

	// DeleteEvent is used to handle deletion events, as per NIP-09.
	DeleteEvent(context.Context, *nostr.Event) error

	// SaveEvent is called once Relay.AcceptEvent reports true.
	SaveEvent(context.Context, *nostr.Event) error
}
```

[![Go Reference](https://pkg.go.dev/badge/github.com/fiatjaf/eventstore.svg)](https://pkg.go.dev/github.com/fiatjaf/eventstore) [![Run Tests](https://github.com/fiatjaf/eventstore/actions/workflows/test.yml/badge.svg)](https://github.com/fiatjaf/eventstore/actions/workflows/test.yml)

## command-line tool

There is an [`eventstore` command-line tool](cmd/eventstore) that can be used to query these databases directly.
