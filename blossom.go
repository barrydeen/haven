package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fiatjaf/khatru/blossom"
	"github.com/nbd-wtf/go-nostr"
)

//
// where the bytes live
//

// blobPath is where one blob's bytes are stored. It concatenates rather than
// joins, which is what the upload hook has always done: BLOSSOM_PATH defaults to
// "blossom" with no trailing separator, so a relay running on the defaults has
// its blobs sitting *beside* the blossom directory as ./blossom<sha256>, and a
// filepath.Join here would quietly stop finding every file it had already
// written. checkBlobPath complains about that layout instead of correcting it.
func blobPath(sha256 string) string {
	return config.BlossomPath + sha256
}

// blobDirAndPrefix splits BLOSSOM_PATH the way blobPath's concatenation implies:
// "blossom/" means bare hashes inside blossom/, while "blossom" means names
// beginning with "blossom" in the working directory. The directory scan needs
// both halves, and deriving them here keeps one definition of the layout.
func blobDirAndPrefix() (dir, prefix string) {
	dir, prefix = filepath.Split(config.BlossomPath)
	if dir == "" {
		dir = "."
	}
	return dir, prefix
}

// checkBlobPath warns when BLOSSOM_PATH has no trailing separator. haven creates
// the directory and then writes blobs next to it rather than inside it, which is
// confusing, litters the working directory with one file per blob, and is not
// something we can quietly correct: every file already written would vanish from
// the media browser the moment we started joining paths properly.
func checkBlobPath() {
	path := config.BlossomPath
	if path != "" && os.IsPathSeparator(path[len(path)-1]) {
		return
	}
	slog.Warn("⚠️ BLOSSOM_PATH has no trailing slash, so blobs are stored beside that directory rather than inside it",
		"blossom_path", path,
		"example", blobPath("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"),
		"fix", "move the files into the directory and set BLOSSOM_PATH=\""+path+"/\"")
}

// writeBlob stores one blob. The bytes go to a temporary name and are renamed
// into place, so an upload that dies halfway leaves nothing behind instead of a
// truncated file that every later GET would serve under an ETag claiming to be
// the hash of the whole thing. The file is closed on every path — the hook this
// replaces leaked a descriptor per upload.
func writeBlob(sha256 string, body []byte) error {
	final := blobPath(sha256)
	tmp := final + ".tmp"

	file, err := fs.Create(tmp)
	if err != nil {
		return err
	}

	abandon := func(err error) error {
		_ = file.Close()
		_ = fs.Remove(tmp)
		return err
	}

	if _, err := io.Copy(file, bytes.NewReader(body)); err != nil {
		return abandon(err)
	}
	if err := file.Sync(); err != nil {
		return abandon(err)
	}
	if err := file.Close(); err != nil {
		_ = fs.Remove(tmp)
		return err
	}
	return fs.Rename(tmp, final)
}

// removeBlob deletes one blob's bytes. A file that is already gone is not an
// error: the index and the disk drift apart for all sorts of reasons, and the
// caller's intent is satisfied either way. It reports whether a file was
// actually there to remove.
func removeBlob(sha256 string) (bool, error) {
	if err := fs.Remove(blobPath(sha256)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// blobServiceURL is the base every public blob URL is built from. It is the same
// expression blossom.New is handed in initRelays, and deliberately not derived
// from the request: an owner reaching the relay through a tunnel is browsing
// localhost while the URL they share still has to name the relay.
func blobServiceURL() string {
	return getHTTPScheme(config.RelayURL) + config.RelayURL
}

// blobExtension mirrors khatru's unexported getExtension, which is what builds
// the URLs its own /list endpoint serves. It is copied rather than approximated
// because the special cases are load bearing: mime.ExtensionsByType("image/jpeg")
// answers ".jfif" first, and a URL we invent that disagrees with the one the
// relay serves is worse than no URL at all.
func blobExtension(mimetype string) string {
	if mimetype == "" {
		return ""
	}

	switch mimetype {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "application/vnd.android.package-archive":
		return ".apk"
	}

	exts, _ := mime.ExtensionsByType(mimetype)
	if len(exts) > 0 {
		return exts[0]
	}

	return ""
}

//
// reading the blob index
//

const (
	// blobIndexKind is the synthetic kind khatru's blob index keeps one event of
	// per (uploader, blob). The events are unsigned and never leave the relay.
	blobIndexKind = 24242

	// blobIndexMaxLimit is the query limit haven configures on the blob index
	// database. It is not a page size any caller passes: eachEvent in
	// eventscan.go reads it back through queryPageSize, so raising it here is
	// what makes the media browser's walk use 5000-event pages.
	//
	// Both event store backends cap a query at MaxLimit — 1500 on lmdb, 1000 on
	// badger when the caller leaves it zero, which everywhere else in haven does
	// — and a filter asking for more than the cap is not clamped to it but
	// silently dropped to a quarter of it. So a plain "give me everything" query
	// against the blob index answers with 375 blobs, newest first, and nothing
	// says the rest exist. Raising it is safe here because this database is not
	// any relay's store: only the blossom index and the media browser read it.
	//
	// Both backends also mirror MaxLimit into MaxLimitNegentropy. Nothing runs
	// negentropy against this database, so that is inert — do not "fix" it.
	blobIndexMaxLimit = 5000
)

// blobIndexEntry is one row of the index: one uploader's claim on one blob.
type blobIndexEntry struct {
	Owner    string
	SHA256   string
	Size     int
	Type     string
	Uploaded nostr.Timestamp
}

// parseBlobIndexEvent reads one index event, reporting whether it was usable.
//
// The tags are read by name. khatru's own reader takes them positionally —
// evt.Tags[0][1] and on — and panics on anything it did not write itself, from
// inside the goroutine that feeds the public /list endpoint, where a panic takes
// the process rather than the request. This walks every event in the database,
// including whatever a restore or a hand edit left behind, so it reports instead.
func parseBlobIndexEvent(evt *nostr.Event) (blobIndexEntry, bool) {
	x := evt.Tags.Find("x")
	if x == nil {
		return blobIndexEntry{}, false
	}
	hash := strings.ToLower(strings.TrimSpace(x[1]))
	if !nostr.IsValid32ByteHex(hash) {
		return blobIndexEntry{}, false
	}

	entry := blobIndexEntry{
		Owner:    evt.PubKey,
		SHA256:   hash,
		Uploaded: evt.CreatedAt,
	}
	// a missing type or size is cosmetic, not an identity problem, so the entry
	// still counts — it just shows up in the browser as an unknown blob
	if t := evt.Tags.Find("type"); t != nil {
		entry.Type = strings.TrimSpace(t[1])
	}
	if s := evt.Tags.Find("size"); s != nil {
		entry.Size, _ = strconv.Atoi(strings.TrimSpace(s[1]))
	}
	return entry, true
}

// countBlobIndexEvents counts the index exactly. CountEvents walks the whole
// cursor range with no limit of its own, which is what makes it a usable check
// on a listing that silently truncates.
func countBlobIndexEvents(ctx context.Context) (int64, error) {
	return blossomDB.CountEvents(ctx, nostr.Filter{Kinds: []int{blobIndexKind}})
}

//
// reading the disk
//

type diskBlob struct {
	Size     int64
	Modified time.Time
}

// scanBlobDir lists the blobs on disk. The directory is read in batches rather
// than in one call: a relay with fifty thousand blobs has fifty thousand entries
// in there and this runs on an owner's HTTP request. It also returns a count of
// the names it did not recognise, which is the only evidence the owner gets that
// something else is living in their blob directory.
func scanBlobDir(ctx context.Context) (map[string]diskBlob, int, error) {
	dir, prefix := blobDirAndPrefix()

	handle, err := fs.Open(dir)
	if err != nil {
		// no directory yet is the normal state of a relay nobody has uploaded to
		if errors.Is(err, os.ErrNotExist) {
			return map[string]diskBlob{}, 0, nil
		}
		return nil, 0, err
	}
	defer handle.Close()

	blobs := make(map[string]diskBlob)
	skipped := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		infos, readErr := handle.Readdir(1024)
		for _, info := range infos {
			name, ok := strings.CutPrefix(info.Name(), prefix)
			// .tmp files from an upload in flight are 68 characters and fail
			// the length check, so a partial write is never mistaken for a blob
			if !ok || info.IsDir() || !nostr.IsValid32ByteHex(name) {
				skipped++
				continue
			}
			blobs[name] = diskBlob{Size: info.Size(), Modified: info.ModTime()}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, 0, readErr
		}
		if len(infos) == 0 {
			break
		}
	}

	return blobs, skipped, nil
}

//
// the inventory
//

// blobEntry is one blob as the media browser sees it. The index holds one entry
// per (uploader, hash) but the file is content addressed and shared, so several
// uploaders of the same bytes collapse into one row: deleting the file takes it
// away from all of them, and three rows would invite the owner to believe
// otherwise.
//
// The three states reconciliation cares about are readable off this struct
// rather than needing a concept of their own. Owners and OnDisk means an
// ordinary blob; owners and no file means a dead public URL; no owners means a
// file nothing in the index points at, which khatru still serves quite happily.
type blobEntry struct {
	SHA256   string   `json:"sha256"`
	Size     int      `json:"size"` // as the index recorded it
	Type     string   `json:"type"`
	Uploaded int64    `json:"uploaded"` // earliest upload, or the file's mtime when nothing indexed it
	Owners   []string `json:"owners"`   // sorted, never nil
	URL      string   `json:"url"`
	OnDisk   bool     `json:"on_disk"`
	DiskSize int64    `json:"disk_size"` // 0 when the file is gone
	Blocked  bool     `json:"blocked"`
}

type blobStats struct {
	Path                 string `json:"path"`
	Blobs                int    `json:"blobs"`                  // distinct hashes, indexed or on disk
	IndexEntries         int    `json:"index_entries"`          // one per (uploader, hash)
	IndexEntriesExpected int64  `json:"index_entries_expected"` // what CountEvents says
	Complete             bool   `json:"complete"`
	Uploaders            int    `json:"uploaders"`
	OnDisk               int    `json:"on_disk"`
	DiskBytes            int64  `json:"disk_bytes"`
	IndexedBytes         int64  `json:"indexed_bytes"`
	MissingFiles         int    `json:"missing_files"`
	Unindexed            int    `json:"unindexed"`
	UnindexedBytes       int64  `json:"unindexed_bytes"`
	BlockedHashes        int    `json:"blocked_hashes"`
	SkippedIndexEntries  int    `json:"skipped_index_entries"`
	SkippedFiles         int    `json:"skipped_files"`
}

// buildBlobInventory reconciles the blob index against the blob directory.
//
// The disk is read first, but the order does not really matter: an upload writes
// its index entry before it writes its file, so a blob landing between the two
// scans looks like an index entry with no file whichever way round we read them.
// That is what the grace window in the orphan report is for.
func buildBlobInventory(ctx context.Context) ([]blobEntry, blobStats, error) {
	stats := blobStats{Path: config.BlossomPath}

	disk, skippedFiles, err := scanBlobDir(ctx)
	if err != nil {
		return nil, stats, fmt.Errorf("could not read the blob directory: %w", err)
	}
	stats.SkippedFiles = skippedFiles

	expected, err := countBlobIndexEvents(ctx)
	if err != nil {
		return nil, stats, fmt.Errorf("could not count the blob index: %w", err)
	}
	stats.IndexEntriesExpected = expected

	entries := make(map[string]*blobEntry, len(disk))
	owners := make(map[string]map[string]struct{}, len(disk))
	uploaders := make(map[string]struct{})

	visited, _, err := eachEvent(ctx, blossomDB, nostr.Filter{Kinds: []int{blobIndexKind}}, maxAggregateScan, func(evt *nostr.Event) bool {
		parsed, ok := parseBlobIndexEvent(evt)
		if !ok {
			stats.SkippedIndexEntries++
			return true
		}
		stats.IndexEntries++
		uploaders[parsed.Owner] = struct{}{}

		entry, seen := entries[parsed.SHA256]
		if !seen {
			entry = &blobEntry{
				SHA256:   parsed.SHA256,
				Size:     parsed.Size,
				Type:     parsed.Type,
				Uploaded: int64(parsed.Uploaded),
			}
			entries[parsed.SHA256] = entry
			owners[parsed.SHA256] = make(map[string]struct{}, 1)
		} else {
			// the earliest upload is the one reported, and a type one uploader
			// recorded beats an empty one another left behind
			if int64(parsed.Uploaded) < entry.Uploaded {
				entry.Uploaded = int64(parsed.Uploaded)
				entry.Size = parsed.Size
			}
			if entry.Type == "" {
				entry.Type = parsed.Type
			}
		}
		owners[parsed.SHA256][parsed.Owner] = struct{}{}
		return true
	})
	if err != nil {
		return nil, stats, fmt.Errorf("could not read the blob index: %w", err)
	}

	// a file nothing in the index mentions still gets a row: it is served, it is
	// public, and it is the only place the owner will ever see it. Its mtime
	// stands in for an upload time so it sorts with everything else rather than
	// piling up at the epoch.
	for hash, file := range disk {
		if _, ok := entries[hash]; ok {
			continue
		}
		entries[hash] = &blobEntry{
			SHA256:   hash,
			Size:     int(file.Size),
			Uploaded: file.Modified.Unix(),
		}
		owners[hash] = map[string]struct{}{}
	}

	blocked := management.get().BlockedBlobs
	base := blobServiceURL()
	list := make([]blobEntry, 0, len(entries))

	for hash, entry := range entries {
		entry.Owners = slices.Sorted(maps.Keys(owners[hash]))
		if entry.Owners == nil {
			entry.Owners = []string{}
		}
		entry.URL = base + "/" + hash + blobExtension(entry.Type)
		_, entry.Blocked = blocked[hash]

		if file, ok := disk[hash]; ok {
			entry.OnDisk = true
			entry.DiskSize = file.Size
			stats.OnDisk++
			stats.DiskBytes += file.Size
		} else {
			stats.MissingFiles++
		}

		if len(entry.Owners) == 0 {
			stats.Unindexed++
			stats.UnindexedBytes += entry.DiskSize
		} else {
			stats.IndexedBytes += int64(entry.Size)
		}

		list = append(list, *entry)
	}

	// newest first, hash as the tie break: the offset cursor the listing method
	// hands out is only meaningful because this order is deterministic
	slices.SortFunc(list, func(a, b blobEntry) int {
		if a.Uploaded != b.Uploaded {
			return cmp.Compare(b.Uploaded, a.Uploaded)
		}
		return cmp.Compare(a.SHA256, b.SHA256)
	})

	stats.Blobs = len(list)
	stats.Uploaders = len(uploaders)
	stats.BlockedHashes = len(blocked)
	// the only defence against a future change silently truncating this again
	stats.Complete = int64(visited) >= expected

	if !stats.Complete {
		slog.Warn("⚠️ the blob index could not be read in full", "read", visited, "expected", expected)
	}

	return list, stats, nil
}

//
// the inventory cache
//

// blobInventoryTTL bounds how often the media browser makes the relay walk its
// whole blob index and its whole blob directory. It matches eventCountTTL for
// the same reason that one exists, and it never costs freshness: everything that
// changes a blob invalidates the cache outright, so a delete shows up on the
// next call and this only ever caps the cost of looking.
const blobInventoryTTL = time.Minute

var blobInventory = &blobInventoryCache{}

type blobInventoryCache struct {
	mu      sync.Mutex
	at      time.Time
	entries []blobEntry
	stats   blobStats
}

// get rebuilds at most once per TTL. The lock is held across both scans on
// purpose: a second caller arriving mid-scan waits and gets the fresh answer
// instead of starting a scan of its own. The slice is handed out by reference
// rather than copied — several megabytes per call would be a strange thing to
// spend on a read-only result — so nothing may write through it.
func (c *blobInventoryCache) get(ctx context.Context) ([]blobEntry, blobStats, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries != nil && time.Since(c.at) < blobInventoryTTL {
		return c.entries, c.stats, c.at, nil
	}

	entries, stats, err := buildBlobInventory(ctx)
	if err != nil {
		// deliberately not cached: a directory that was briefly unreadable must
		// not pin an empty inventory in front of the owner for a minute
		return nil, stats, time.Time{}, err
	}

	c.entries, c.stats, c.at = entries, stats, time.Now()
	return c.entries, c.stats, c.at, nil
}

// invalidate drops the snapshot. It takes the same lock get holds across its
// scan, so a rebuild that was already running when a delete landed finishes,
// stores its now-stale result, and is cleared by the invalidate queued behind
// it. That ordering is why no generation counter is needed.
// peek returns the cached inventory without rebuilding it.
//
// get walks every file in the blob directory and the whole index while holding
// this lock, which on a relay with fifty thousand blobs is seconds. The dashboard
// takes what is already there or nothing at all, and reports the absence, rather
// than making a page refresh the thing that triggers a full disk scan.
func (c *blobInventoryCache) peek() (blobStats, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return blobStats{}, false
	}
	return c.stats, true
}

func (c *blobInventoryCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.at = nil, time.Time{}
}

//
// the blocklist
//

type blobReason struct {
	SHA256 string `json:"sha256"`
	Reason string `json:"reason"`
}

func blockBlob(sha256, reason string) error {
	if err := management.update(func(st *managementState) error {
		st.BlockedBlobs[sha256] = reason
		return nil
	}); err != nil {
		return err
	}
	// Blocked is baked into every entry at scan time, so the snapshot is wrong
	// the moment this changes
	blobInventory.invalidate()
	slog.Info("🛡️ blocked a blob", "sha256", sha256, "reason", reason)
	return nil
}

// unblockBlob lifts a block. It cannot bring back a blob that was deleted at the
// same time — the bytes are gone — so an upload is what puts it back.
func unblockBlob(sha256 string) error {
	if err := management.update(func(st *managementState) error {
		delete(st.BlockedBlobs, sha256)
		return nil
	}); err != nil {
		return err
	}
	blobInventory.invalidate()
	return nil
}

func listBlockedBlobs() []blobReason {
	blocked := management.get().BlockedBlobs
	entries := make([]blobReason, 0, len(blocked))
	for _, hash := range slices.Sorted(maps.Keys(blocked)) {
		entries = append(entries, blobReason{SHA256: hash, Reason: blocked[hash]})
	}
	return entries
}

// havenBlobIndex is khatru's blob index with the owner's blocklist in front of
// it. It embeds rather than reimplements so the URLs and file extensions the
// public /list endpoint serves stay byte identical to what they are today.
//
// This is where a block is actually enforced. The upload reject hooks run before
// khatru has read the body, so they cannot know the hash of what is arriving —
// but both the upload and the mirror handler call Keep before they call
// StoreBlob, which makes the index the one chokepoint every recording path
// crosses before any bytes reach the disk.
type havenBlobIndex struct {
	blossom.EventStoreBlobIndexWrapper
}

func (ix havenBlobIndex) Keep(ctx context.Context, blob blossom.BlobDescriptor, pubkey string) error {
	if isBlockedBlob(blob.SHA256) {
		slog.Info("🚫 refused a blocked blob", "sha256", blob.SHA256, "pubkey", pubkey)
		return errors.New("this blob is blocked by the relay owner")
	}
	return ix.EventStoreBlobIndexWrapper.Keep(ctx, blob, pubkey)
}

func (ix havenBlobIndex) Get(ctx context.Context, sha256 string) (*blossom.BlobDescriptor, error) {
	if isBlockedBlob(sha256) {
		return nil, nil
	}
	return ix.EventStoreBlobIndexWrapper.Get(ctx, sha256)
}

func (ix havenBlobIndex) List(ctx context.Context, pubkey string) (chan blossom.BlobDescriptor, error) {
	upstream, err := ix.EventStoreBlobIndexWrapper.List(ctx, pubkey)
	if err != nil {
		return nil, err
	}

	// a relay goroutine rather than a second query. khatru's list handler always
	// drains what it is given, so this cannot be stranded; anything else calling
	// it has to drain too.
	filtered := make(chan blossom.BlobDescriptor)
	go func() {
		defer close(filtered)
		for blob := range upstream {
			if isBlockedBlob(blob.SHA256) {
				continue
			}
			filtered <- blob
		}
	}()
	return filtered, nil
}

//
// deleting
//

// maxBlobsPerCall caps a bulk delete. maxManagementBody caps the whole request
// body at 64 KiB and a hash costs about 68 bytes of JSON, so a longer list comes
// back as a bare 413 with nothing to read rather than as the thing it is. This
// keeps the failure inside the method, where it can explain itself.
const maxBlobsPerCall = 500

type blobDeleteResult struct {
	SHA256       string `json:"sha256"`
	IndexRemoved int    `json:"index_removed"`
	FileRemoved  bool   `json:"file_removed"`
	Blocked      bool   `json:"blocked"`
	Error        string `json:"error,omitempty"`
}

// deleteBlobs removes blobs by hash: every index entry any uploader holds for
// them, and the file. Deleting one that is already half gone is not an error —
// an index entry with no file, and a file with no index entry, are exactly what
// this has to be able to clean up.
//
// When block is set the hashes go on the blocklist first, before anything is
// deleted, so an upload racing the delete cannot put the blob back in between.
func deleteBlobs(ctx context.Context, hashes []string, block bool, reason string) ([]blobDeleteResult, error) {
	if len(hashes) == 0 {
		return []blobDeleteResult{}, nil
	}

	// the caller's order is preserved, and targets points into the results so
	// the index walk below can record straight onto the row it belongs to
	results := make([]blobDeleteResult, 0, len(hashes))
	for _, hash := range hashes {
		results = append(results, blobDeleteResult{SHA256: hash, Blocked: block})
	}
	targets := make(map[string]*blobDeleteResult, len(results))
	for i := range results {
		targets[results[i].SHA256] = &results[i]
	}

	if block {
		if err := management.update(func(st *managementState) error {
			for hash := range targets {
				st.BlockedBlobs[hash] = reason
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	// one walk of the index, not one query per hash. With Kinds set, both query
	// planners route an x tag to the kind index with the tag as a post filter,
	// so a per-hash query would cost a full index walk each.
	var doomed []*nostr.Event
	if _, _, err := eachEvent(ctx, blossomDB, nostr.Filter{Kinds: []int{blobIndexKind}}, maxAggregateScan, func(evt *nostr.Event) bool {
		parsed, ok := parseBlobIndexEvent(evt)
		if !ok {
			return true
		}
		if _, wanted := targets[parsed.SHA256]; wanted {
			doomed = append(doomed, evt)
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("could not read the blob index: %w", err)
	}

	for _, evt := range doomed {
		parsed, _ := parseBlobIndexEvent(evt)
		result := targets[parsed.SHA256]
		// the event came out of the store, so it carries every field the
		// backends need to work out which index keys to remove; a stand-in
		// holding only the id would leave the indexes inconsistent
		if err := blossomDB.DeleteEvent(ctx, evt); err != nil {
			slog.Error("🚫 error deleting a blob index entry", "sha256", parsed.SHA256, "error", err)
			result.Error = err.Error()
			continue
		}
		result.IndexRemoved++
	}

	for i := range results {
		removed, err := removeBlob(results[i].SHA256)
		if err != nil {
			slog.Error("🚫 error deleting a blob", "sha256", results[i].SHA256, "error", err)
			if results[i].Error == "" {
				results[i].Error = err.Error()
			}
			continue
		}
		results[i].FileRemoved = removed
		slog.Info("🗑️ deleted a blob", "sha256", results[i].SHA256,
			"index_entries", results[i].IndexRemoved, "file", removed, "blocked", block)
	}

	blobInventory.invalidate()
	return results, nil
}

//
// reconciliation
//

// blobOrphanGrace keeps a reconciliation from tidying away an upload that is
// still happening. khatru writes the index entry before it writes the file, so
// for as long as an upload is in flight it looks exactly like an index entry
// whose file has been lost — and a cleanup that believed the scan would delete
// the record of a blob that is about to land.
const blobOrphanGrace = time.Hour

// maxOrphanList caps how many hashes a report names. The counts are always
// exact; the lists are for reviewing, and nobody reviews ten thousand rows.
const maxOrphanList = 1000

type blobOrphanReport struct {
	MissingFiles       []blobEntry `json:"missing_files"`
	Unindexed          []blobEntry `json:"unindexed"`
	MissingFilesTotal  int         `json:"missing_files_total"`
	UnindexedTotal     int         `json:"unindexed_total"`
	MissingFilesRecent int         `json:"missing_files_recent"`
	UnindexedRecent    int         `json:"unindexed_recent"`
	UnindexedBytes     int64       `json:"unindexed_bytes"`
	TotalFiles         int         `json:"total_files"`
	Truncated          bool        `json:"truncated"`
	GraceSeconds       int64       `json:"grace_seconds"`
	CountedAt          int64       `json:"counted_at"`
	Complete           bool        `json:"complete"`
	Warning            string      `json:"warning,omitempty"`
}

// classifyOrphans splits an inventory into the two problems worth reporting,
// holding back anything inside the grace window. A row with no owners is always
// on disk — that is how it got into the inventory — so the two cases cannot
// overlap.
func classifyOrphans(entries []blobEntry, cutoff int64) (missing, unindexed []blobEntry, missingRecent, unindexedRecent int) {
	for _, entry := range entries {
		switch {
		case len(entry.Owners) == 0:
			if entry.Uploaded > cutoff {
				unindexedRecent++
				continue
			}
			unindexed = append(unindexed, entry)
		case !entry.OnDisk:
			if entry.Uploaded > cutoff {
				missingRecent++
				continue
			}
			missing = append(missing, entry)
		}
	}
	return missing, unindexed, missingRecent, unindexedRecent
}

// blobOrphans reports what reconciliation would clean. Only what cleanup would
// actually remove is listed — anything inside the grace window is held back and
// counted separately — so the report is a truthful dry run rather than a
// superset of it.
func blobOrphans(ctx context.Context) (blobOrphanReport, error) {
	entries, stats, countedAt, err := blobInventory.get(ctx)
	if err != nil {
		return blobOrphanReport{}, err
	}

	cutoff := time.Now().Add(-blobOrphanGrace).Unix()
	missing, unindexed, missingRecent, unindexedRecent := classifyOrphans(entries, cutoff)

	report := blobOrphanReport{
		MissingFilesTotal:  len(missing),
		UnindexedTotal:     len(unindexed),
		MissingFilesRecent: missingRecent,
		UnindexedRecent:    unindexedRecent,
		TotalFiles:         stats.OnDisk,
		GraceSeconds:       int64(blobOrphanGrace.Seconds()),
		CountedAt:          countedAt.Unix(),
		Complete:           stats.Complete,
	}
	for _, entry := range unindexed {
		report.UnindexedBytes += entry.DiskSize
	}

	if len(missing) > maxOrphanList {
		missing = missing[:maxOrphanList]
		report.Truncated = true
	}
	if len(unindexed) > maxOrphanList {
		unindexed = unindexed[:maxOrphanList]
		report.Truncated = true
	}
	report.MissingFiles = missing
	report.Unindexed = unindexed
	if report.MissingFiles == nil {
		report.MissingFiles = []blobEntry{}
	}
	if report.Unindexed == nil {
		report.Unindexed = []blobEntry{}
	}
	if !stats.Complete {
		report.Warning = incompleteIndexWarning(stats) + ", so this report is incomplete and deleting files is refused"
	}
	return report, nil
}

type blobCleanupResult struct {
	Mode          string   `json:"mode"`
	IndexRemoved  int      `json:"index_removed"`
	FilesRemoved  int      `json:"files_removed"`
	BytesFreed    int64    `json:"bytes_freed"`
	SkippedRecent int      `json:"skipped_recent"`
	Errors        []string `json:"errors"`
}

// deleteOrphanBlobs removes what reconciliation found. It rescans rather than
// reading the cache: this deletes things, and a minute old snapshot is not what
// you want to be deleting from.
func deleteOrphanBlobs(ctx context.Context, mode string) (blobCleanupResult, error) {
	switch mode {
	case "index", "files", "both":
	default:
		return blobCleanupResult{}, errors.New(`mode must be "index", "files" or "both"`)
	}

	blobInventory.invalidate()
	entries, stats, _, err := blobInventory.get(ctx)
	if err != nil {
		return blobCleanupResult{}, err
	}

	// deleting files because an index we could not read in full did not mention
	// them is the one mistake here that destroys data for good
	if !stats.Complete && mode != "index" {
		return blobCleanupResult{}, fmt.Errorf(
			"refusing to delete blob files: %s, so a file that looks unindexed may not be",
			incompleteIndexWarning(stats))
	}

	cutoff := time.Now().Add(-blobOrphanGrace).Unix()
	missing, unindexed, missingRecent, unindexedRecent := classifyOrphans(entries, cutoff)

	result := blobCleanupResult{Mode: mode, Errors: []string{}}

	if mode == "index" || mode == "both" {
		hashes := make([]string, 0, len(missing))
		for _, entry := range missing {
			hashes = append(hashes, entry.SHA256)
		}
		if len(hashes) > 0 {
			deleted, err := deleteBlobs(ctx, hashes, false, "")
			if err != nil {
				return result, err
			}
			for _, one := range deleted {
				result.IndexRemoved += one.IndexRemoved
				if one.Error != "" {
					result.Errors = append(result.Errors, one.SHA256+": "+one.Error)
				}
			}
		}
		result.SkippedRecent += missingRecent
	}

	if mode == "files" || mode == "both" {
		// these have no index entries by definition, so the file is all there is
		for _, entry := range unindexed {
			removed, err := removeBlob(entry.SHA256)
			if err != nil {
				slog.Error("🚫 error deleting an unindexed blob", "sha256", entry.SHA256, "error", err)
				result.Errors = append(result.Errors, entry.SHA256+": "+err.Error())
				continue
			}
			if removed {
				result.FilesRemoved++
				result.BytesFreed += entry.DiskSize
			}
		}
		result.SkippedRecent += unindexedRecent
	}

	blobInventory.invalidate()
	slog.Info("🗑️ cleaned up orphaned blobs", "mode", mode,
		"index_removed", result.IndexRemoved, "files_removed", result.FilesRemoved,
		"bytes_freed", result.BytesFreed, "skipped_recent", result.SkippedRecent)
	return result, nil
}

//
// what the management API answers with
//

// maxBlobsPerList caps one page of the inventory. At roughly 350 bytes an entry
// this is about 3.4 MB of JSON, which covers essentially every personal relay in
// one call — and one call is one signature prompt, which is the whole reason the
// media browser can filter and sort without asking the owner's signer again.
// Anything larger pages with the offset parameter.
const maxBlobsPerList = 10000

type blobListResult struct {
	Blobs      []blobEntry  `json:"blobs"`
	Stats      blobStats    `json:"stats"`
	Blocked    []blobReason `json:"blocked"`
	Total      int          `json:"total"`
	Returned   int          `json:"returned"`
	Offset     int          `json:"offset"`
	Truncated  bool         `json:"truncated"`
	NextOffset int          `json:"next_offset"`
	CountedAt  int64        `json:"counted_at"`
	Complete   bool         `json:"complete"`
	Warning    string       `json:"warning,omitempty"`
}

type blobStatsResult struct {
	blobStats
	CountedAt int64  `json:"counted_at"`
	Warning   string `json:"warning,omitempty"`
}

// incompleteIndexWarning says the same thing everywhere it needs saying, in the
// owner's terms and with both numbers, because "some blobs are missing from this
// list" is not something anybody should have to infer.
func incompleteIndexWarning(stats blobStats) string {
	return fmt.Sprintf("only %d of %d blob index entries could be read",
		stats.IndexEntries+stats.SkippedIndexEntries, stats.IndexEntriesExpected)
}

// listBlobs answers with everything the media browser needs in one call: the
// blobs, the storage summary and the blocked hashes all fall out of the same
// scan, and splitting them would spend three signature prompts on data already
// in hand.
//
// Two quite different things can shorten this list, so they are reported
// separately: truncated means the page ended at the cap and next_offset says
// where to resume, while complete false means the index itself could not be read
// to the end and some blobs are missing altogether.
func listBlobs(ctx context.Context, offset, limit int) (blobListResult, error) {
	entries, stats, countedAt, err := blobInventory.get(ctx)
	if err != nil {
		return blobListResult{}, err
	}

	if limit <= 0 || limit > maxBlobsPerList {
		limit = maxBlobsPerList
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	end := min(offset+limit, len(entries))
	page := entries[offset:end]

	result := blobListResult{
		Blobs:     page,
		Stats:     stats,
		Blocked:   listBlockedBlobs(),
		Total:     len(entries),
		Returned:  len(page),
		Offset:    offset,
		CountedAt: countedAt.Unix(),
		Complete:  stats.Complete,
	}
	if end < len(entries) {
		result.Truncated = true
		result.NextOffset = end
	}
	if result.Blobs == nil {
		result.Blobs = []blobEntry{}
	}
	if !stats.Complete {
		result.Warning = incompleteIndexWarning(stats) + ", so some blobs are missing from this list"
	}
	return result, nil
}

func blobStatistics(ctx context.Context) (blobStatsResult, error) {
	_, stats, countedAt, err := blobInventory.get(ctx)
	if err != nil {
		return blobStatsResult{}, err
	}
	result := blobStatsResult{blobStats: stats, CountedAt: countedAt.Unix()}
	if !stats.Complete {
		result.Warning = incompleteIndexWarning(stats)
	}
	return result, nil
}

// blobBulkResult is what a bulk delete answers with. The per-hash rows are what
// the media browser needs to report honestly — a plain true could not say
// whether a file was already gone — and the totals save it counting them.
type blobBulkResult struct {
	Deleted      []blobDeleteResult `json:"deleted"`
	IndexRemoved int                `json:"index_removed"`
	FilesRemoved int                `json:"files_removed"`
	Errors       int                `json:"errors"`
}

func summariseDeletes(results []blobDeleteResult) blobBulkResult {
	summary := blobBulkResult{Deleted: results}
	if summary.Deleted == nil {
		summary.Deleted = []blobDeleteResult{}
	}
	for _, one := range results {
		summary.IndexRemoved += one.IndexRemoved
		if one.FileRemoved {
			summary.FilesRemoved++
		}
		if one.Error != "" {
			summary.Errors++
		}
	}
	return summary
}
