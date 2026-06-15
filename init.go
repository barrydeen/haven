package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
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

var blossomDB = newDBBackend("db/blossom")

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
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			privateRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.EventIPLimiterInterval),
			privateRelayLimits.EventIPLimiterMaxTokens,
		),
		MustBeWhitelistedToPost,
	)

	privateRelay.RejectConnection = append(privateRelay.RejectConnection,
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
	privateRelay.CountEvents = append(privateRelay.CountEvents, privateDB.CountEvents)
	privateRelay.ReplaceEvent = append(privateRelay.ReplaceEvent, privateDB.ReplaceEvent)

	mux := privateRelay.Router()

	mux.HandleFunc("GET /private", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles("templates/index.html"))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.PrivateRelayName,
			RelayPubkey:      nPubToPubkey("PRIVATE_RELAY_NPUB", config.PrivateRelayNpub),
			RelayDescription: config.PrivateRelayDescription,
			RelayURL:         getWSScheme(config.RelayURL) + config.RelayURL + "/private",
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

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
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			chatRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.EventIPLimiterInterval),
			chatRelayLimits.EventIPLimiterMaxTokens,
		),
		MustNotBeBlacklistedToPost,
		MustBeInWotToPost,
		EventMustBeChatRelated,
	)

	chatRelay.RejectConnection = append(chatRelay.RejectConnection,
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
	chatRelay.CountEvents = append(chatRelay.CountEvents, chatDB.CountEvents)
	chatRelay.ReplaceEvent = append(chatRelay.ReplaceEvent, chatDB.ReplaceEvent)

	mux = chatRelay.Router()

	mux.HandleFunc("GET /chat", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles("templates/index.html"))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.ChatRelayName,
			RelayPubkey:      nPubToPubkey("CHAT_RELAY_NPUB", config.ChatRelayNpub),
			RelayDescription: config.ChatRelayDescription,
			RelayURL:         getWSScheme(config.RelayURL) + config.RelayURL + "/chat",
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

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
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(
			outboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.EventIPLimiterInterval),
			outboxRelayLimits.EventIPLimiterMaxTokens,
		),
		MustBeWhitelistedToPost,
	)

	outboxRelay.RejectConnection = append(outboxRelay.RejectConnection,
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
	outboxRelay.CountEvents = append(outboxRelay.CountEvents, outboxDB.CountEvents)
	outboxRelay.ReplaceEvent = append(outboxRelay.ReplaceEvent, outboxDB.ReplaceEvent)

	mux = outboxRelay.Router()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		tmpl := template.Must(template.ParseFiles("templates/index.html"))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.OutboxRelayName,
			RelayPubkey:      nPubToPubkey("OUTBOX_RELAY_NPUB", config.OutboxRelayNpub),
			RelayDescription: config.OutboxRelayDescription,
			RelayURL:         getWSScheme(config.RelayURL) + config.RelayURL + "/outbox",
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	bl := blossom.New(outboxRelay, getHTTPScheme(config.RelayURL)+config.RelayURL)
	bl.Store = blossom.EventStoreBlobIndexWrapper{Store: blossomDB, ServiceURL: bl.ServiceURL}
	bl.StoreBlob = append(bl.StoreBlob, func(ctx context.Context, sha256 string, ext string, body []byte) error {
		slog.Debug("storing blob", "sha256", sha256, "ext", ext)
		file, err := fs.Create(config.BlossomPath + sha256)
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, bytes.NewReader(body)); err != nil {
			return err
		}
		return nil
	})
	bl.LoadBlob = append(bl.LoadBlob, func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, error) {
		slog.Debug("loading blob", "sha256", sha256, "ext", ext)
		return fs.Open(config.BlossomPath + sha256)
	})
	bl.DeleteBlob = append(bl.DeleteBlob, func(ctx context.Context, sha256 string, ext string) error {
		slog.Debug("deleting blob", "sha256", sha256, "ext", ext)
		return fs.Remove(config.BlossomPath + sha256)
	})
	bl.RejectUpload = append(bl.RejectUpload, func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		if _, ok := config.WhitelistedPubKeys[event.PubKey]; ok {
			return false, ext, size
		}

		return true, "only media signed by whitelisted pubkeys are allowed", 403
	})
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
	)

	inboxRelay.RejectConnection = append(inboxRelay.RejectConnection,
		policies.ConnectionRateLimiter(
			inboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.ConnectionRateLimiterInterval),
			inboxRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	inboxRelay.StoreEvent = append(inboxRelay.StoreEvent, inboxDB.SaveEvent)
	inboxRelay.QueryEvents = append(inboxRelay.QueryEvents, inboxDB.QueryEvents)
	inboxRelay.DeleteEvent = append(inboxRelay.DeleteEvent, inboxDB.DeleteEvent)
	inboxRelay.CountEvents = append(inboxRelay.CountEvents, inboxDB.CountEvents)
	inboxRelay.ReplaceEvent = append(inboxRelay.ReplaceEvent, inboxDB.ReplaceEvent)

	mux = inboxRelay.Router()

	mux.HandleFunc("GET /inbox", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles("templates/index.html"))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.InboxRelayName,
			RelayPubkey:      nPubToPubkey("INBOX_RELAY_NPUB", config.InboxRelayNpub),
			RelayDescription: config.InboxRelayDescription,
			RelayURL:         getWSScheme(config.RelayURL) + config.RelayURL + "/inbox",
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

}
