package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip86"
)

// Where a ban or an allow came from. haven has more than one source and only
// the ones the API wrote can be undone through the API, so the source travels
// with each entry. NIP-86 clients that don't know the field ignore it.
const (
	sourceAPI   = "api"   // the relay's own management state
	sourceList  = "list"  // the owner's kind 10084 ban list
	sourceFile  = "file"  // WHITELISTED_NPUBS_FILE
	sourceOwner = "owner" // OWNER_NPUB
)

// Caps on the strings the API stores and then serves back out in NIP-11.
const (
	maxRelayNameRunes        = 200
	maxRelayDescriptionRunes = 2000
	maxRelayIconRunes        = 2000
)

// pubKeyEntry is NIP-86's {pubkey, reason} object with the source added.
type pubKeyEntry struct {
	PubKey string `json:"pubkey"`
	Reason string `json:"reason"`
	Source string `json:"source"`
}

//
// pubkeys
//

func banPubKey(pubKey, reason string) error {
	// banning the owner would lock them out of their own relay; banlist.go
	// already skips the owner when it reads the kind 10084 list, and the API
	// controls its own writes, so it can refuse outright
	if pubKey == config.OwnerPubKey {
		return errors.New("the relay owner cannot be banned")
	}

	if isWhitelisted(pubKey) {
		slog.Warn("⚠️ banned a pubkey that is also whitelisted; the ban wins", "pubkey", pubKey)
	}

	return management.update(func(st *managementState) error {
		st.BannedPubKeys[pubKey] = reason
		return nil
	})
}

// unbanPubKey lifts a ban this API applied. A pubkey the owner banned with
// their kind 10084 list stays banned whatever we do here — the relay has no key
// to sign a replacement list with — so the store is cleaned up either way and
// the caller is told the ban is still standing and why.
func unbanPubKey(pubKey string) error {
	if err := management.update(func(st *managementState) error {
		delete(st.BannedPubKeys, pubKey)
		return nil
	}); err != nil {
		return err
	}

	if bannedPubKeys.has(pubKey) {
		return fmt.Errorf("this pubkey is still banned by your kind %d list; publish an updated list to unban it", KindBanList)
	}
	return nil
}

func listBannedPubKeys() []pubKeyEntry {
	st := management.get()
	entries := make([]pubKeyEntry, 0, len(st.BannedPubKeys)+bannedPubKeys.size())

	for _, pubKey := range bannedPubKeys.list() {
		entries = append(entries, pubKeyEntry{
			PubKey: pubKey,
			Reason: fmt.Sprintf("on the owner's kind %d ban list", KindBanList),
			Source: sourceList,
		})
	}
	for _, pubKey := range slices.Sorted(maps.Keys(st.BannedPubKeys)) {
		// a pubkey on both lists is reported once, as the entry that can be
		// removed from here
		if bannedPubKeys.has(pubKey) {
			continue
		}
		entries = append(entries, pubKeyEntry{PubKey: pubKey, Reason: st.BannedPubKeys[pubKey], Source: sourceAPI})
	}
	return entries
}

func allowPubKey(pubKey, reason string) error {
	if isBanned(pubKey) {
		slog.Warn("⚠️ allowed a pubkey that is also banned; the ban wins", "pubkey", pubKey)
	}

	return management.update(func(st *managementState) error {
		st.AllowedPubKeys[pubKey] = reason
		return nil
	})
}

// unallowPubKey takes away the whitelist privileges this API granted. A pubkey
// listed in WHITELISTED_NPUBS_FILE keeps them: haven must not rewrite a file
// the owner maintains by hand.
func unallowPubKey(pubKey string) error {
	if pubKey == config.OwnerPubKey {
		return errors.New("the relay owner is always allowed")
	}

	if err := management.update(func(st *managementState) error {
		delete(st.AllowedPubKeys, pubKey)
		return nil
	}); err != nil {
		return err
	}

	if _, ok := config.WhitelistedPubKeys[pubKey]; ok {
		return fmt.Errorf("this pubkey is still whitelisted in %s; remove it there and restart haven",
			getEnvString("WHITELISTED_NPUBS_FILE", "your whitelist file"))
	}
	return nil
}

func listAllowedPubKeys() []pubKeyEntry {
	st := management.get()
	entries := make([]pubKeyEntry, 0, len(st.AllowedPubKeys)+len(config.WhitelistedPubKeys))

	for _, pubKey := range slices.Sorted(maps.Keys(config.WhitelistedPubKeys)) {
		entry := pubKeyEntry{PubKey: pubKey, Reason: "whitelisted in your npubs file", Source: sourceFile}
		if pubKey == config.OwnerPubKey {
			entry.Reason, entry.Source = "the relay owner", sourceOwner
		}
		entries = append(entries, entry)
	}
	for _, pubKey := range slices.Sorted(maps.Keys(st.AllowedPubKeys)) {
		if _, ok := config.WhitelistedPubKeys[pubKey]; ok {
			continue
		}
		entries = append(entries, pubKeyEntry{PubKey: pubKey, Reason: st.AllowedPubKeys[pubKey], Source: sourceAPI})
	}
	return entries
}

//
// events
//

// banEvent stops an event being stored on one relay and drops the copy that is
// already there. It cannot publish a NIP-09 delete request in the owner's name:
// haven holds no private key, so the ban in the state file is the only record
// there is, and it is what MustNotBeBannedEvent reads.
func banEvent(ctx context.Context, relay, id, reason string) error {
	if err := management.update(func(st *managementState) error {
		rs := st.relay(relay)
		delete(rs.AllowedEvents, id)
		rs.BannedEvents[id] = reason
		return nil
	}); err != nil {
		return err
	}

	// the ban is what keeps the event out from here on; this only clears what
	// is already stored, so a failure is logged rather than returned
	_, _ = deleteStoredEvent(ctx, relay, id)
	return nil
}

// deleteStoredEvent drops one event from one relay's store and returns the event
// it removed. A nil event with a nil error means there was nothing stored under
// that id, which is not a failure: the caller's view of the relay may simply be
// a minute out of date.
//
// The event is read back in full before it is deleted because both backends work
// out which index keys to remove from the event's own fields — a stand-in
// carrying only the id would leave the indexes inconsistent.
func deleteStoredEvent(ctx context.Context, relay, id string) (*nostr.Event, error) {
	db, ok := dbs[relay]
	if !ok {
		return nil, fmt.Errorf("unknown relay %q", relay)
	}

	found, err := findStoredEvents(ctx, db, []string{id})
	if err != nil {
		slog.Error("🚫 error looking up an event to delete", "event", id, "relay", relay, "error", err)
		return nil, err
	}
	event, ok := found[id]
	if !ok {
		return nil, nil
	}

	if err := db.DeleteEvent(ctx, event); err != nil {
		slog.Error("🚫 error deleting an event", "event", id, "relay", relay, "error", err)
		return nil, err
	}
	slog.Info("🗑️ deleted a stored event", "event", id, "relay", relay, "kind", event.Kind, "author", event.PubKey)

	// the cached per database totals are now one out, and the owner is very
	// likely looking at a page that shows them
	eventCounts.invalidate()
	return event, nil
}

// allowEvent lifts a ban. It cannot bring back what banEvent deleted, and it
// deliberately does not override the NIP-09 deletion check either: an event the
// owner or its author deleted stays deleted.
func allowEvent(relay, id, reason string) error {
	return management.update(func(st *managementState) error {
		rs := st.relay(relay)
		delete(rs.BannedEvents, id)
		rs.AllowedEvents[id] = reason
		return nil
	})
}

func listBannedEvents(relay string) []nip86.IDReason {
	return idReasons(management.get().relayOrEmpty(relay).BannedEvents)
}

func listAllowedEvents(relay string) []nip86.IDReason {
	return idReasons(management.get().relayOrEmpty(relay).AllowedEvents)
}

func idReasons(m map[string]string) []nip86.IDReason {
	entries := make([]nip86.IDReason, 0, len(m))
	for _, id := range slices.Sorted(maps.Keys(m)) {
		entries = append(entries, nip86.IDReason{ID: id, Reason: m[id]})
	}
	return entries
}

//
// relay information
//

func changeRelayName(relay, name string) error {
	name, err := trimTo(name, maxRelayNameRunes, "name")
	if err != nil {
		return err
	}
	return management.update(func(st *managementState) error {
		st.relay(relay).Name = name
		return nil
	})
}

func changeRelayDescription(relay, description string) error {
	description, err := trimTo(description, maxRelayDescriptionRunes, "description")
	if err != nil {
		return err
	}
	return management.update(func(st *managementState) error {
		st.relay(relay).Description = description
		return nil
	})
}

// changeRelayIcon requires an absolute URL. khatru resolves a relative icon
// against the relay's base URL before our NIP-11 hook runs, so a relative
// override would slip past that and end up served as-is.
func changeRelayIcon(relay, icon string) error {
	icon, err := trimTo(icon, maxRelayIconRunes, "icon")
	if err != nil {
		return err
	}
	if icon != "" {
		parsed, err := url.Parse(icon)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("the icon must be an absolute http or https URL, or empty to clear it")
		}
	}
	return management.update(func(st *managementState) error {
		st.relay(relay).Icon = icon
		return nil
	})
}

// trimTo trims a value and rejects one that is too long, rather than silently
// truncating something the owner will see served back at them in NIP-11.
func trimTo(value string, max int, what string) (string, error) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > max {
		return "", fmt.Errorf("the relay %s must be at most %d characters", what, max)
	}
	return value, nil
}

//
// kinds
//

// allowKind puts a kind on the allow list and takes it off the disallow list,
// so the two stay disjoint and each list means something on its own.
func allowKind(relay string, kind int) error {
	return management.update(func(st *managementState) error {
		rs := st.relay(relay)
		rs.DisallowedKinds = withoutKind(rs.DisallowedKinds, kind)
		rs.AllowedKinds = withKind(rs.AllowedKinds, kind)
		return nil
	})
}

func disallowKind(relay string, kind int) error {
	return management.update(func(st *managementState) error {
		rs := st.relay(relay)
		rs.AllowedKinds = withoutKind(rs.AllowedKinds, kind)
		rs.DisallowedKinds = withKind(rs.DisallowedKinds, kind)
		return nil
	})
}

func listAllowedKinds(relay string) []int {
	return kindList(management.get().relayOrEmpty(relay).AllowedKinds)
}

func listDisallowedKinds(relay string) []int {
	return kindList(management.get().relayOrEmpty(relay).DisallowedKinds)
}

// kindList copies a kind list for the wire. Never nil: NIP-86 says these
// methods answer with an array of numbers, and a nil slice marshals to null,
// which a client iterating the result would choke on.
func kindList(kinds []int) []int {
	if kinds == nil {
		return []int{}
	}
	return slices.Clone(kinds)
}

func withKind(kinds []int, kind int) []int {
	if slices.Contains(kinds, kind) {
		return kinds
	}
	kinds = append(kinds, kind)
	slices.Sort(kinds)
	return kinds
}

func withoutKind(kinds []int, kind int) []int {
	return slices.DeleteFunc(kinds, func(k int) bool { return k == kind })
}

//
// IP addresses
//

// blockIP refuses the addresses that would take the relay down for everybody.
// Behind a proxy that does not set X-Forwarded-For, every client looks like
// 127.0.0.1, so blocking a loopback or private address is never what the owner
// meant.
func blockIP(ip net.IP, reason string) error {
	switch {
	case ip.IsLoopback():
		return errors.New("refusing to block a loopback address: behind a reverse proxy every client can look like one")
	case ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsUnspecified():
		return errors.New("refusing to block a private or unspecified address")
	case !ip.IsGlobalUnicast():
		return errors.New("only globally routable addresses can be blocked")
	}

	return management.update(func(st *managementState) error {
		st.BlockedIPs[ip.String()] = reason
		return nil
	})
}

func unblockIP(ip net.IP) error {
	return management.update(func(st *managementState) error {
		delete(st.BlockedIPs, ip.String())
		return nil
	})
}

func listBlockedIPs() []nip86.IPReason {
	blocked := management.get().BlockedIPs
	entries := make([]nip86.IPReason, 0, len(blocked))
	for _, ip := range slices.Sorted(maps.Keys(blocked)) {
		entries = append(entries, nip86.IPReason{IP: ip, Reason: blocked[ip]})
	}
	return entries
}

//
// stats
//

// startedAt is when this process came up, for the uptime stats reports.
var startedAt = time.Now()

// eventCountTTL bounds how often stats walks the databases. CountEvents with an
// empty filter is a full index scan on both lmdb and badger, and neither of
// them checks whether the context is done while they do it, so an owner polling
// stats in a loop would otherwise keep a core busy for as long as they liked.
const eventCountTTL = time.Minute

var eventCounts = &eventCountCache{}

type eventCountCache struct {
	mu     sync.Mutex
	at     time.Time
	counts map[string]int64
}

// get recomputes the counts at most once per TTL. The lock is held across the
// scan on purpose: a second caller arriving mid-scan waits and gets the fresh
// numbers instead of starting a scan of its own.
func (c *eventCountCache) get(ctx context.Context) (map[string]int64, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts != nil && time.Since(c.at) < eventCountTTL {
		return c.counts, c.at
	}

	counts := make(map[string]int64, len(dbs))
	for name, db := range dbs {
		// countStoredEvents rather than db.CountEvents: on lmdb — which is what
		// DB_ENGINE defaults to — CountEvents never returns once anything
		// matches, and it is holding this lock while it does not, so every later
		// caller would block behind it forever too. See backendCountsSafely.
		count, exact, err := countStoredEvents(ctx, db, nostr.Filter{}, maxAggregateScan)
		if err != nil {
			slog.Error("🚫 error counting events", "db", name, "error", err)
			continue
		}
		if !exact {
			slog.Warn("⚠️ event count stopped at the scan budget, so it is a floor rather than a total",
				"db", name, "counted", count, "budget", maxAggregateScan)
		}
		counts[name] = count
	}

	c.counts, c.at = counts, time.Now()
	return c.counts, c.at
}

// invalidate drops the cached counts so the next caller recomputes them. It is
// called after a delete: without it a deleted event stays counted for up to a
// minute, on a page the owner is looking at because they just deleted something.
//
// No generation counter is needed for the same reason blobInventoryCache needs
// none — a scan already in flight will store counts that are at most one delete
// stale, and the next call a second later corrects it.
func (c *eventCountCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts, c.at = nil, time.Time{}
}

// relayStats is not part of NIP-86 — khatru defines the slot and we define the
// shape. Counts are reported per source rather than added up: the ban list and
// the management state overlap, and a single number would hide which of them is
// doing the work.
func relayStats(ctx context.Context, relay string) map[string]any {
	st := management.get()
	rs := st.relayOrEmpty(relay)
	counts, countedAt := eventCounts.get(ctx)

	return map[string]any{
		"relay":             relay,
		"version":           config.RelayVersion,
		"uptime_seconds":    int64(time.Since(startedAt).Seconds()),
		"events":            counts,
		"events_counted_at": countedAt.Unix(),
		"banned_pubkeys": map[string]int{
			"list": bannedPubKeys.size(),
			"api":  len(st.BannedPubKeys),
		},
		"allowed_pubkeys": map[string]int{
			"file": len(config.WhitelistedPubKeys),
			"api":  len(st.AllowedPubKeys),
		},
		"blacklisted_pubkeys": len(config.BlacklistedPubKeys),
		"blocked_ips":         len(st.BlockedIPs),
		"banned_events":       len(rs.BannedEvents),
	}
}
