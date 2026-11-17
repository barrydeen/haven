package main

import (
	"context"
	"log/slog"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/blossom"
	nipb7blossom "fiatjaf.com/nostr/nipb7/blossom"
)

// migrateBlossomMetadata moves the owner's blob descriptors out of the outbox
// database, where older versions of haven kept them, and into the dedicated
// blossom one.
//
// It walks the outbox database with the paged reader rather than the blob
// index's own List. List issues a single query with no limit, and both event
// store engines answer that with a quarter of their maximum — so this used to
// migrate at most a few hundred descriptors per boot and report success.
func migrateBlossomMetadata(ctx context.Context, bl *blossom.BlossomServer) {
	ownerPubkey := nPubToPubkey("OWNER_NPUB", config.OwnerNpub)
	ownerKey, err := nostr.PubKeyFromHex(ownerPubkey)
	if err != nil {
		slog.Error("🚫 owner pubkey is not valid hex; blob metadata migration is skipped", "error", err)
		return
	}

	// everything is collected before anything is written or deleted: the walk
	// below holds a cursor into the outbox database, and deleting from
	// underneath it would move the ground it is standing on
	var events []*nostr.Event
	if _, _, err := eachEvent(ctx, outboxDB, nostr.Filter{
		Authors: []nostr.PubKey{ownerKey},
		Kinds:   []nostr.Kind{blobIndexKind},
	}, maxAggregateScan, func(evt *nostr.Event) bool {
		events = append(events, evt)
		return true
	}); err != nil {
		slog.Error("🚫 Failed to list blobs", "error", err)
		return
	}

	if len(events) == 0 {
		slog.Debug("No blobs found to migrate", "ownerPubkey", ownerPubkey)
		return
	}

	slog.Info("BlobDescriptors will be migrated from Outbox to Blossom's DB", "count", len(events))

	migrated := 0
	for _, evt := range events {
		parsed, ok := parseBlobIndexEvent(evt)
		if !ok {
			// left where it is rather than deleted: we could not read it, so we
			// are in no position to decide it is worthless
			slog.Warn("⚠️ skipping an unreadable blob index entry", "event", evt.ID)
			continue
		}

		blob := nipb7blossom.BlobDescriptor{
			SHA256:   parsed.SHA256,
			Size:     parsed.Size,
			Type:     parsed.Type,
			Uploaded: parsed.Uploaded,
		}
		if blob.Type == "" {
			blob.Type = "application/octet-stream"
		}
		blob.URL = bl.ServiceURL + "/" + blob.SHA256 + blobExtension(blob.Type)

		slog.Debug("Moving BlobDescriptor", "sha256", blob.SHA256, "type", blob.Type, "size", blob.Size)

		if err := bl.Store.Keep(ctx, blob, ownerKey); err != nil {
			slog.Error("🚫 Failed to store blob in Blossom DB", "sha256", blob.SHA256, "error", err)
			continue
		}

		// the event came out of the store, so it still carries every field the
		// backend needs to take its index keys back out again
		if err := outboxDB.DeleteEvent(ctx, evt); err != nil {
			slog.Error("🚫 Failed to delete blob from outbox DB", "sha256", blob.SHA256, "error", err)
		}

		migrated++
	}

	blobInventory.invalidate()
	slog.Info("✅ Blob migration completed", "migrated", migrated)
}
