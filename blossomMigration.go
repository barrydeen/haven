package main

import (
	"context"
	"log/slog"

	"github.com/fiatjaf/khatru/blossom"
)

func migrateBlossomMetadata(ctx context.Context, bl *blossom.BlossomServer) {
	// Create a temporary Blossom dbWrapper for the migration
	outboxDBWrapper := blossom.EventStoreBlobIndexWrapper{Store: outboxDB, ServiceURL: getHTTPScheme(config.RelayURL) + config.RelayURL}

	// List all BlobDescriptor for the relay owner pubkey
	ownerPubkey := nPubToPubkey("OWNER_NPUB", config.OwnerNpub)
	blobsChan, err := outboxDBWrapper.List(ctx, ownerPubkey)
	if err != nil {
		slog.Error("🚫 Failed to list blobs", "error", err)
		return
	}
	var blobs []blossom.BlobDescriptor
	for blob := range blobsChan {
		blobs = append(blobs, blob)
	}

	if len(blobs) == 0 {
		slog.Debug("No blobs found to migrate", "ownerPubkey", ownerPubkey)
		return
	}

	// Create a map to track migrated blobs
	migrated := make(map[string]blossom.BlobDescriptor, len(blobs))

	slog.Info("BlobDescriptors will be migrated from Outbox to Blossom's DB", "count", len(blobs))

	for _, blob := range blobs {
		slog.Debug("Moving BlobDescriptor", "sha256", blob.SHA256, "type", blob.Type, "size", blob.Size)

		if blob.Type == "" {
			blob.Type = "application/octet-stream"
		}

		err := bl.Store.Keep(ctx, blob, ownerPubkey)
		if err != nil {
			slog.Error("🚫 Failed to store blob in Blossom DB", "sha256", blob.SHA256, "error", err)
			continue
		}

		err = outboxDBWrapper.Delete(ctx, blob.SHA256, ownerPubkey)
		if err != nil {
			slog.Error("🚫 Failed to delete blob from outbox DB", "sha256", blob.SHA256, "error", err)
		}

		migrated[blob.SHA256] = blob
	}

	slog.Info("✅ Blob migration completed", "migrated", len(migrated))
}
