package main

import (
	"fmt"

	dgbadger "github.com/dgraph-io/badger/v4"
)

// BadgerTuning holds optional BadgerDB memory settings. A zero value means
// "keep the BadgerDB default", so a Haven without any BADGER_* variable behaves
// exactly as before.
//
// Haven opens five independent BadgerDB instances (private, chat, outbox, inbox
// and blossom). BadgerDB's defaults are sized for a dedicated database server,
// and they are multiplied by five here: 5 x (up to 5 x 64 MB memtables + 256 MB
// block cache + 4 compactors). On a small box (Raspberry Pi, 1 GB VPS, a
// notebook running other services) that is a lot of reserved memory for a
// database that is usually a few tens of megabytes.
type BadgerTuning struct {
	Profile          string
	MemTableSizeMB   int
	NumMemtables     int
	BlockCacheMB     int
	IndexCacheMB     int
	NumCompactors    int
	ValueLogFileMB   int
	NumLevelZeroTabs int
}

const (
	badgerProfileDefault   = "default"
	badgerProfileLowMemory = "low-memory"
)

// lowMemoryTuning is the preset selected by BADGER_PROFILE=low-memory.
func lowMemoryTuning() BadgerTuning {
	return BadgerTuning{
		MemTableSizeMB:   8,
		NumMemtables:     2,
		BlockCacheMB:     16,
		IndexCacheMB:     8,
		NumCompactors:    2,
		ValueLogFileMB:   64,
		NumLevelZeroTabs: 2,
	}
}

// resolve merges the selected profile with the explicit overrides (an explicit
// non-zero value always wins over the profile).
func (t BadgerTuning) resolve() (BadgerTuning, error) {
	var base BadgerTuning
	switch t.Profile {
	case "", badgerProfileDefault:
	case badgerProfileLowMemory:
		base = lowMemoryTuning()
	default:
		return t, fmt.Errorf("invalid BADGER_PROFILE %q (use %q or %q)", t.Profile, badgerProfileDefault, badgerProfileLowMemory)
	}
	pick := func(explicit, preset int) int {
		if explicit != 0 {
			return explicit
		}
		return preset
	}
	out := BadgerTuning{
		Profile:          t.Profile,
		MemTableSizeMB:   pick(t.MemTableSizeMB, base.MemTableSizeMB),
		NumMemtables:     pick(t.NumMemtables, base.NumMemtables),
		BlockCacheMB:     pick(t.BlockCacheMB, base.BlockCacheMB),
		IndexCacheMB:     pick(t.IndexCacheMB, base.IndexCacheMB),
		NumCompactors:    pick(t.NumCompactors, base.NumCompactors),
		ValueLogFileMB:   pick(t.ValueLogFileMB, base.ValueLogFileMB),
		NumLevelZeroTabs: pick(t.NumLevelZeroTabs, base.NumLevelZeroTabs),
	}
	if out.NumCompactors == 1 {
		return out, fmt.Errorf("BADGER_NUM_COMPACTORS must be 0 (default) or at least 2")
	}
	return out, nil
}

// badgerOptionsModifier returns a function suitable for
// BadgerBackend.BadgerOptionsModifier, or nil when nothing is customised.
func (t BadgerTuning) badgerOptionsModifier() (func(dgbadger.Options) dgbadger.Options, error) {
	r, err := t.resolve()
	if err != nil {
		return nil, err
	}
	if r == (BadgerTuning{Profile: r.Profile}) {
		return nil, nil
	}
	return func(o dgbadger.Options) dgbadger.Options {
		if r.MemTableSizeMB > 0 {
			o.MemTableSize = int64(r.MemTableSizeMB) << 20
		}
		if r.NumMemtables > 0 {
			o.NumMemtables = r.NumMemtables
		}
		if r.BlockCacheMB > 0 {
			o.BlockCacheSize = int64(r.BlockCacheMB) << 20
		}
		if r.IndexCacheMB > 0 {
			o.IndexCacheSize = int64(r.IndexCacheMB) << 20
		}
		if r.NumCompactors > 0 {
			o.NumCompactors = r.NumCompactors
		}
		if r.ValueLogFileMB > 0 {
			o.ValueLogFileSize = int64(r.ValueLogFileMB) << 20
		}
		if r.NumLevelZeroTabs > 0 {
			o.NumLevelZeroTables = r.NumLevelZeroTabs
			// the stall threshold must stay above the compaction trigger
			if o.NumLevelZeroTablesStall <= r.NumLevelZeroTabs {
				o.NumLevelZeroTablesStall = r.NumLevelZeroTabs * 3
			}
		}
		// BadgerDB refuses to open when ValueThreshold exceeds 15% of the
		// memtable (its max batch size), so a smaller memtable needs a
		// smaller threshold. Events are tiny; larger values spill to the
		// value log, which is the intended path for big payloads.
		if maxBatch := o.MemTableSize * 15 / 100; o.ValueThreshold > maxBatch {
			o.ValueThreshold = maxBatch
		}
		return o
	}, nil
}

// loadBadgerTuning reads the BADGER_* environment variables.
func loadBadgerTuning() BadgerTuning {
	return BadgerTuning{
		Profile:          getEnvString("BADGER_PROFILE", badgerProfileDefault),
		MemTableSizeMB:   getEnvInt("BADGER_MEMTABLE_SIZE_MB", 0),
		NumMemtables:     getEnvInt("BADGER_NUM_MEMTABLES", 0),
		BlockCacheMB:     getEnvInt("BADGER_BLOCK_CACHE_MB", 0),
		IndexCacheMB:     getEnvInt("BADGER_INDEX_CACHE_MB", 0),
		NumCompactors:    getEnvInt("BADGER_NUM_COMPACTORS", 0),
		ValueLogFileMB:   getEnvInt("BADGER_VALUE_LOG_FILE_SIZE_MB", 0),
		NumLevelZeroTabs: getEnvInt("BADGER_NUM_LEVEL_ZERO_TABLES", 0),
	}
}
