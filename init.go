package main

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/blossom"
	"github.com/fiatjaf/khatru/policies"
	"github.com/nbd-wtf/go-nostr"
)

// getHTTPScheme returns the appropriate HTTP scheme based on the URL.
// Returns "http://" for .onion domains (Tor), "https://" for regular domains.
func getHTTPScheme(url string) string {
	if strings.Contains(url, ".onion") {
		return "http://"
	}
	return "https://"
}

// getWSScheme returns the appropriate WebSocket scheme based on the URL.
// Returns "ws://" for .onion domains (Tor), "wss://" for regular domains.
func getWSScheme(url string) string {
	if strings.Contains(url, ".onion") {
		return "ws://"
	}
	return "wss://"
}

// indexTemplate parses the relay landing page once, instead of on every
// request as this used to. It is html/template rather than text/template: the
// name and description it renders can now be changed through a web form over
// the management API, so escaping them stops being optional.
var indexTemplate = sync.OnceValues(func() (*template.Template, error) {
	return template.ParseFiles("templates/index.html")
})

// relayIndexHandler serves one relay's landing page. name and description are
// the .env values; whatever the owner set over the management API wins.
func relayIndexHandler(relay, pubKey, name, description, wsURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		tmpl, err := indexTemplate()
		if err != nil {
			slog.Error("🚫 error parsing the relay landing page", "error", err)
			http.Error(w, "the relay landing page is unavailable", http.StatusInternalServerError)
			return
		}

		effectiveName, effectiveDescription, _ := effectiveRelayInfo(relay, name, description, "")
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        effectiveName,
			RelayPubkey:      pubKey,
			RelayDescription: effectiveDescription,
			RelayURL:         wsURL,
		}

		// rendered whole before anything is written, so a failure halfway
		// through can still be reported as an error instead of a torn page
		var page bytes.Buffer
		if err := tmpl.Execute(&page, data); err != nil {
			slog.Error("🚫 error rendering the relay landing page", "relay", relay, "error", err)
			http.Error(w, "the relay landing page is unavailable", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(page.Bytes()); err != nil {
			slog.Debug("🚫 error writing the relay landing page", "error", err)
		}
	}
}

var (
	privateRelay = khatru.NewRelay()
	privateDB    = newDBBackend("db/private")
)

var (
	chatRelay = khatru.NewRelay()
	chatDB    = newDBBackend("db/chat")
)

var (
	outboxRelay = khatru.NewRelay()
	outboxDB    = newDBBackend("db/outbox")
)

var (
	inboxRelay = khatru.NewRelay()
	inboxDB    = newDBBackend("db/inbox")
)

var blossomDB = newBlobDBBackend("db/blossom")

var dbs = map[string]DBBackend{
	"blossom": blossomDB,
	"chat":    chatDB,
	"inbox":   inboxDB,
	"outbox":  outboxDB,
	"private": privateDB,
}

type DBBackend interface {
	Init() error
	Close()
	CountEvents(ctx context.Context, filter nostr.Filter) (int64, error)
	DeleteEvent(ctx context.Context, evt *nostr.Event) error
	QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error)
	SaveEvent(ctx context.Context, evt *nostr.Event) error
	ReplaceEvent(ctx context.Context, evt *nostr.Event) error
	Serial() []byte
}

func newDBBackend(path string) DBBackend {
	switch config.DBEngine {
	case "lmdb":
		return newLMDBBackend(path)
	case "badger":
		return &badger.BadgerBackend{
			Path: path,
		}
	default:
		return newLMDBBackend(path)
	}
}

// newBlobDBBackend builds the blob index database. It is the same backend as
// every other one with a single difference: the query limit is raised.
//
// Both engines cap a query at MaxLimit and answer a filter that asks for no
// limit — or for more than the cap — with a quarter of it, so the obvious "give
// me every blob" query returns 375 of them and nothing says the rest exist.
// Raising it cannot change what any relay serves: this database is not attached
// to a relay, only to the blossom index and the media browser.
func newBlobDBBackend(path string) DBBackend {
	switch db := newDBBackend(path).(type) {
	case *lmdb.LMDBBackend:
		db.MaxLimit = blobIndexMaxLimit
		return db
	case *badger.BadgerBackend:
		db.MaxLimit = blobIndexMaxLimit
		return db
	default:
		return db
	}
}

// queryPageSize is the largest limit a backend will actually honour, which is
// not the same thing as the largest limit it will accept. Both engines cap a
// query at MaxLimit and answer a filter asking for more than the cap — or for no
// limit at all — with a *quarter* of it rather than clamping to it, so a walk
// that guesses high gets 375 events on lmdb and 250 on badger and nothing says
// the rest exist. Init() fills MaxLimit in on both engines before anything can
// call this, so the value read here is normally already set.
//
// This deliberately reads MaxLimit rather than raising it. On a relay database
// MaxLimit is also what caps every public REQ the relay serves, and Init mirrors
// it into MaxLimitNegentropy — raising it so the owner could browse events
// faster would change what the relay hands to strangers.
func queryPageSize(db DBBackend) int {
	switch b := db.(type) {
	case *lmdb.LMDBBackend:
		if b.MaxLimit > 0 {
			return b.MaxLimit
		}
		return 1500
	case *badger.BadgerBackend:
		if b.MaxLimit > 0 {
			return b.MaxLimit
		}
		return 1000
	default:
		// a backend we do not recognise: a quarter of the smaller default is the
		// only page size that is safe whatever its cap turns out to be
		return 250
	}
}

// backendCountsSafely reports whether a backend's own CountEvents can be
// trusted to return.
//
// It cannot on lmdb. eventstore v0.17.5's lmdb/count.go advances its cursor only
// on the paths where an event is *rejected* by an extra author, kind or tag
// check — the two paths that actually increment the counter both fall through to
// the top of the loop without calling it.next(), so the cursor sits on the first
// matching event and counts it forever. A filter matching nothing returns zero
// correctly; a filter matching anything never returns at all, on a goroutine that
// never looks at the context, so it cannot even be cancelled.
//
// That matters here because DB_ENGINE defaults to lmdb and CountEvents(Filter{})
// is what the stats method is built on. countStoredEvents walks instead when
// this is false.
func backendCountsSafely(db DBBackend) bool {
	switch db.(type) {
	case *badger.BadgerBackend:
		return true
	case *lmdb.LMDBBackend:
		return false
	default:
		// an engine we do not know: walking is slower but it terminates
		return false
	}
}

func newLMDBBackend(path string) *lmdb.LMDBBackend {
	return &lmdb.LMDBBackend{
		Path:    path,
		MapSize: config.LmdbMapSize,
	}
}

func initDBs() {
	if err := privateDB.Init(); err != nil {
		panic(err)
	}

	if err := chatDB.Init(); err != nil {
		panic(err)
	}

	if err := outboxDB.Init(); err != nil {
		panic(err)
	}

	if err := inboxDB.Init(); err != nil {
		panic(err)
	}

	if err := blossomDB.Init(); err != nil {
		panic(err)
	}
}

func initRelays(ctx context.Context) {
	initDBs()

	loadBanList(ctx)

	initRelayLimits()

	privateRelay.Info.Name = config.PrivateRelayName
	privateRelay.Info.PubKey = nPubToPubkey("PRIVATE_RELAY_NPUB", config.PrivateRelayNpub)
	privateRelay.Info.Description = config.PrivateRelayDescription
	privateRelay.Info.Icon = config.PrivateRelayIcon
	privateRelay.Info.Version = config.RelayVersion
	privateRelay.Info.Software = config.RelaySoftware
	privateRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/private"

	if !privateRelayLimits.AllowEmptyFilters {
		privateRelay.RejectFilter = append(privateRelay.RejectFilter, policies.NoEmptyFilters)
	}
	if !privateRelayLimits.AllowComplexFilters {
		privateRelay.RejectFilter = append(privateRelay.RejectFilter, policies.NoComplexFilters)
	}
	privateRelay.RejectFilter = append(privateRelay.RejectFilter, policies.MustAuth, MustBeWhitelistedToQuery)

	privateRelay.RejectEvent = append(privateRelay.RejectEvent,
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayPrivate),
		MustBeAnAllowedKind(relayPrivate),
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			privateRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.EventIPLimiterInterval),
			privateRelayLimits.EventIPLimiterMaxTokens,
		),
		MustBeWhitelistedToPost,
		MustNotBeDeleted(privateDB),
	)

	privateRelay.RejectConnection = append(privateRelay.RejectConnection,
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			privateRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.ConnectionRateLimiterInterval),
			privateRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	privateRelay.OnConnect = append(privateRelay.OnConnect, khatru.RequestAuth)

	privateRelay.StoreEvent = append(privateRelay.StoreEvent, privateDB.SaveEvent)
	privateRelay.QueryEvents = append(privateRelay.QueryEvents, privateDB.QueryEvents)
	privateRelay.DeleteEvent = append(privateRelay.DeleteEvent, privateDB.DeleteEvent)
	privateRelay.OverwriteDeletionOutcome = append(privateRelay.OverwriteDeletionOutcome, OwnerCanDeleteAnyEvent)
	privateRelay.OverwriteRelayInformation = append(privateRelay.OverwriteRelayInformation, OverwriteRelayInfo(relayPrivate))
	privateRelay.CountEvents = append(privateRelay.CountEvents, privateDB.CountEvents)
	privateRelay.ReplaceEvent = append(privateRelay.ReplaceEvent, privateDB.ReplaceEvent)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(privateRelay, relayPrivate)

	mux := privateRelay.Router()

	mux.HandleFunc("GET /private", relayIndexHandler(
		relayPrivate,
		privateRelay.Info.PubKey,
		config.PrivateRelayName,
		config.PrivateRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/private",
	))

	chatRelay.Info.Name = config.ChatRelayName
	chatRelay.Info.PubKey = nPubToPubkey("CHAT_RELAY_NPUB", config.ChatRelayNpub)
	chatRelay.Info.Description = config.ChatRelayDescription
	chatRelay.Info.Icon = config.ChatRelayIcon
	chatRelay.Info.Version = config.RelayVersion
	chatRelay.Info.Software = config.RelaySoftware
	chatRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/chat"

	if !chatRelayLimits.AllowEmptyFilters {
		chatRelay.RejectFilter = append(chatRelay.RejectFilter, policies.NoEmptyFilters)
	}
	if !chatRelayLimits.AllowComplexFilters {
		chatRelay.RejectFilter = append(chatRelay.RejectFilter, policies.NoComplexFilters)
	}
	chatRelay.RejectFilter = append(chatRelay.RejectFilter, policies.MustAuth, MustBeInWotToQuery)

	chatRelay.RejectEvent = append(chatRelay.RejectEvent,
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayChat),
		MustBeAnAllowedKind(relayChat),
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			chatRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.EventIPLimiterInterval),
			chatRelayLimits.EventIPLimiterMaxTokens,
		),
		MustNotBeBlacklistedToPost,
		MustBeInWotToPost,
		EventMustBeChatRelated,
		MustNotBeDeleted(chatDB),
	)

	chatRelay.RejectConnection = append(chatRelay.RejectConnection,
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			chatRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.ConnectionRateLimiterInterval),
			chatRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	chatRelay.OnConnect = append(chatRelay.OnConnect, khatru.RequestAuth)

	chatRelay.StoreEvent = append(chatRelay.StoreEvent, chatDB.SaveEvent)
	chatRelay.QueryEvents = append(chatRelay.QueryEvents, chatDB.QueryEvents)
	chatRelay.DeleteEvent = append(chatRelay.DeleteEvent, chatDB.DeleteEvent)
	chatRelay.OverwriteDeletionOutcome = append(chatRelay.OverwriteDeletionOutcome, OwnerCanDeleteAnyEvent)
	chatRelay.OverwriteRelayInformation = append(chatRelay.OverwriteRelayInformation, OverwriteRelayInfo(relayChat))
	chatRelay.CountEvents = append(chatRelay.CountEvents, chatDB.CountEvents)
	chatRelay.ReplaceEvent = append(chatRelay.ReplaceEvent, chatDB.ReplaceEvent)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(chatRelay, relayChat)

	mux = chatRelay.Router()

	mux.HandleFunc("GET /chat", relayIndexHandler(
		relayChat,
		chatRelay.Info.PubKey,
		config.ChatRelayName,
		config.ChatRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/chat",
	))

	outboxRelay.Info.Name = config.OutboxRelayName
	outboxRelay.Info.PubKey = nPubToPubkey("OUTBOX_RELAY_NPUB", config.OutboxRelayNpub)
	outboxRelay.Info.Description = config.OutboxRelayDescription
	outboxRelay.Info.Icon = config.OutboxRelayIcon
	outboxRelay.Info.Version = config.RelayVersion
	outboxRelay.Info.Software = config.RelaySoftware
	outboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL

	if !outboxRelayLimits.AllowEmptyFilters {
		outboxRelay.RejectFilter = append(outboxRelay.RejectFilter, policies.NoEmptyFilters)
	}
	if !outboxRelayLimits.AllowComplexFilters {
		outboxRelay.RejectFilter = append(outboxRelay.RejectFilter, policies.NoComplexFilters)
	}

	outboxRelay.RejectEvent = append(outboxRelay.RejectEvent,
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayOutbox),
		MustBeAnAllowedKind(relayOutbox),
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			outboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.EventIPLimiterInterval),
			outboxRelayLimits.EventIPLimiterMaxTokens,
		),
		MustBeWhitelistedToPost,
		MustNotBeDeleted(outboxDB),
	)

	outboxRelay.RejectConnection = append(outboxRelay.RejectConnection,
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			outboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.ConnectionRateLimiterInterval),
			outboxRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	outboxRelay.StoreEvent = append(outboxRelay.StoreEvent, outboxDB.SaveEvent, func(ctx context.Context, event *nostr.Event) error {
		go blast(ctx, event)
		return nil
	})
	outboxRelay.QueryEvents = append(outboxRelay.QueryEvents, outboxDB.QueryEvents)
	outboxRelay.DeleteEvent = append(outboxRelay.DeleteEvent, outboxDB.DeleteEvent)
	outboxRelay.OverwriteDeletionOutcome = append(outboxRelay.OverwriteDeletionOutcome, OwnerCanDeleteAnyEvent)
	outboxRelay.OverwriteRelayInformation = append(outboxRelay.OverwriteRelayInformation, OverwriteRelayInfo(relayOutbox))
	outboxRelay.CountEvents = append(outboxRelay.CountEvents, outboxDB.CountEvents)
	outboxRelay.ReplaceEvent = append(outboxRelay.ReplaceEvent, outboxDB.ReplaceEvent)
	outboxRelay.OnEventSaved = append(outboxRelay.OnEventSaved, refreshBanList)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(outboxRelay, relayOutbox)

	mux = outboxRelay.Router()

	mux.HandleFunc("GET /{$}", relayIndexHandler(
		relayOutbox,
		outboxRelay.Info.PubKey,
		config.OutboxRelayName,
		config.OutboxRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/outbox",
	))

	bl := blossom.New(outboxRelay, blobServiceURL())
	bl.Store = havenBlobIndex{
		EventStoreBlobIndexWrapper: blossom.EventStoreBlobIndexWrapper{Store: blossomDB, ServiceURL: bl.ServiceURL},
	}
	bl.StoreBlob = append(bl.StoreBlob, func(ctx context.Context, sha256 string, ext string, body []byte) error {
		slog.Debug("storing blob", "sha256", sha256, "ext", ext)
		if err := writeBlob(sha256, body); err != nil {
			return err
		}
		blobInventory.invalidate()
		return nil
	})
	bl.LoadBlob = append(bl.LoadBlob, func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, error) {
		slog.Debug("loading blob", "sha256", sha256, "ext", ext)
		return fs.Open(blobPath(sha256))
	})
	bl.DeleteBlob = append(bl.DeleteBlob, func(ctx context.Context, sha256 string, ext string) error {
		slog.Debug("deleting blob", "sha256", sha256, "ext", ext)
		removed, err := removeBlob(sha256)
		if removed {
			blobInventory.invalidate()
		}
		return err
	})

	// the whitelist check stays first: somebody who cannot upload at all should
	// never get to use this endpoint to find out whether a hash is blocked
	bl.RejectUpload = append(bl.RejectUpload, func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		if isWhitelisted(event.PubKey) {
			return false, ext, size
		}

		return true, "only media signed by whitelisted pubkeys are allowed", 403
	})

	// khatru hashes the body after this hook runs, so the only hash available
	// here is the one the client declared in its own authorization event. That
	// is optional and unverified, which is fine: this exists to give a well
	// behaved client a clear refusal — including from HEAD /upload, before it
	// sends a gigabyte — while havenBlobIndex.Keep is what actually enforces the
	// block on everything else.
	bl.RejectUpload = append(bl.RejectUpload, func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		for tag := range event.Tags.FindAll("x") {
			if isBlockedBlob(strings.ToLower(strings.TrimSpace(tag[1]))) {
				return true, "this blob is blocked by the relay owner", 403
			}
		}
		return false, ext, size
	})

	// a blob that was blocked after it was stored stays on disk until the owner
	// deletes it, so this is what stops it being served in the meantime. Blobs
	// go out with a week of immutable caching, so anything that already fetched
	// one keeps its copy regardless.
	bl.RejectGet = append(bl.RejectGet, func(ctx context.Context, auth *nostr.Event, sha256 string, ext string) (bool, string, int) {
		if isBlockedBlob(sha256) {
			return true, "this blob has been removed by the relay owner", 410
		}
		return false, "", 0
	})

	// after every RejectUpload and RejectGet above, for the same reason the relay
	// counters go last; the LoadBlob counter inside is prepended instead, because
	// that loop stops at the first hook returning a reader
	instrumentBlossom(bl, relayOutbox)

	migrateBlossomMetadata(ctx, bl)

	inboxRelay.Info.Name = config.InboxRelayName
	inboxRelay.Info.PubKey = nPubToPubkey("INBOX_RELAY_NPUB", config.InboxRelayNpub)
	inboxRelay.Info.Description = config.InboxRelayDescription
	inboxRelay.Info.Icon = config.InboxRelayIcon
	inboxRelay.Info.Version = config.RelayVersion
	inboxRelay.Info.Software = config.RelaySoftware
	inboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/inbox"

	if !inboxRelayLimits.AllowEmptyFilters {
		inboxRelay.RejectFilter = append(inboxRelay.RejectFilter, policies.NoEmptyFilters)
	}
	if !inboxRelayLimits.AllowComplexFilters {
		inboxRelay.RejectFilter = append(inboxRelay.RejectFilter, policies.NoComplexFilters)
	}

	inboxRelay.RejectEvent = append(inboxRelay.RejectEvent,
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayInbox),
		MustBeAnAllowedKind(relayInbox),
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			inboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.EventIPLimiterInterval),
			inboxRelayLimits.EventIPLimiterMaxTokens,
		),
		OnlyGiftWrappedDMs,
		MustNotBeBlacklistedToPost,
		MustBeInWotToPost,
		MustTagWhitelistedPubKey,
		MustNotBeDeleted(inboxDB),
	)

	inboxRelay.RejectConnection = append(inboxRelay.RejectConnection,
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			inboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.ConnectionRateLimiterInterval),
			inboxRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	inboxRelay.StoreEvent = append(inboxRelay.StoreEvent, inboxDB.SaveEvent)
	inboxRelay.QueryEvents = append(inboxRelay.QueryEvents, inboxDB.QueryEvents)
	inboxRelay.DeleteEvent = append(inboxRelay.DeleteEvent, inboxDB.DeleteEvent)
	inboxRelay.OverwriteDeletionOutcome = append(inboxRelay.OverwriteDeletionOutcome, OwnerCanDeleteAnyEvent)
	inboxRelay.OverwriteRelayInformation = append(inboxRelay.OverwriteRelayInformation, OverwriteRelayInfo(relayInbox))
	inboxRelay.CountEvents = append(inboxRelay.CountEvents, inboxDB.CountEvents)
	inboxRelay.ReplaceEvent = append(inboxRelay.ReplaceEvent, inboxDB.ReplaceEvent)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(inboxRelay, relayInbox)

	mux = inboxRelay.Router()

	mux.HandleFunc("GET /inbox", relayIndexHandler(
		relayInbox,
		inboxRelay.Info.PubKey,
		config.InboxRelayName,
		config.InboxRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/inbox",
	))

}
