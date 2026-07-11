package main

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
)

func (z *zipWriter) close() {
	if err := z.w.Close(); err != nil {
		slog.Error("❌ error closing zip writer", "error", err)
	}
	if err := z.f.Close(); err != nil {
		slog.Error("❌ error closing zip file", "error", err)
	}
}

func exportToZip(ctx context.Context, zipFileName string) error {
	slog.Info("🛫 starting export", "file", zipFileName)
	f, err := os.Create(zipFileName)
	if err != nil {
		return fmt.Errorf("error creating zip file: %w", err)
	}

	zw := zip.NewWriter(f)
	z := &zipWriter{f: f, w: zw}
	defer z.close()

	for name, db := range dbs {
		fileName := name + ".jsonl"
		slog.Info("📦 exporting db to fileName", "fileName", fileName)

		header := &zip.FileHeader{
			Name:     fileName,
			Method:   zip.Deflate,
			Modified: time.Now(),
		}

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("error creating zip entry %s: %w", fileName, err)
		}

		if err := exportDB(ctx, db, writer); err != nil {
			return fmt.Errorf("error exporting %s: %w", fileName, err)
		}
	}

	slog.Info("✅ export complete", "file", zipFileName)
	return nil
}

func exportToJSONL(ctx context.Context, relayName, jsonlFileName string) error {
	slog.Info("🛫 starting export", "relay", relayName, "file", jsonlFileName)
	db, ok := dbs[relayName]
	if !ok {
		return fmt.Errorf("unknown relay: %s", relayName)
	}

	f, err := os.Create(jsonlFileName)
	if err != nil {
		return fmt.Errorf("error creating jsonl file: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("❌ error closing jsonl file", "error", err)
		}
	}()

	if err := exportDB(ctx, db, f); err != nil {
		return fmt.Errorf("error exporting %s: %w", relayName, err)
	}

	slog.Info("✅ export complete", "file", jsonlFileName)
	return nil
}

func importFromZip(ctx context.Context, zipFileName string) error {
	slog.Info("🛬 starting import", "file", zipFileName)

	zipFile, err := zip.OpenReader(zipFileName)
	if err != nil {
		return fmt.Errorf("error opening zip file: %w", err)
	}
	defer func() {
		if err := zipFile.Close(); err != nil {
			slog.Error("❌ error closing zip file", "error", err)
		}
	}()

	for _, file := range zipFile.File {
		if !strings.HasSuffix(file.Name, ".jsonl") {
			slog.Warn("⏭️ skipping unknown file in zip", "file", file.Name)
			continue
		}
		relayName := strings.TrimSuffix(file.Name, ".jsonl")
		db, ok := dbs[relayName]
		if !ok {
			slog.Warn("⏭️ skipping file for unknown relay type", "file", file.Name)
			continue
		}

		slog.Info("📦 importing file to db", "file", file.Name)

		if err := importEntry(ctx, db, file); err != nil {
			return err
		}
	}

	slog.Info("✅ import complete", "file", zipFileName)
	return nil
}

func importEntry(ctx context.Context, db DBBackend, file *zip.File) error {
	rc, err := file.Open()
	if err != nil {
		return fmt.Errorf("error opening zip entry %s: %w", file.Name, err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("❌ error closing zip entry", "file", file.Name, "error", err)
		}
	}()

	if err := importDB(ctx, db, rc); err != nil {
		return fmt.Errorf("error importing %s: %w", file.Name, err)
	}
	return nil
}

func importFromJSONL(ctx context.Context, relayName, jsonlFileName string) error {
	slog.Info("🛬 starting import", "relay", relayName, "file", jsonlFileName)
	db, ok := dbs[relayName]
	if !ok {
		return fmt.Errorf("unknown relay: %s", relayName)
	}

	f, err := os.Open(jsonlFileName)
	if err != nil {
		return fmt.Errorf("error opening jsonl file: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("❌ error closing jsonl file", "error", err)
		}
	}()

	if err := importDB(ctx, db, f); err != nil {
		return fmt.Errorf("error importing %s: %w", relayName, err)
	}

	slog.Info("✅ import complete", "file", jsonlFileName)
	return nil
}

type zipWriter struct {
	f *os.File
	w *zip.Writer
}

func importDB(ctx context.Context, db DBBackend, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// Nostr events can be large, increase buffer size if necessary.
	// Default is 64KB, which might be enough for most events, but let's be safe.
	const maxCapacity = 100 * 1024 * 1024 // 100MB
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxCapacity)

	count := 0
	for scanner.Scan() {
		var event nostr.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}

		if err := db.SaveEvent(event); err != nil {
			if errors.Is(err, eventstore.ErrDupEvent) {
				slog.Debug("⏭️ skipping duplicate event", "id", event.ID)
				continue
			}
			return err
		}
		count++
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	slog.Info("📥 imported events", "count", count)
	return nil
}

func exportDB(ctx context.Context, db DBBackend, w io.Writer) error {
	const limit = 1000
	var lastTimestamp nostr.Timestamp
	count := 0

	var eventBuffer []nostr.Event

	flushBuffer := func() error {
		for _, e := range eventBuffer {
			if _, err := fmt.Fprintln(w, e); err != nil {
				return err
			}
			count++
		}
		eventBuffer = eventBuffer[:0]
		return nil
	}

	for {
		filter := nostr.Filter{
			Limit: limit,
		}
		if lastTimestamp != 0 {
			filter.Until = lastTimestamp
		}

		events := db.QueryEvents(filter, limit)

		initialCount := count
		initialBufferSize := len(eventBuffer)

		for event := range events {
			if len(eventBuffer) > 0 && event.CreatedAt != eventBuffer[0].CreatedAt {
				if err := flushBuffer(); err != nil {
					return err
				}
			}

			// Insert into eventBuffer maintaining ID order and skipping duplicates.
			// This works around eventstore LMDB backend that don't guarantee NIP-01
			// REQ order when paginating with a limit.
			//
			// This ensures that:
			// 1. Identical JSONL exports are produced (checksum guarantee) regardless of the
			//    underlying DB implementation.
			// 2. Roundtrip consistency is maintained when importing back into a DB, even
			//    if the original source had duplicates (e.g. due to bugs in older versions
			//    of eventstore and khatru).
			pos, found := slices.BinarySearchFunc(eventBuffer, event, func(a, b nostr.Event) int {
				return nostr.CompareEvent(a, b)
			})
			if !found {
				eventBuffer = slices.Insert(eventBuffer, pos, event)
			} else {
				slog.Debug("⏭️ skipping duplicated event", "id", event.ID)
			}

			lastTimestamp = event.CreatedAt
		}

		if count == initialCount && len(eventBuffer) == initialBufferSize {
			break
		}
	}

	if err := flushBuffer(); err != nil {
		return err
	}

	slog.Info("📤 exported events", "count", count)

	return nil
}
