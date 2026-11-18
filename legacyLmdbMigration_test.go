package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/codec/betterbinary"
	"github.com/PowerDNS/lmdb-go/lmdb"
)

// legacyMarshal replicates the deprecated eventstore@v0.17.15 internal/binary
// Marshal exactly, including the ONE TRAILING ZERO BYTE after each tag item.
func legacyMarshal(evt nostr.Event) []byte {
	buf := make([]byte, 0, 136+len(evt.Content)+len(evt.Tags)*64)
	buf = append(buf, evt.ID[:]...)
	buf = append(buf, evt.PubKey[:]...)
	buf = append(buf, evt.Sig[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(evt.CreatedAt))
	buf = binary.BigEndian.AppendUint16(buf, uint16(evt.Kind))
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(evt.Content)))
	buf = append(buf, evt.Content...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(evt.Tags)))
	for _, tag := range evt.Tags {
		buf = append(buf, uint8(len(tag)))
		for _, item := range tag {
			buf = binary.BigEndian.AppendUint16(buf, uint16(len(item)))
			buf = append(buf, item...)
			buf = append(buf, 0)
		}
	}
	return buf
}

// signedTestEvent returns an event whose ID matches its own hash, so both the
// legacy and the successor codec path can be exercised with hash validation on.
func signedTestEvent(t *testing.T, content string, tags nostr.Tags) nostr.Event {
	t.Helper()
	sk := nostr.Generate()
	evt := nostr.Event{
		Kind:      1,
		CreatedAt: nostr.Timestamp(1700000000),
		Content:   content,
		Tags:      tags,
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("failed to sign test event: %v", err)
	}
	if !evt.CheckID() {
		t.Fatal("test event does not hash-validate; test setup is broken")
	}
	return evt
}

func openTestLegacyEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db", "testenv")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	env, err := lmdb.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetMaxDBs(12); err != nil {
		t.Fatalf("SetMaxDBs: %v", err)
	}
	if err := env.Open(path, lmdb.NoTLS, 0644); err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := env.Update(func(txn *lmdb.Txn) error {
		if _, err := txn.OpenDBI("settings", lmdb.Create); err != nil {
			return err
		}
		if _, err := txn.OpenDBI("raw", lmdb.Create); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("creating dbis: %v", err)
	}
	return path
}

func writeTestRawBlobs(t *testing.T, path string, blobs map[string][]byte, legacyMarker bool) {
	t.Helper()
	env, err := lmdb.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetMaxDBs(12); err != nil {
		t.Fatalf("SetMaxDBs: %v", err)
	}
	if err := env.Open(path, lmdb.NoTLS, 0644); err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	err = env.Update(func(txn *lmdb.Txn) error {
		settings, err := txn.OpenDBI("settings", 0)
		if err != nil {
			return err
		}
		raw, err := txn.OpenDBI("raw", 0)
		if err != nil {
			return err
		}
		for key, blob := range blobs {
			if err := txn.Put(raw, []byte(key), blob, 0); err != nil {
				return err
			}
		}
		if legacyMarker {
			// legacy value: big-endian uint16 version 9 under the 1-byte key 'v'
			if err := txn.Put(settings, legacyVersionKey, []byte{0, 9}, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seeding raw blobs: %v", err)
	}
}

func readTestRawBlob(t *testing.T, path string, key string) []byte {
	t.Helper()
	env, err := lmdb.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetMaxDBs(12); err != nil {
		t.Fatalf("SetMaxDBs: %v", err)
	}
	if err := env.Open(path, lmdb.NoTLS, 0644); err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	var out []byte
	err = env.View(func(txn *lmdb.Txn) error {
		raw, err := txn.OpenDBI("raw", 0)
		if err != nil {
			return err
		}
		v, err := txn.Get(raw, []byte(key))
		if err != nil {
			return err
		}
		out = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		t.Fatalf("reading raw blob %s: %v", key, err)
	}
	return out
}

func getTestSetting(t *testing.T, path string, key []byte) ([]byte, bool) {
	t.Helper()
	env, err := lmdb.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetMaxDBs(12); err != nil {
		t.Fatalf("SetMaxDBs: %v", err)
	}
	if err := env.Open(path, lmdb.NoTLS, 0644); err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	var out []byte
	err = env.View(func(txn *lmdb.Txn) error {
		settings, err := txn.OpenDBI("settings", 0)
		if err != nil {
			return err
		}
		v, err := txn.Get(settings, key)
		if err != nil {
			return err
		}
		out = append([]byte(nil), v...)
		return nil
	})
	if lmdb.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("reading setting %q: %v", key, err)
	}
	return out, true
}

// TestLegacyMarshalRoundTrip checks legacyMarshal against the decoder used by
// the migration: whatever the old Marshal produced must come back intact.
func TestLegacyMarshalRoundTrip(t *testing.T) {
	evt := signedTestEvent(t, "hello legacy world", nostr.Tags{
		nostr.Tag{"e", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		nostr.Tag{"p", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "wss://relay.example.com"},
		nostr.Tag{}, // an empty tag, the degenerate case
	})

	decoded, err := legacyBinaryUnmarshal(legacyMarshal(evt))
	if err != nil {
		t.Fatalf("legacy decode failed: %v", err)
	}
	if decoded.ID != evt.ID {
		t.Fatalf("id mismatch after round-trip: %s != %s", decoded.ID, evt.ID)
	}
	if decoded.PubKey != evt.PubKey {
		t.Fatal("pubkey mismatch after round-trip")
	}
	if decoded.Sig != evt.Sig {
		t.Fatal("sig mismatch after round-trip")
	}
	if decoded.CreatedAt != evt.CreatedAt || decoded.Kind != evt.Kind {
		t.Fatal("timestamp or kind mismatch after round-trip")
	}
	if decoded.Content != evt.Content {
		t.Fatalf("content mismatch after round-trip: %q != %q", decoded.Content, evt.Content)
	}
	if len(decoded.Tags) != len(evt.Tags) {
		t.Fatalf("tag count mismatch after round-trip: %d != %d", len(decoded.Tags), len(evt.Tags))
	}
	for i := range evt.Tags {
		if len(decoded.Tags[i]) != len(evt.Tags[i]) {
			t.Fatalf("tag %d item count mismatch after round-trip", i)
		}
		for j := range evt.Tags[i] {
			if decoded.Tags[i][j] != evt.Tags[i][j] {
				t.Fatalf("tag %d item %d mismatch after round-trip: %q != %q", i, j, decoded.Tags[i][j], evt.Tags[i][j])
			}
		}
	}
	if !decoded.CheckID() {
		t.Fatal("round-tripped event does not hash-validate")
	}
}

// TestLegacyLmdbMigration covers conversion, the version markers, idempotence
// and the leave-me-alone path for blobs already in the successor codec.
func TestLegacyLmdbMigration(t *testing.T) {
	path := openTestLegacyEnv(t)

	legacyEvt := signedTestEvent(t, "a legacy note", nostr.Tags{
		nostr.Tag{"e", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	newEvt := signedTestEvent(t, "already in the successor codec", nil)
	corrupt := []byte("this is not a codec blob at all, just noise bytes padding")

	newBuf := make([]byte, betterbinary.Measure(newEvt))
	if err := betterbinary.Marshal(newEvt, newBuf); err != nil {
		t.Fatalf("failed to encode new-format event: %v", err)
	}

	writeTestRawBlobs(t, path, map[string][]byte{
		"\x00\x00\x00\x01": legacyMarshal(legacyEvt),
		"\x00\x00\x00\x02": newBuf,
		"\x00\x00\x00\x03": corrupt,
	}, true)

	converted, err := migrateLegacyRawEvents([]string{path})
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if converted != 1 {
		t.Fatalf("expected 1 converted blob, got %d", converted)
	}

	// (a) the legacy blob now decodes as betterbinary, hash-validates, and kept
	// its identity
	raw := readTestRawBlob(t, path, "\x00\x00\x00\x01")
	var evt nostr.Event
	if err := betterbinary.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("converted blob does not decode with betterbinary: %v", err)
	}
	if !evt.CheckID() {
		t.Fatal("converted blob does not hash-validate")
	}
	if evt.ID != legacyEvt.ID {
		t.Fatalf("converted id changed: %s != %s", evt.ID, legacyEvt.ID)
	}
	if evt.Content != legacyEvt.Content || evt.Sig != legacyEvt.Sig || evt.PubKey != legacyEvt.PubKey {
		t.Fatal("converted blob lost fields")
	}

	// (b) the legacy 'v' marker is gone
	if v, present := getTestSetting(t, path, legacyVersionKey); present {
		t.Fatalf("legacy 'v' marker still present after migration: %v", v)
	}

	// (c) the nostrlib version marker is 1 (big-endian uint16), which is below
	// the successor's target, so its own reindex will run on the next Init
	v, present := getTestSetting(t, path, []byte("version"))
	if !present {
		t.Fatal("nostrlib 'version' marker missing after migration")
	}
	if len(v) != 2 || binary.BigEndian.Uint16(v) != 1 {
		t.Fatalf("nostrlib 'version' marker is %v, want big-endian uint16 1", v)
	}

	// (d) idempotent: the second run converts nothing
	converted, err = migrateLegacyRawEvents([]string{path})
	if err != nil {
		t.Fatalf("second migration pass failed: %v", err)
	}
	if converted != 0 {
		t.Fatalf("second pass converted %d blobs, want 0", converted)
	}

	// (e) a blob already in the successor codec is left byte-for-byte untouched
	if raw := readTestRawBlob(t, path, "\x00\x00\x00\x02"); !bytes.Equal(raw, newBuf) {
		t.Fatal("already-converted blob was rewritten")
	}
}

// TestLegacyLmdbMigrationSkipsCorrupt checks that a blob readable by neither
// codec is skipped, left as-is, and does not fail the migration.
func TestLegacyLmdbMigrationSkipsCorrupt(t *testing.T) {
	path := openTestLegacyEnv(t)

	// hash-mismatching legacy bytes: valid legacy layout, wrong id
	badEvt := signedTestEvent(t, "hash will not match", nil)
	badBlob := legacyMarshal(badEvt)
	badBlob[0] ^= 0xff // corrupt the stored id, CheckID will fail

	corrupt := []byte{1, 2, 3, 4, 5, 6, 7, 8} // far too short for any codec

	writeTestRawBlobs(t, path, map[string][]byte{
		"\x00\x00\x00\x01": badBlob,
		"\x00\x00\x00\x02": corrupt,
	}, true)

	converted, err := migrateLegacyRawEvents([]string{path})
	if err != nil {
		t.Fatalf("migration should not fail on undecodable blobs: %v", err)
	}
	if converted != 0 {
		t.Fatalf("converted %d blobs, want 0", converted)
	}

	if raw := readTestRawBlob(t, path, "\x00\x00\x00\x01"); !bytes.Equal(raw, badBlob) {
		t.Fatal("hash-mismatching legacy blob was rewritten")
	}
	if raw := readTestRawBlob(t, path, "\x00\x00\x00\x02"); !bytes.Equal(raw, corrupt) {
		t.Fatal("corrupt blob was rewritten")
	}

	// legacy still: the 'v' marker is dropped, and since nothing was converted
	// the version marker is still written so the successor reindexes the
	// remaining blobs once
	if _, present := getTestSetting(t, path, legacyVersionKey); present {
		t.Fatal("legacy 'v' marker should be dropped even with nothing converted")
	}
	v, present := getTestSetting(t, path, []byte("version"))
	if !present || len(v) != 2 || binary.BigEndian.Uint16(v) != 1 {
		t.Fatalf("'version' marker should be 1 after a legacy pass, got %v", v)
	}
}

// TestLegacyLmdbMigrationMissingPath checks that a path with no database is
// skipped without an error, as a fresh install's would be.
func TestLegacyLmdbMigrationMissingPath(t *testing.T) {
	converted, err := migrateLegacyRawEvents([]string{filepath.Join(t.TempDir(), "db", "nothing")})
	if err != nil {
		t.Fatalf("missing path should be skipped, got error: %v", err)
	}
	if converted != 0 {
		t.Fatalf("missing path converted %d blobs, want 0", converted)
	}
}
