package main

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
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

// newBlobDBBackend, newDBBackend, newLMDBBackend and the DBBackend interface
// live in store.go; they moved when the relay moved to the khatru successor
// (whose eventstore is iterator-based and has no badger backend).

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
	privateRelay.Info.PubKey = relayPubKey("PRIVATE_RELAY_NPUB", config.PrivateRelayNpub)
	privateRelay.Info.Description = config.PrivateRelayDescription
	privateRelay.Info.Icon = config.PrivateRelayIcon
	privateRelay.Info.Version = config.RelayVersion
	privateRelay.Info.Software = config.RelaySoftware
	privateRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/private"

	var privateRequestPolicies []requestPolicy
	if !privateRelayLimits.AllowEmptyFilters {
		privateRequestPolicies = append(privateRequestPolicies, policies.NoEmptyFilters)
	}
	if !privateRelayLimits.AllowComplexFilters {
		privateRequestPolicies = append(privateRequestPolicies, policies.NoComplexFilters)
	}
	privateRequestPolicies = append(privateRequestPolicies, policies.MustAuth, MustBeWhitelistedToQuery)
	privateRelay.OnRequest = composeRequestPolicies(privateRequestPolicies...)

	privateRelay.OnEvent = composeEventPolicies(
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayPrivate),
		MustBeAnAllowedKind(relayPrivate),
		valueEventPolicy(policies.RejectEventsWithBase64Media),
		valueEventPolicy(policies.EventIPRateLimiter(
			privateRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.EventIPLimiterInterval),
			privateRelayLimits.EventIPLimiterMaxTokens,
		)),
		MustBeWhitelistedToPost,
		MustNotBeDeleted(privateDB),
	)

	privateRelay.RejectConnection = composeConnectionPolicies(
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			privateRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(privateRelayLimits.ConnectionRateLimiterInterval),
			privateRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	privateRelay.OnConnect = khatru.RequestAuth

	relayStoreHooks(privateRelay, privateDB)
	privateRelay.AllowDeleting = OwnerCanDeleteAnyEvent
	privateRelay.OverwriteRelayInformation = OverwriteRelayInfo(relayPrivate)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(privateRelay, relayPrivate)

	mux := privateRelay.Router()

	mux.HandleFunc("GET /private", relayIndexHandler(
		relayPrivate,
		privateRelay.Info.PubKey.Hex(),
		config.PrivateRelayName,
		config.PrivateRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/private",
	))

	chatRelay.Info.Name = config.ChatRelayName
	chatRelay.Info.PubKey = relayPubKey("CHAT_RELAY_NPUB", config.ChatRelayNpub)
	chatRelay.Info.Description = config.ChatRelayDescription
	chatRelay.Info.Icon = config.ChatRelayIcon
	chatRelay.Info.Version = config.RelayVersion
	chatRelay.Info.Software = config.RelaySoftware
	chatRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/chat"

	var chatRequestPolicies []requestPolicy
	if !chatRelayLimits.AllowEmptyFilters {
		chatRequestPolicies = append(chatRequestPolicies, policies.NoEmptyFilters)
	}
	if !chatRelayLimits.AllowComplexFilters {
		chatRequestPolicies = append(chatRequestPolicies, policies.NoComplexFilters)
	}
	chatRequestPolicies = append(chatRequestPolicies, policies.MustAuth, MustBeInWotToQuery)
	chatRelay.OnRequest = composeRequestPolicies(chatRequestPolicies...)

	chatRelay.OnEvent = composeEventPolicies(
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayChat),
		MustBeAnAllowedKind(relayChat),
		valueEventPolicy(policies.RejectEventsWithBase64Media),
		valueEventPolicy(policies.EventIPRateLimiter(
			chatRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.EventIPLimiterInterval),
			chatRelayLimits.EventIPLimiterMaxTokens,
		)),
		MustNotBeBlacklistedToPost,
		MustBeInWotToPost,
		EventMustBeChatRelated,
		MustNotBeDeleted(chatDB),
	)

	chatRelay.RejectConnection = composeConnectionPolicies(
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			chatRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(chatRelayLimits.ConnectionRateLimiterInterval),
			chatRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	chatRelay.OnConnect = khatru.RequestAuth

	relayStoreHooks(chatRelay, chatDB)
	chatRelay.AllowDeleting = OwnerCanDeleteAnyEvent
	chatRelay.OverwriteRelayInformation = OverwriteRelayInfo(relayChat)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(chatRelay, relayChat)

	mux = chatRelay.Router()

	mux.HandleFunc("GET /chat", relayIndexHandler(
		relayChat,
		chatRelay.Info.PubKey.Hex(),
		config.ChatRelayName,
		config.ChatRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/chat",
	))

	outboxRelay.Info.Name = config.OutboxRelayName
	outboxRelay.Info.PubKey = relayPubKey("OUTBOX_RELAY_NPUB", config.OutboxRelayNpub)
	outboxRelay.Info.Description = config.OutboxRelayDescription
	outboxRelay.Info.Icon = config.OutboxRelayIcon
	outboxRelay.Info.Version = config.RelayVersion
	outboxRelay.Info.Software = config.RelaySoftware
	outboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL

	var outboxRequestPolicies []requestPolicy
	if !outboxRelayLimits.AllowEmptyFilters {
		outboxRequestPolicies = append(outboxRequestPolicies, policies.NoEmptyFilters)
	}
	if !outboxRelayLimits.AllowComplexFilters {
		outboxRequestPolicies = append(outboxRequestPolicies, policies.NoComplexFilters)
	}
	outboxRelay.OnRequest = composeRequestPolicies(outboxRequestPolicies...)

	outboxRelay.OnEvent = composeEventPolicies(
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayOutbox),
		MustBeAnAllowedKind(relayOutbox),
		valueEventPolicy(policies.RejectEventsWithBase64Media),
		valueEventPolicy(policies.EventIPRateLimiter(
			outboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.EventIPLimiterInterval),
			outboxRelayLimits.EventIPLimiterMaxTokens,
		)),
		MustBeWhitelistedToPost,
		MustNotBeDeleted(outboxDB),
	)

	outboxRelay.RejectConnection = composeConnectionPolicies(
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			outboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(outboxRelayLimits.ConnectionRateLimiterInterval),
			outboxRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	outboxRelay.StoreEvent = func(ctx context.Context, evt nostr.Event) error {
		if err := outboxDB.SaveEvent(ctx, &evt); err != nil {
			return err
		}
		go blast(ctx, &evt)
		return nil
	}
	relayStoreHooksQueryOnly(outboxRelay, outboxDB)
	outboxRelay.AllowDeleting = OwnerCanDeleteAnyEvent
	outboxRelay.OverwriteRelayInformation = OverwriteRelayInfo(relayOutbox)
	outboxRelay.OnEventSaved = refreshBanList

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(outboxRelay, relayOutbox)

	mux = outboxRelay.Router()

	mux.HandleFunc("GET /{$}", relayIndexHandler(
		relayOutbox,
		outboxRelay.Info.PubKey.Hex(),
		config.OutboxRelayName,
		config.OutboxRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/outbox",
	))

	bl := blossom.New(outboxRelay, blobServiceURL())
	bl.Store = havenBlobIndex{
		EventStoreBlobIndexWrapper: blossom.EventStoreBlobIndexWrapper{Store: blossomDB.Raw(), ServiceURL: bl.ServiceURL},
	}
	bl.StoreBlob = func(ctx context.Context, sha256 string, ext string, body []byte) error {
		slog.Debug("storing blob", "sha256", sha256, "ext", ext)
		if err := writeBlob(sha256, body); err != nil {
			return err
		}
		blobInventory.invalidate()
		return nil
	}
	bl.LoadBlob = func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, *url.URL, error) {
		slog.Debug("loading blob", "sha256", sha256, "ext", ext)
		file, err := fs.Open(blobPath(sha256))
		return file, nil, err
	}
	bl.DeleteBlob = func(ctx context.Context, sha256 string, ext string) error {
		slog.Debug("deleting blob", "sha256", sha256, "ext", ext)
		removed, err := removeBlob(sha256)
		if removed {
			blobInventory.invalidate()
		}
		return err
	}

	// the whitelist check stays first: somebody who cannot upload at all should
	// never get to use this endpoint to find out whether a hash is blocked
	whitelistUpload := func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		if isWhitelisted(event.PubKey.Hex()) {
			return false, ext, size
		}

		return true, "only media signed by whitelisted pubkeys are allowed", 403
	}

	// khatru hashes the body after this hook runs, so the only hash available
	// here is the one the client declared in its own authorization event. That
	// is optional and unverified, which is fine: this exists to give a well
	// behaved client a clear refusal — including from HEAD /upload, before it
	// sends a gigabyte — while havenBlobIndex.Keep is what actually enforces the
	// block on everything else.
	blocklistUpload := func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		for tag := range event.Tags.FindAll("x") {
			if isBlockedBlob(strings.ToLower(strings.TrimSpace(tag[1]))) {
				return true, "this blob is blocked by the relay owner", 403
			}
		}
		return false, ext, size
	}

	// whitelist first, then the owner's blocklist; the successor consumes a
	// single function, and a non-rejecting policy's returned ext/size are
	// ignored by it, so the passthroughs are vestigial but harmless
	bl.RejectUpload = composeUploadPolicies(whitelistUpload, blocklistUpload)

	// a blob that was blocked after it was stored stays on disk until the owner
	// deletes it, so this is what stops it being served in the meantime. Blobs
	// go out with a week of immutable caching, so anything that already fetched
	// one keeps its copy regardless.
	bl.RejectGet = func(ctx context.Context, auth *nostr.Event, sha256 string, ext string) (bool, string, int) {
		if isBlockedBlob(sha256) {
			return true, "this blob has been removed by the relay owner", 410
		}
		return false, "", 0
	}

	// after every RejectUpload and RejectGet above, for the same reason the relay
	// counters go last; the LoadBlob counter inside is prepended instead, because
	// that loop stops at the first hook returning a reader
	instrumentBlossom(bl, relayOutbox)

	migrateBlossomMetadata(ctx, bl)

	inboxRelay.Info.Name = config.InboxRelayName
	inboxRelay.Info.PubKey = relayPubKey("INBOX_RELAY_NPUB", config.InboxRelayNpub)
	inboxRelay.Info.Description = config.InboxRelayDescription
	inboxRelay.Info.Icon = config.InboxRelayIcon
	inboxRelay.Info.Version = config.RelayVersion
	inboxRelay.Info.Software = config.RelaySoftware
	inboxRelay.ServiceURL = getHTTPScheme(config.RelayURL) + config.RelayURL + "/inbox"

	var inboxRequestPolicies []requestPolicy
	if !inboxRelayLimits.AllowEmptyFilters {
		inboxRequestPolicies = append(inboxRequestPolicies, policies.NoEmptyFilters)
	}
	if !inboxRelayLimits.AllowComplexFilters {
		inboxRequestPolicies = append(inboxRequestPolicies, policies.NoComplexFilters)
	}
	inboxRelay.OnRequest = composeRequestPolicies(inboxRequestPolicies...)

	inboxRelay.OnEvent = composeEventPolicies(
		MustNotBeBannedToPost,
		MustNotBeABannedEvent(relayInbox),
		MustBeAnAllowedKind(relayInbox),
		valueEventPolicy(policies.RejectEventsWithBase64Media),
		valueEventPolicy(policies.EventIPRateLimiter(
			inboxRelayLimits.EventIPLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.EventIPLimiterInterval),
			inboxRelayLimits.EventIPLimiterMaxTokens,
		)),
		OnlyGiftWrappedDMs,
		MustNotBeBlacklistedToPost,
		MustBeInWotToPost,
		MustTagWhitelistedPubKey,
		MustNotBeDeleted(inboxDB),
	)

	inboxRelay.RejectConnection = composeConnectionPolicies(
		MustNotBeIPBlocked,
		policies.ConnectionRateLimiter(
			inboxRelayLimits.ConnectionRateLimiterTokensPerInterval,
			time.Minute*time.Duration(inboxRelayLimits.ConnectionRateLimiterInterval),
			inboxRelayLimits.ConnectionRateLimiterMaxTokens,
		),
	)

	inboxRelay.StoreEvent = func(ctx context.Context, evt nostr.Event) error {
		return inboxDB.SaveEvent(ctx, &evt)
	}
	relayStoreHooksQueryOnly(inboxRelay, inboxDB)
	inboxRelay.AllowDeleting = OwnerCanDeleteAnyEvent
	inboxRelay.OverwriteRelayInformation = OverwriteRelayInfo(relayInbox)

	// last in this block on purpose: the "passed" counters have to sit behind
	// every policy above, or they would count rejections as acceptances
	instrument(inboxRelay, relayInbox)

	mux = inboxRelay.Router()

	mux.HandleFunc("GET /inbox", relayIndexHandler(
		relayInbox,
		inboxRelay.Info.PubKey.Hex(),
		config.InboxRelayName,
		config.InboxRelayDescription,
		getWSScheme(config.RelayURL)+config.RelayURL+"/inbox",
	))

}
