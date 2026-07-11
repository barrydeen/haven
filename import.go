package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"fiatjaf.com/nostr"

	"github.com/barrydeen/haven/pkg/wot"
)

const layout = "2006-01-02"

func ensureImportRelays() {
	nErrors := 0
	log.Println("🧪 Testing import relays")
	for _, relay := range config.ImportSeedRelays {
		if _, err := pool.EnsureRelay(relay); err != nil {
			nErrors++
			slog.Error("🚫 Error connecting to relay", "relay", relay, "error", err)
		} else {
			slog.Debug("✅ Connected to relay", "relay", relay)
		}
	}
	if nErrors == 0 {
		slog.Info("✅ All relays connected successfully")
	} else if nErrors == len(config.ImportSeedRelays) {
		slog.Error("🚫 Unable to connect to any import relays, check your connectivity and relays_import.json file")
		os.Exit(1)
	} else {
		slog.Warn("⚠️ Some relays failed to connect, proceeding, but this may cause issues")
		slog.Info("ℹ️ If you always see this message during startup, consider removing the relays that are not working from your relays_import.json file")
	}
}

func runImport(ctx context.Context) {
	importCmd := flag.NewFlagSet("import", flag.ExitOnError)
	importCmd.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "Usage of import:\n")
		importCmd.PrintDefaults()
	}
	err := importCmd.Parse(os.Args[2:])
	if err != nil {
		log.Fatal("🚫 failed to parse import command:", err)
		return
	}

	initDBs()
	wotModel := wot.NewSimpleInMemory(
		pool,
		config.WhitelistedPubKeys,
		config.ImportSeedRelays,
		config.WotDepth,
		config.WotMinimumFollowers,
		config.WotFetchTimeoutSeconds,
	)
	wot.Initialize(ctx, wotModel)

	log.Println("📦 importing notes")
	importOwnerNotes(ctx)
	importTaggedNotes(ctx)
}

func importOwnerNotes(ctx context.Context) {
	ownerImportedNotes := 0
	nFailedImportNotes := 0

	startTime, err := time.Parse(layout, config.ImportStartDate)
	if err != nil {
		fmt.Println("Error parsing start date:", err)
		return
	}
	endTime := startTime.Add(240 * time.Hour)

	for {
		startTimestamp := nostr.Timestamp(startTime.Unix())
		endTimestamp := nostr.Timestamp(endTime.Unix())
		authors := make([]nostr.PubKey, 0, len(config.WhitelistedPubKeys))
		for pubkeyHex := range config.WhitelistedPubKeys {
			authors = append(authors, nostr.MustPubKeyFromHex(pubkeyHex))
		}

		filter := nostr.Filter{
			Authors: authors,
			Since:   startTimestamp,
			Until:   endTimestamp,
		}

		done := make(chan int, 1)
		timeout := time.Duration(config.ImportOwnerNotesFetchTimeoutSeconds) * time.Second
		ctx, cancel := context.WithTimeout(ctx, timeout)

		go func() {
			defer cancel()
			batchImportedNotes := 0

			events := pool.FetchMany(ctx, config.ImportSeedRelays, filter, nostr.SubscriptionOptions{})
			for ev := range events {
				if ctx.Err() != nil {
					break // Stop the loop on timeout
				}
				if _, ok := config.BlacklistedPubKeys[ev.PubKey.Hex()]; ok {
					slog.Debug("🚫 skipping event from blacklisted pubkey", "pubkey", ev.PubKey, "id", ev.ID)
					continue
				}
				if err := outboxDB.SaveEvent(ev.Event); err != nil {
					log.Println("🚫  error importing note", ev.ID, ":", err)
					nFailedImportNotes++
				}
				batchImportedNotes++
			}
			done <- batchImportedNotes
			close(done)
		}()

		select {
		case batchImportedNotes := <-done:
			ownerImportedNotes += batchImportedNotes
			if batchImportedNotes == 0 {
				log.Printf("ℹ️ No notes found for %s to %s", startTime.Format(layout), endTime.Format(layout))
			} else {
				log.Printf("📦 Imported %d notes from %s to %s", batchImportedNotes, startTime.Format(layout), endTime.Format(layout))
			}
		case <-ctx.Done():
			log.Printf("🚫 Timeout after %v while importing notes from %s to %s", timeout, startTime.Format(layout), endTime.Format(layout))
		}

		startTime = startTime.Add(240 * time.Hour)
		endTime = endTime.Add(240 * time.Hour)

		if startTime.After(time.Now()) {
			log.Println("✅ owner note import complete! Imported", ownerImportedNotes, "notes")
			break
		}
		if nFailedImportNotes > 0 {
			log.Printf("⚠️ Failed to import %d notes", nFailedImportNotes)
		}

		time.Sleep(1 * time.Second) // Avoid bombarding relays with too many requests
	}
}

func importTaggedNotes(ctx context.Context) {
	taggedImportedNotes := 0
	done := make(chan struct{}, 1)
	timeout := time.Duration(config.ImportTaggedNotesFetchTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inboxStore := inboxDB
	chatStore := chatDB
	filter := nostr.Filter{
		Tags: nostr.TagMap{
			"p": slices.Collect(maps.Keys(config.WhitelistedPubKeys)),
		},
	}

	log.Println("📦 importing inbox notes, please wait up to", timeout)

	go func() {
		events := pool.FetchMany(ctx, config.ImportSeedRelays, filter, nostr.SubscriptionOptions{})
		for ev := range events {
			if ctx.Err() != nil {
				break // Stop the loop on timeout
			}

			if _, ok := config.BlacklistedPubKeys[ev.PubKey.Hex()]; ok {
				slog.Debug("🚫 skipping tagged event from blacklisted pubkey", "pubkey", ev.PubKey, "id", ev.ID)
				continue
			}

			if !wot.GetInstance().Has(ctx, ev.PubKey.Hex()) && ev.Kind != nostr.KindGiftWrap {
				continue
			}
			for tag := range ev.Tags.FindAll("p") {
				if len(tag) < 2 {
					continue
				}
				if _, ok := config.WhitelistedPubKeys[tag[1]]; ok {
					dbToWrite := inboxStore
					if ev.Kind == nostr.KindGiftWrap {
						dbToWrite = chatStore
					}
					if err := dbToWrite.SaveEvent(ev.Event); err != nil {
						log.Println("🚫 error importing tagged note", ev.ID, ":", err)
					}
					taggedImportedNotes++
				}
			}
		}
		close(done)
	}()

	select {
	case <-done:
		log.Println("📦 imported", taggedImportedNotes, "tagged notes")
	case <-ctx.Done():
		log.Println("🚫 Timeout after", timeout, "while importing tagged notes")
	}

	log.Println("✅ tagged import complete")
}

func subscribeInboxAndChat(ctx context.Context) {
	startTime := nostr.Timestamp(time.Now().Add(-time.Minute * 5).Unix())
	filter := nostr.Filter{
		Tags: nostr.TagMap{
			"p": slices.Collect(maps.Keys(config.WhitelistedPubKeys)),
		},
		Since: startTime,
	}

	log.Println("📢 subscribing to inbox")

	for ev := range pool.SubscribeMany(ctx, config.ImportSeedRelays, filter, nostr.SubscriptionOptions{}) {
		if _, ok := config.BlacklistedPubKeys[ev.PubKey.Hex()]; ok {
			slog.Debug("🚫discarding imported note from blacklisted pubkey", "pubkey", ev.PubKey, "id", ev.ID)
			continue
		}
		if !wot.GetInstance().Has(ctx, ev.PubKey.Hex()) && ev.Kind != nostr.KindGiftWrap {
			continue
		}
		for tag := range ev.Tags.FindAll("p") {
			if len(tag) < 2 {
				continue
			}
			if _, ok := config.WhitelistedPubKeys[tag[1]]; ok {
				dbToPublish := inboxDB
				if ev.Kind == nostr.KindGiftWrap {
					dbToPublish = chatDB
				}

				slog.Debug("ℹ️ importing event", "kind", ev.Kind, "id", ev.ID, "relay", ev.Relay.URL)

				if isDuplicate(ctx, dbToPublish, ev.Event) {
					slog.Debug("ℹ️ skipping duplicate event", "id", ev.ID)
					break // Avoid re-importing duplicates
				}

				if err := dbToPublish.SaveEvent(ev.Event); err != nil {
					log.Println("🚫 error importing tagged note", ev.ID, ":", "from relay", ev.Relay.URL, ":", err)
					break
				}

				switch ev.Kind {
				case nostr.KindTextNote:
					log.Println("📰 new note in your inbox")
				case nostr.KindReaction:
					log.Println(ev.Content, "new reaction in your inbox")
				case nostr.KindZap:
					log.Println("⚡️ new zap in your inbox")
				case nostr.KindEncryptedDirectMessage:
					log.Println("🔒✉️ new encrypted message in your inbox")
				case nostr.KindGiftWrap:
					log.Println("🎁🔒️✉️ new gift-wrapped message in your chat relay")
				case nostr.KindRepost:
					log.Println("🔁 new repost in your inbox")
				case nostr.KindFollowList:
					// do nothing
				default:
					log.Println("📦 new event kind", ev.Kind, "event in your inbox")
				}
			}
		}
	}
}

func isDuplicate(ctx context.Context, db DBBackend, event nostr.Event) bool {
	filter := nostr.Filter{
		IDs:   []nostr.ID{event.ID},
		Since: event.CreatedAt,
		Limit: 1,
	}

	for range db.QueryEvents(filter, 1) {
		return true
	}

	return false
}
