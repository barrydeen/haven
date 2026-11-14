package main

import (
	"testing"

	dgbadger "github.com/dgraph-io/badger/v4"
)

func TestBadgerTuningDefaultIsNoop(t *testing.T) {
	m, err := BadgerTuning{Profile: badgerProfileDefault}.badgerOptionsModifier()
	if err != nil || m != nil {
		t.Fatalf("default profile must not customise anything, got modifier=%v err=%v", m != nil, err)
	}
}

func TestBadgerTuningLowMemoryProfileAndOverride(t *testing.T) {
	m, err := BadgerTuning{Profile: badgerProfileLowMemory, BlockCacheMB: 32}.badgerOptionsModifier()
	if err != nil || m == nil {
		t.Fatalf("expected a modifier, err=%v", err)
	}
	o := m(dgbadger.DefaultOptions(t.TempDir()))
	if o.MemTableSize != 8<<20 || o.NumMemtables != 2 {
		t.Errorf("memtable not applied: %d x %d", o.MemTableSize, o.NumMemtables)
	}
	if o.BlockCacheSize != 32<<20 {
		t.Errorf("explicit override must win over profile, got %d", o.BlockCacheSize)
	}
	if o.ValueThreshold > o.MemTableSize*15/100 {
		t.Errorf("value threshold %d exceeds max batch size", o.ValueThreshold)
	}
	if o.NumLevelZeroTablesStall <= o.NumLevelZeroTables {
		t.Errorf("L0 stall %d must exceed L0 tables %d", o.NumLevelZeroTablesStall, o.NumLevelZeroTables)
	}
}

func TestBadgerTuningRejectsBadInput(t *testing.T) {
	if _, err := (BadgerTuning{Profile: "turbo"}).badgerOptionsModifier(); err == nil {
		t.Error("unknown profile must error")
	}
	if _, err := (BadgerTuning{NumCompactors: 1}).badgerOptionsModifier(); err == nil {
		t.Error("a single compactor must error")
	}
}

// The tuned options must actually open and serve a database.
func TestBadgerTuningOpens(t *testing.T) {
	m, _ := BadgerTuning{Profile: badgerProfileLowMemory}.badgerOptionsModifier()
	o := m(dgbadger.DefaultOptions(t.TempDir()).WithLogger(nil))
	db, err := dgbadger.Open(o)
	if err != nil {
		t.Fatalf("badger refused the low-memory options: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
}
