package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/spf13/afero"
)

// The four relays haven serves. These are keyed the same way as the dbs map in
// init.go, so a NIP-86 request addressed to one of them reaches its database
// without a second lookup table.
const (
	relayPrivate = "private"
	relayChat    = "chat"
	relayInbox   = "inbox"
	relayOutbox  = "outbox"
)

// managementStateVersion goes into the state file so a future haven can tell
// what it is reading.
const managementStateVersion = 1

// managementState is an immutable snapshot of everything the NIP-86 relay
// management API can change. Every mutation clones it, so the readers on the
// hot paths — one per event, one per connection — only ever do an atomic load,
// the same trick banlist.go uses for the kind 10084 cache.
//
// The maps are keyed by the thing being moderated and hold the reason, rather
// than being arrays of {pubkey, reason} objects: that is one representation
// instead of a list plus a derived lookup set that has to be kept in sync,
// encoding/json sorts map keys so the file stays diffable, and duplicates
// become impossible.
type managementState struct {
	Version        int                    `json:"version"`
	BannedPubKeys  map[string]string      `json:"banned_pubkeys"`
	AllowedPubKeys map[string]string      `json:"allowed_pubkeys"`
	BlockedIPs     map[string]string      `json:"blocked_ips"`
	BlockedBlobs   map[string]string      `json:"blocked_blobs"`
	Relays         map[string]*relayState `json:"relays"`
}

// relayState is the per-relay slice of the API. A NIP-86 request is addressed
// to a URL and haven serves four relays on four paths, so a kind rule or an
// event ban belongs to the relay it was sent to, not to haven as a whole.
type relayState struct {
	Name            string            `json:"name,omitempty"`
	Description     string            `json:"description,omitempty"`
	Icon            string            `json:"icon,omitempty"`
	AllowedKinds    []int             `json:"allowed_kinds,omitempty"`
	DisallowedKinds []int             `json:"disallowed_kinds,omitempty"`
	BannedEvents    map[string]string `json:"banned_events,omitempty"`
	AllowedEvents   map[string]string `json:"allowed_events,omitempty"`
}

func newManagementState() *managementState {
	return &managementState{
		Version:        managementStateVersion,
		BannedPubKeys:  map[string]string{},
		AllowedPubKeys: map[string]string{},
		BlockedIPs:     map[string]string{},
		BlockedBlobs:   map[string]string{},
		Relays:         map[string]*relayState{},
	}
}

// normalise fills in whatever a hand-written or older state file left out, so
// no reader has to nil check. It deliberately does not validate entries: the
// only thing we could do with a bad one is drop it, and a dropped entry is
// erased for good the next time the file is written.
func (st *managementState) normalise() {
	st.Version = managementStateVersion
	if st.BannedPubKeys == nil {
		st.BannedPubKeys = map[string]string{}
	}
	if st.AllowedPubKeys == nil {
		st.AllowedPubKeys = map[string]string{}
	}
	if st.BlockedIPs == nil {
		st.BlockedIPs = map[string]string{}
	}
	if st.BlockedBlobs == nil {
		st.BlockedBlobs = map[string]string{}
	}
	if st.Relays == nil {
		st.Relays = map[string]*relayState{}
	}
	for _, rs := range st.Relays {
		if rs.BannedEvents == nil {
			rs.BannedEvents = map[string]string{}
		}
		if rs.AllowedEvents == nil {
			rs.AllowedEvents = map[string]string{}
		}
	}
}

// clone deep copies the state. Every nested map and slice has to be copied too:
// sharing a *relayState would let a mutation leak into the snapshot readers are
// already holding.
func (st *managementState) clone() *managementState {
	next := &managementState{
		Version:        managementStateVersion,
		BannedPubKeys:  maps.Clone(st.BannedPubKeys),
		AllowedPubKeys: maps.Clone(st.AllowedPubKeys),
		BlockedIPs:     maps.Clone(st.BlockedIPs),
		BlockedBlobs:   maps.Clone(st.BlockedBlobs),
		Relays:         make(map[string]*relayState, len(st.Relays)),
	}
	for name, rs := range st.Relays {
		next.Relays[name] = &relayState{
			Name:            rs.Name,
			Description:     rs.Description,
			Icon:            rs.Icon,
			AllowedKinds:    slices.Clone(rs.AllowedKinds),
			DisallowedKinds: slices.Clone(rs.DisallowedKinds),
			BannedEvents:    maps.Clone(rs.BannedEvents),
			AllowedEvents:   maps.Clone(rs.AllowedEvents),
		}
	}
	return next
}

// relay returns the state for one relay, creating it if this is the first time
// anything has been set on it. Only call it on a clone, from inside update.
func (st *managementState) relay(name string) *relayState {
	rs, ok := st.Relays[name]
	if !ok {
		rs = &relayState{
			BannedEvents:  map[string]string{},
			AllowedEvents: map[string]string{},
		}
		st.Relays[name] = rs
	}
	return rs
}

// relayOrEmpty returns the state for one relay, or an empty one when nothing
// has been set. For readers, which must not allocate into the shared snapshot.
func (st *managementState) relayOrEmpty(name string) *relayState {
	if rs, ok := st.Relays[name]; ok {
		return rs
	}
	return &relayState{}
}

// managementStore holds the state the NIP-86 API works on. Readers load the
// pointer and never take the lock; the lock only serialises writers, which are
// owner initiated and rare.
type managementStore struct {
	path   string
	mu     sync.Mutex
	state  atomic.Pointer[managementState]
	frozen bool // guarded by mu; set when the file on disk could not be read
}

var management = &managementStore{}

// get returns the current snapshot. It is never nil once loadManagementStore
// has run, which main does before anything else touches it.
func (s *managementStore) get() *managementState {
	if st := s.state.Load(); st != nil {
		return st
	}
	return newManagementState()
}

// errManagementFrozen is what every mutating method returns when the state file
// could not be read at startup. Writing would overwrite whatever is in there,
// which is exactly what somebody with a broken file does not want.
func (s *managementStore) errManagementFrozen() error {
	return fmt.Errorf("the management state file %s could not be read at startup; fix or remove it and restart haven", s.path)
}

// update applies mutate to a copy of the current state and saves it before it
// becomes visible, so a "true" answer from the API means the change survived a
// restart. If saving fails nothing changes at all.
func (s *managementStore) update(mutate func(*managementState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.frozen {
		return s.errManagementFrozen()
	}

	next := s.get().clone()
	if err := mutate(next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		slog.Error("🚫 error saving the management state", "path", s.path, "error", err)
		return fmt.Errorf("could not save the management state: %w", err)
	}
	s.state.Store(next)
	return nil
}

// persist writes through a temp file and a rename, so a crash halfway through
// leaves the previous state intact instead of a truncated file. The temp file
// has to live next to the target: a rename is only atomic within a filesystem.
//
// afero has no way to fsync the containing directory, so a power loss in the
// instant after the rename could still lose the write. Nothing here is worth
// dropping the afero indirection for.
func (s *managementStore) persist(state *managementState) error {
	tmp := s.path + ".tmp"

	f, err := fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	cleanup := func(err error) error {
		_ = f.Close()
		_ = fs.Remove(tmp)
		return err
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		return cleanup(err)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err)
	}
	if err := f.Close(); err != nil {
		_ = fs.Remove(tmp)
		return err
	}
	return fs.Rename(tmp, s.path)
}

// loadManagementStore reads the state the NIP-86 API saved on a previous run.
// A missing file is the normal first boot. A file that cannot be read or parsed
// freezes the API instead of starting empty: whatever is in there was either
// hand edited or written by a newer haven, and the next write would destroy it.
// The relay keeps serving either way — moderation state going bad is not a
// reason to refuse to start.
func loadManagementStore() {
	management.path = config.ManagementStateFile
	management.state.Store(newManagementState())

	data, err := afero.ReadFile(fs, management.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		slog.Debug("ℹ️ no management state file yet", "path", management.path)
		return
	case err != nil:
		management.frozen = true
		slog.Error("🚫 could not read the management state file, the relay management API is read-only until it is fixed",
			"path", management.path, "error", err)
		return
	}

	var state managementState
	if err := json.Unmarshal(data, &state); err != nil {
		management.frozen = true
		slog.Error("🚫 could not parse the management state file, the relay management API is read-only until it is fixed",
			"path", management.path, "error", err)
		return
	}

	state.normalise()
	management.state.Store(&state)

	log.Println("🛡️ Management state loaded:",
		len(state.BannedPubKeys), "banned pubkeys,",
		len(state.AllowedPubKeys), "allowed pubkeys,",
		len(state.BlockedIPs), "blocked IPs,",
		len(state.BlockedBlobs), "blocked blobs")
}

//
// union readers
//
// The NIP-86 API is not the only source of bans or of whitelisting: the owner
// also publishes a kind 10084 list and maintains npub files. Neither of those
// can be written by the relay, so the API keeps its own store and everything
// that enforces a rule reads the union through one of these.
//

// isBanned reports whether a pubkey is banned, by either route: the kind 10084
// list the owner publishes, or the NIP-86 API. The list is read-only to the
// API — haven has no key to sign a replacement with.
func isBanned(pubKey string) bool {
	if bannedPubKeys.has(pubKey) {
		return true
	}
	_, ok := management.get().BannedPubKeys[pubKey]
	return ok
}

// isWhitelisted reports whether a pubkey has owner-level access, from either
// the npubs file or the NIP-86 allow list.
func isWhitelisted(pubKey string) bool {
	if _, ok := config.WhitelistedPubKeys[pubKey]; ok {
		return true
	}
	_, ok := management.get().AllowedPubKeys[pubKey]
	return ok
}

// isBlockedIP reports whether an address has been blocked over the API.
func isBlockedIP(ip string) bool {
	if ip == "" {
		return false
	}
	_, ok := management.get().BlockedIPs[ip]
	return ok
}

// isBlockedBlob reports whether a blob hash is on the owner's blocklist. Unlike
// a banned pubkey this has only one source — there is no published list of
// blocked hashes to merge in — but it belongs with the other readers because it
// sits on the same kind of hot path they do: every upload and every download of
// a blob asks it.
func isBlockedBlob(sha256 string) bool {
	if sha256 == "" {
		return false
	}
	_, ok := management.get().BlockedBlobs[sha256]
	return ok
}

// isBannedEvent reports whether an event was banned from one relay.
func isBannedEvent(relay, id string) bool {
	_, ok := management.get().relayOrEmpty(relay).BannedEvents[id]
	return ok
}

// effectiveRelayInfo applies the overrides set over the management API to the
// name, description and icon a relay was configured with. An override that was
// never set, or was cleared, leaves the configured value alone.
func effectiveRelayInfo(relay, name, description, icon string) (string, string, string) {
	rs := management.get().relayOrEmpty(relay)
	return cmp.Or(rs.Name, name), cmp.Or(rs.Description, description), cmp.Or(rs.Icon, icon)
}

// whitelistedPubKeys returns the union as a slice, for the nostr filters the
// import paths build.
func whitelistedPubKeys() []string {
	return slices.Collect(maps.Keys(whitelistedPubKeySet()))
}

// whitelistedPubKeySet returns the union as a map. It is always a fresh map:
// the web of trust refresher reads whatever it is given on every refresh, so
// handing it config.WhitelistedPubKeys and then writing to that map would be a
// data race.
func whitelistedPubKeySet() map[string]struct{} {
	allowed := management.get().AllowedPubKeys
	set := make(map[string]struct{}, len(config.WhitelistedPubKeys)+len(allowed))
	maps.Copy(set, config.WhitelistedPubKeys)
	for pubKey := range allowed {
		set[pubKey] = struct{}{}
	}
	return set
}
