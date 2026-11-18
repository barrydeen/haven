package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/codec/betterbinary"
	"github.com/PowerDNS/lmdb-go/lmdb"
)

// The relay used to store events through github.com/fiatjaf/eventstore@v0.17.15,
// whose raw blob codec (internal/binary, big-endian) is incompatible with the
// successor's betterbinary codec (little-endian). The successor lmdb backend
// unmarshals every raw blob with betterbinary and never re-encodes, so legacy
// blobs either fail to decode (events silently dropped from queries) or decode
// into garbage. Its own migration cannot help: it looks up the version under
// the settings key "version" while legacy databases stored it under the single
// byte 'v', misses, and assumes the database is already current — and even if
// it ran, it only rebuilds indexes, never the raw values.
//
// migrateLegacyRawEvents re-encodes the "raw" dbi of each lmdb environment in
// place, from the legacy codec to betterbinary, under the same 4-byte serial
// keys. It runs before the package-level backends are opened (they only carry
// the path until Init), so nothing else has the environments open yet. It must
// run before initDBs for a second reason: writing version 1 makes the
// successor backend's own migrate() drop and rebuild every index from the now
// readable raw blobs on the next Init.
//
// Every blob is hash-validated (NIP-01 id check) before it is written back:
// a blob that decodes under neither codec, or whose decoded id does not match
// its own hash, is left untouched and counted. Nothing is rewritten on a
// second run: converted blobs decode as betterbinary and validate, and the
// 'v' marker is deleted, so the migration is idempotent.
func migrateLegacyRawEvents(paths []string) (converted int, err error) {
	for _, path := range paths {
		n, err := migrateLegacyRawEventsOne(path)
		converted += n
		if err != nil {
			// keep going: one environment failing to open (or to migrate) must
			// not stop the relay from booting and serving new events on the rest
			slog.Error("🚫 legacy lmdb migration failed for a database", "path", path, "error", err)
			err = fmt.Errorf("%s: %w", path, err)
		}
	}
	return converted, err
}

func migrateLegacyRawEventsOne(path string) (converted int, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			// fresh installs and bolt-backed deployments have nothing to convert
			slog.Debug("skipping legacy lmdb migration, no database at path", "path", path)
			return 0, nil
		}
		return 0, statErr
	}

	env, err := lmdb.NewEnv()
	if err != nil {
		return 0, err
	}
	defer env.Close()

	if err := env.SetMaxDBs(12); err != nil {
		return 0, err
	}
	if err := env.SetMaxReaders(1000); err != nil {
		return 0, err
	}
	if config.LmdbMapSize != 0 {
		if err := env.SetMapSize(config.LmdbMapSize); err != nil {
			return 0, err
		}
	} else {
		if err := env.SetMapSize(1 << 38); err != nil {
			return 0, err
		}
	}
	if err := env.Open(path, lmdb.NoTLS, 0644); err != nil {
		return 0, err
	}

	var scanned, skippedInvalid int
	err = env.Update(func(txn *lmdb.Txn) error {
		settings, err := txn.OpenDBI("settings", lmdb.Create)
		if err != nil {
			return err
		}
		raw, err := txn.OpenDBI("raw", 0)
		if err != nil {
			if lmdb.IsNotFound(err) {
				// nothing has ever been stored here
				return nil
			}
			return err
		}

		// legacy databases carry their version under the single-byte key 'v'
		// (big-endian uint16, value 9); the successor looks for "version"
		var legacyDB bool
		if _, err := txn.Get(settings, legacyVersionKey); err == nil {
			legacyDB = true
		} else if !lmdb.IsNotFound(err) {
			return err
		}

		// collect the serial keys first: putting new values into the same dbi
		// this cursor is walking would move the ground it stands on
		var serialKeys [][]byte
		cursor, err := txn.OpenCursor(raw)
		if err != nil {
			return err
		}
		for {
			k, _, err := cursor.Get(nil, nil, lmdb.Next)
			if lmdb.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			key := make([]byte, len(k))
			copy(key, k)
			serialKeys = append(serialKeys, key)
		}
		cursor.Close()

		for _, key := range serialKeys {
			scanned++
			blob, err := txn.Get(raw, key)
			if err != nil {
				return err
			}

			// already readable by the successor and consistent: leave it alone
			var evt nostr.Event
			if uerr := betterbinary.Unmarshal(blob, &evt); uerr == nil && evt.CheckID() {
				continue
			}

			evt, lerr := legacyBinaryUnmarshal(blob)
			if lerr != nil || !evt.CheckID() {
				skippedInvalid++
				slog.Warn("⚠️ skipping a legacy raw blob that does not hash-validate", "path", path, "serial", key, "error", lerr)
				continue
			}

			buf := make([]byte, betterbinary.Measure(evt))
			if err := betterbinary.Marshal(evt, buf); err != nil {
				return fmt.Errorf("failed to re-encode event %s: %w", evt.ID, err)
			}
			if err := txn.Put(raw, key, buf, 0); err != nil {
				return err
			}
			converted++
		}

		if legacyDB {
			// the successor would misread the byte 'v' as nothing it knows, and
			// its absence is what keeps a converted database from looking legacy
			if err := txn.Del(settings, legacyVersionKey, nil); err != nil && !lmdb.IsNotFound(err) {
				return err
			}
		}

		if converted > 0 || legacyDB {
			// version 1 is below the successor's target (2), so its own
			// migrate() drops all indexes and rebuilds them from the raw dbi on
			// the next Init. Only worth writing once: after that reindex the
			// version is already 2 and this write is skipped on later runs.
			if _, err := txn.Get(settings, nostrlibVersionKey); lmdb.IsNotFound(err) {
				var v [2]byte
				binary.BigEndian.PutUint16(v[:], 1)
				if err := txn.Put(settings, nostrlibVersionKey, v[:], 0); err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	if converted > 0 || skippedInvalid > 0 {
		slog.Info("🔁 legacy lmdb raw events converted to the successor codec",
			"path", path, "scanned", scanned, "converted", converted, "skippedInvalid", skippedInvalid)
	} else {
		slog.Debug("legacy lmdb migration found nothing to convert",
			"path", path, "scanned", scanned, "converted", converted, "skippedInvalid", skippedInvalid)
	}
	return converted, nil
}

// legacyBinaryUnmarshal decodes the deprecated github.com/fiatjaf/eventstore
// @v0.17.15 internal/binary layout, big-endian:
//
//	id[0:32] pk[32:64] sig[64:128] createdAt BE u32 [128:132] kind BE u16
//	[132:134] contentLen BE u16 [134:136] content[136:136+len] tagCount BE u16,
//	then per tag: 1 byte itemCount, then per item: 2-byte BE itemSize + item
//	bytes, with ONE TRAILING ZERO BYTE after each item (skipped with curr++
//	between items and between tags, exactly as the old Unmarshal did).
//
// The decoded event must still pass CheckID(): the caller refuses to write
// anything that does not hash-validate.
func legacyBinaryUnmarshal(data []byte) (nostr.Event, error) {
	var evt nostr.Event
	if len(data) < 136 {
		return evt, fmt.Errorf("blob too short for the legacy layout: %d bytes", len(data))
	}
	copy(evt.ID[:], data[0:32])
	copy(evt.PubKey[:], data[32:64])
	copy(evt.Sig[:], data[64:128])
	evt.CreatedAt = nostr.Timestamp(binary.BigEndian.Uint32(data[128:132]))
	evt.Kind = nostr.Kind(binary.BigEndian.Uint16(data[132:134]))
	contentLength := int(binary.BigEndian.Uint16(data[134:136]))
	if 136+contentLength > len(data) {
		return evt, fmt.Errorf("content length %d overflows blob of %d bytes", contentLength, len(data))
	}
	evt.Content = string(data[136 : 136+contentLength])
	curr := 136 + contentLength
	if curr+2 > len(data) {
		return evt, fmt.Errorf("tag count overflows blob at %d", curr)
	}
	nTags := int(binary.BigEndian.Uint16(data[curr : curr+2]))
	curr += 2
	evt.Tags = make(nostr.Tags, nTags)
	for t := 0; t < nTags; t++ {
		if curr+1 > len(data) {
			return evt, fmt.Errorf("tag %d overflows blob at %d", t, curr)
		}
		nItems := int(data[curr])
		curr++
		tag := make(nostr.Tag, nItems)
		for i := 0; i < nItems; i++ {
			if curr+2 > len(data) {
				return evt, fmt.Errorf("tag %d item %d overflows blob at %d", t, i, curr)
			}
			itemSize := int(binary.BigEndian.Uint16(data[curr : curr+2]))
			curr += 2
			if curr+itemSize > len(data) {
				return evt, fmt.Errorf("tag %d item %d data overflows blob at %d", t, i, curr)
			}
			tag[i] = string(data[curr : curr+itemSize])
			curr += itemSize
			// the old Marshal appends a trailing 0 byte after each item
			curr++
		}
		evt.Tags[t] = tag
	}
	return evt, nil
}

var (
	legacyVersionKey   = []byte{'v'}
	nostrlibVersionKey = []byte("version")
)
