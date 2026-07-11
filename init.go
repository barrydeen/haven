package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/lmdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/blossom"
	"fiatjaf.com/nostr/khatru/policies"
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

type DBBackend = eventstore.Store

func newDBBackend(path string) DBBackend {
	switch config.DBEngine {
	case "lmdb":
		return newLMDBBackend(path)
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
	privatePrivatePubKey := nostr.MustPubKeyFromHex(nPubToPubkey("PRIVATE_RELAY_NPUB", config.PrivateRelayNpub))
	privateRelay.Info.PubKey = &privatePrivatePubKey
	privateRelay.Info.Description = config.PrivateRelayDescription
	privateRelay.Info.Icon = config.PrivateRelayIcon
	privateRelay.Info.Version = config.RelayVersion
	privateRelay.Info.Software = config.RelaySoftware
	privateRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/private"

	privateRelay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if !privateRelayLimits.AllowEmptyFilters {
			if reject, msg := policies.NoEmptyFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if !privateRelayLimits.AllowComplexFilters {
			if reject, msg := policies.NoComplexFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if reject, msg := policies.MustAuth(ctx, filter); reject {
			return reject, msg
		}
		return MustBeWhitelistedToQuery(ctx, filter)
	}

	privateRelay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if reject, msg := policies.RejectEventsWithBase64Media(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := policies.EventIPRateLimiter(
			privateRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.EventIPLimiterInterval),
			privateRelayLimits.EventIPLimiterMaxTokens,
		)(ctx, event); reject {
			return reject, msg
		}
		return MustBeWhitelistedToPost(ctx, &event)
	}

	privateRelay.RejectConnection = policies.ConnectionRateLimiter(
		privateRelayLimits.ConnectionRateLimiterTokensPerInterval,
		time.Minute*time.Duration(privateRelayLimits.ConnectionRateLimiterInterval),
		privateRelayLimits.ConnectionRateLimiterMaxTokens,
	)

	privateRelay.OnConnect = khatru.RequestAuth

	privateRelay.UseEventstore(privateDB, 1000)

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
	chatPrivatePubKey := nostr.MustPubKeyFromHex(nPubToPubkey("CHAT_RELAY_NPUB", config.ChatRelayNpub))
	chatRelay.Info.PubKey = &chatPrivatePubKey
	chatRelay.Info.Description = config.ChatRelayDescription
	chatRelay.Info.Icon = config.ChatRelayIcon
	chatRelay.Info.Version = config.RelayVersion
	chatRelay.Info.Software = config.RelaySoftware
	chatRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/chat"

	chatRelay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if !chatRelayLimits.AllowEmptyFilters {
			if reject, msg := policies.NoEmptyFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if !chatRelayLimits.AllowComplexFilters {
			if reject, msg := policies.NoComplexFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if reject, msg := policies.MustAuth(ctx, filter); reject {
			return reject, msg
		}
		return MustBeInWotToQuery(ctx, filter)
	}

	chatRelay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if reject, msg := policies.RejectEventsWithBase64Media(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := policies.EventIPRateLimiter(
			chatRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.EventIPLimiterInterval),
			chatRelayLimits.EventIPLimiterMaxTokens,
		)(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := MustNotBeBlacklistedToPost(ctx, &event); reject {
			return reject, msg
		}
		if reject, msg := MustBeInWotToPost(ctx, &event); reject {
			return reject, msg
		}
		return EventMustBeChatRelated(ctx, &event)
	}

	chatRelay.RejectConnection = policies.ConnectionRateLimiter(
		chatRelayLimits.ConnectionRateLimiterTokensPerInterval,
		time.Minute*time.Duration(chatRelayLimits.ConnectionRateLimiterInterval),
		chatRelayLimits.ConnectionRateLimiterMaxTokens,
	)

	chatRelay.OnConnect = khatru.RequestAuth

	chatRelay.UseEventstore(chatDB, 1000)

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
	outboxOutboxPubKey := nostr.MustPubKeyFromHex(nPubToPubkey("OUTBOX_RELAY_NPUB", config.OutboxRelayNpub))
	outboxRelay.Info.PubKey = &outboxOutboxPubKey
	outboxRelay.Info.Description = config.OutboxRelayDescription
	outboxRelay.Info.Icon = config.OutboxRelayIcon
	outboxRelay.Info.Version = config.RelayVersion
	outboxRelay.Info.Software = config.RelaySoftware
	outboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL

	outboxRelay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if !outboxRelayLimits.AllowEmptyFilters {
			if reject, msg := policies.NoEmptyFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if !outboxRelayLimits.AllowComplexFilters {
			if reject, msg := policies.NoComplexFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		return false, ""
	}

	outboxRelay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if reject, msg := policies.RejectEventsWithBase64Media(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := policies.EventIPRateLimiter(
			outboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.EventIPLimiterInterval),
			outboxRelayLimits.EventIPLimiterMaxTokens,
		)(ctx, event); reject {
			return reject, msg
		}
		return MustBeWhitelistedToPost(ctx, &event)
	}

	outboxRelay.RejectConnection = policies.ConnectionRateLimiter(
		outboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
		time.Minute*time.Duration(outboxRelayLimits.ConnectionRateLimiterInterval),
		outboxRelayLimits.ConnectionRateLimiterMaxTokens,
	)

	outboxRelay.UseEventstore(outboxDB, 1000)
	outboxRelay.OnEventSaved = func(ctx context.Context, event nostr.Event) {
		go blast(ctx, &event)
	}

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
	bl.StoreBlob = func(ctx context.Context, sha256 string, ext string, body []byte) error {
		slog.Debug("storing blob", "sha256", sha256, "ext", ext)
		file, err := fs.Create(config.BlossomPath + sha256)
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, bytes.NewReader(body)); err != nil {
			return err
		}
		return nil
	}
	bl.LoadBlob = func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, *url.URL, error) {
		slog.Debug("loading blob", "sha256", sha256, "ext", ext)
		file, err := fs.Open(config.BlossomPath + sha256)
		if err != nil {
			return nil, nil, err
		}
		return file, nil, nil
	}
	bl.DeleteBlob = func(ctx context.Context, sha256 string, ext string) error {
		slog.Debug("deleting blob", "sha256", sha256, "ext", ext)
		return fs.Remove(config.BlossomPath + sha256)
	}
	bl.RejectUpload = func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		if _, ok := config.WhitelistedPubKeys[event.PubKey.Hex()]; ok {
			return false, ext, size
		}

		return true, "only media signed by whitelisted pubkeys are allowed", 403
	}
	migrateBlossomMetadata(ctx, bl)

	inboxRelay.Info.Name = config.InboxRelayName
	inboxInboxPubKey := nostr.MustPubKeyFromHex(nPubToPubkey("INBOX_RELAY_NPUB", config.InboxRelayNpub))
	inboxRelay.Info.PubKey = &inboxInboxPubKey
	inboxRelay.Info.Description = config.InboxRelayDescription
	inboxRelay.Info.Icon = config.InboxRelayIcon
	inboxRelay.Info.Version = config.RelayVersion
	inboxRelay.Info.Software = config.RelaySoftware
	inboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/inbox"

	inboxRelay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if !inboxRelayLimits.AllowEmptyFilters {
			if reject, msg := policies.NoEmptyFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		if !inboxRelayLimits.AllowComplexFilters {
			if reject, msg := policies.NoComplexFilters(ctx, filter); reject {
				return reject, msg
			}
		}
		return false, ""
	}

	inboxRelay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if reject, msg := policies.RejectEventsWithBase64Media(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := policies.EventIPRateLimiter(
			inboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.EventIPLimiterInterval),
			inboxRelayLimits.EventIPLimiterMaxTokens,
		)(ctx, event); reject {
			return reject, msg
		}
		if reject, msg := OnlyGiftWrappedDMs(ctx, &event); reject {
			return reject, msg
		}
		if reject, msg := MustNotBeBlacklistedToPost(ctx, &event); reject {
			return reject, msg
		}
		if reject, msg := MustBeInWotToPost(ctx, &event); reject {
			return reject, msg
		}
		return MustTagWhitelistedPubKey(ctx, &event)
	}

	inboxRelay.RejectConnection = policies.ConnectionRateLimiter(
		inboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
		time.Minute*time.Duration(inboxRelayLimits.ConnectionRateLimiterInterval),
		inboxRelayLimits.ConnectionRateLimiterMaxTokens,
	)

	inboxRelay.UseEventstore(inboxDB, 1000)

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
