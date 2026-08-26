package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/barrydeen/haven/pkg/wot"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
)

func MustBeWhitelistedToQuery(ctx context.Context, _ nostr.Filter) (bool, string) {
	authenticatedUser := khatru.GetAuthed(ctx)
	if !isWhitelisted(authenticatedUser) {
		slog.Debug("🚫 query rejected: user is not whitelisted", "user", authenticatedUser)
		return true, "restricted: you must be whitelisted to query this relay"
	}
	return false, ""
}

func MustBeInWotToQuery(ctx context.Context, _ nostr.Filter) (bool, string) {
	authenticatedUser := khatru.GetAuthed(ctx)
	if !wot.GetInstance().Has(ctx, authenticatedUser) {
		slog.Debug("🚫 query rejected: user is not in the web of trust", "user", authenticatedUser)
		return true, "restricted: you must be in the web of trust to query this relay"
	}
	return false, ""
}

func MustBeWhitelistedToPost(ctx context.Context, event *nostr.Event) (bool, string) {
	// Event from a whitelisted pubkey can always be posted, even if the user is not authenticated
	if isWhitelisted(event.PubKey) {
		return false, ""
	}
	authenticatedUser := khatru.GetAuthed(ctx)
	if authenticatedUser == "" {
		return true, "auth-required: you must be authenticated to post to this relay"
	}
	if !isWhitelisted(authenticatedUser) {
		slog.Debug("🚫 event rejected: user is not whitelisted", "event", event.ID, "pubkey", authenticatedUser)
		return true, "restricted: you must be whitelisted to post to this relay"
	}
	return false, ""
}

func MustBeInWotToPost(ctx context.Context, event *nostr.Event) (bool, string) {
	// Event from a pubkey in the WoT can always be posted, even if the user is not authenticated
	if wot.GetInstance().Has(ctx, event.PubKey) {
		return false, ""
	}
	authenticatedUser := khatru.GetAuthed(ctx)
	if authenticatedUser == "" {
		return true, "auth-required: you must be authenticated to post to this relay"
	}
	if !wot.GetInstance().Has(ctx, authenticatedUser) {
		slog.Debug("🚫 event rejected: user is not in web of trust", "event", event.ID, "pubkey", authenticatedUser)
		return true, "you must be in the web of trust to post to this relay"
	}
	return false, ""
}

func MustNotBeBannedToPost(ctx context.Context, event *nostr.Event) (bool, string) {
	if isBanned(event.PubKey) {
		slog.Debug("🚫 event rejected: event author is banned", "event", event.ID, "pubkey", event.PubKey)
		return true, "you are banned from this relay"
	}
	// gift wraps and the like are signed with throwaway keys, so check who is on
	// the other end of the connection too, when we know
	if authenticatedUser := khatru.GetAuthed(ctx); authenticatedUser != "" && isBanned(authenticatedUser) {
		slog.Debug("🚫 event rejected: authenticated user is banned", "event", event.ID, "pubkey", authenticatedUser)
		return true, "you are banned from this relay"
	}
	return false, ""
}

func MustNotBeBlacklistedToPost(ctx context.Context, event *nostr.Event) (bool, string) {
	// Events from a blacklisted pubkey ARE always rejected
	if _, ok := config.BlacklistedPubKeys[event.PubKey]; ok {
		slog.Debug("🚫 event rejected: event author is blacklisted", "event", event.ID, "pubkey", event.PubKey)
		return true, "you are blacklisted from this relay"
	}
	// The owner's delete requests carry the owner's signature, so there is nothing
	// left for an AUTH round trip to prove
	if isOwnerDeleteRequest(event) {
		return false, ""
	}
	// Still need auth due to GiftWrap and other events with random pubkeys
	authenticatedUser := khatru.GetAuthed(ctx)
	if authenticatedUser == "" {
		return true, "auth-required: you must be authenticated to post to this relay"
	}
	if _, ok := config.BlacklistedPubKeys[authenticatedUser]; ok {
		slog.Debug("🚫 event rejected: authenticated user is blacklisted", "event", event.ID, "pubkey", authenticatedUser)
		return true, "you are blacklisted from this relay"
	}
	return false, ""
}

// isOwnerDeleteRequest reports whether the event is a NIP-09 delete request from
// the relay owner. Signatures are verified before any policy runs, so the pubkey
// on the event is proof enough that the owner sent it.
func isOwnerDeleteRequest(event *nostr.Event) bool {
	return event.Kind == nostr.KindDeletion && event.PubKey == config.OwnerPubKey
}

// OwnerCanDeleteAnyEvent replaces khatru's default NIP-09 outcome, which only
// lets authors delete their own events, so that the owner can delete anything
// stored on their relay. Everybody else is still limited to their own events.
func OwnerCanDeleteAnyEvent(_ context.Context, target *nostr.Event, deletion *nostr.Event) (bool, string) {
	// khatru handles delete requests before any reject policy runs, so this is
	// where a banned pubkey is stopped from deleting anything
	if isBanned(deletion.PubKey) {
		slog.Debug("🚫 deletion rejected: user is banned", "event", target.ID, "pubkey", deletion.PubKey)
		return false, "you are banned from this relay"
	}

	if target.PubKey == deletion.PubKey {
		return true, ""
	}

	if deletion.PubKey == config.OwnerPubKey {
		slog.Info("🗑️ owner deleted an event", "event", target.ID, "kind", target.Kind, "author", target.PubKey)
		return true, ""
	}

	slog.Debug("🚫 deletion rejected: user is not the author of the event", "event", target.ID, "pubkey", deletion.PubKey)
	return false, "you are not the author of this event"
}

// MustNotBeDeleted rejects events that have already been deleted from db, so a
// re-publish can't resurrect what the owner or the author deleted.
func MustNotBeDeleted(db DBBackend) func(context.Context, *nostr.Event) (bool, string) {
	return func(ctx context.Context, event *nostr.Event) (bool, string) {
		if isDeleted(ctx, db, event) {
			slog.Debug("🚫 event rejected: event has been deleted", "event", event.ID, "pubkey", event.PubKey)
			return true, "this event has been deleted"
		}
		return false, ""
	}
}

// MustNotBeIPBlocked drops websocket connections from addresses the owner
// blocked over the management API. khatru already resolves X-Forwarded-For, so
// this keys on the same address its rate limiters do.
//
// It only covers websocket upgrades, which is all RejectConnection gets called
// for: a blocked address can still read the NIP-11 document, the landing page
// and blossom blobs.
func MustNotBeIPBlocked(r *http.Request) bool {
	ip := khatru.GetIPFromRequest(r)
	if !isBlockedIP(ip) {
		return false
	}
	slog.Debug("🚫 connection rejected: blocked IP", "ip", ip)
	return true
}

// MustBeAnAllowedKind enforces the kind rules set over the management API for
// one relay. An empty allow list means no allow list, or a relay would stop
// accepting everything the moment the state file appeared. The rules only ever
// narrow what a relay already accepts: an allowed kind still has to get past
// every other policy, so allowing kind 1 on the chat relay accepts nothing.
func MustBeAnAllowedKind(relay string) func(context.Context, *nostr.Event) (bool, string) {
	return func(_ context.Context, event *nostr.Event) (bool, string) {
		// the owner's delete requests always get through, or disallowing kind 5
		// would cost them the ability to delete anything
		if isOwnerDeleteRequest(event) {
			return false, ""
		}

		rs := management.get().relayOrEmpty(relay)
		if slices.Contains(rs.DisallowedKinds, event.Kind) {
			slog.Debug("🚫 event rejected: disallowed kind", "event", event.ID, "kind", event.Kind)
			return true, fmt.Sprintf("kind %d is not accepted by this relay", event.Kind)
		}
		if len(rs.AllowedKinds) > 0 && !slices.Contains(rs.AllowedKinds, event.Kind) {
			slog.Debug("🚫 event rejected: kind is not on the allow list", "event", event.ID, "kind", event.Kind)
			return true, fmt.Sprintf("kind %d is not accepted by this relay", event.Kind)
		}
		return false, ""
	}
}

// OverwriteRelayInfo applies the name, description and icon the owner set over
// the management API to a relay's NIP-11 document.
//
// khatru copies rl.Info before it runs these hooks, and copies it without a
// lock, so this is the only race free place to change them — writing to
// rl.Info from an HTTP handler would race every concurrent NIP-11 request. The
// copy is shallow, so only the scalar fields may be touched: SupportedNIPs and
// Limitation are still shared with every other request in flight.
func OverwriteRelayInfo(relay string) func(context.Context, *http.Request, nip11.RelayInformationDocument) nip11.RelayInformationDocument {
	return func(_ context.Context, _ *http.Request, info nip11.RelayInformationDocument) nip11.RelayInformationDocument {
		info.Name, info.Description, info.Icon = effectiveRelayInfo(relay, info.Name, info.Description, info.Icon)
		return info
	}
}

// MustNotBeABannedEvent rejects an event the owner banned over the management
// API. banevent deletes the stored copy; without this the next publish would
// simply put it back, which is the failure MustNotBeDeleted exists to prevent.
func MustNotBeABannedEvent(relay string) func(context.Context, *nostr.Event) (bool, string) {
	return func(_ context.Context, event *nostr.Event) (bool, string) {
		if isBannedEvent(relay, event.ID) {
			slog.Debug("🚫 event rejected: event is banned", "event", event.ID, "relay", relay)
			return true, "this event is banned from this relay"
		}
		return false, ""
	}
}

var allowedChatKinds = map[int]struct{}{
	// Regular kinds
	nostr.KindSimpleGroupChatMessage:   {},
	nostr.KindSimpleGroupThreadedReply: {},
	nostr.KindSimpleGroupThread:        {},
	nostr.KindSimpleGroupReply:         {},
	nostr.KindChannelMessage:           {},
	nostr.KindChannelHideMessage:       {},

	nostr.KindGiftWrap: {},

	nostr.KindSimpleGroupPutUser:      {},
	nostr.KindSimpleGroupRemoveUser:   {},
	nostr.KindSimpleGroupEditMetadata: {},
	nostr.KindSimpleGroupDeleteEvent:  {},
	nostr.KindSimpleGroupCreateGroup:  {},
	nostr.KindSimpleGroupDeleteGroup:  {},
	nostr.KindSimpleGroupCreateInvite: {},
	nostr.KindSimpleGroupJoinRequest:  {},
	nostr.KindSimpleGroupLeaveRequest: {},

	// Addressable kinds
	nostr.KindSimpleGroupMetadata: {},
	nostr.KindSimpleGroupAdmins:   {},
	nostr.KindSimpleGroupMembers:  {},
	nostr.KindSimpleGroupRoles:    {},
}

func EventMustBeChatRelated(_ context.Context, event *nostr.Event) (bool, string) {
	if _, ok := allowedChatKinds[event.Kind]; ok {
		return false, ""
	}

	// the owner's delete requests are stored so the deletion survives a re-publish
	if isOwnerDeleteRequest(event) {
		return false, ""
	}

	return true, "only chat related events are allowed"
}

func OnlyGiftWrappedDMs(_ context.Context, event *nostr.Event) (bool, string) {
	if event.Kind == nostr.KindEncryptedDirectMessage {
		return true, "only gift wrapped DMs are supported"
	}
	return false, ""
}

func MustTagWhitelistedPubKey(_ context.Context, event *nostr.Event) (bool, string) {
	// the owner's delete requests are stored so the deletion survives a re-publish
	if isOwnerDeleteRequest(event) {
		return false, ""
	}

	// User must tag at least one whitelisted pubkey in this relay
	tags := event.Tags.FindAll("p")
	for tag := range tags {
		if len(tag) < 2 {
			continue
		}
		if isWhitelisted(tag[1]) {
			return false, ""
		}
	}

	slog.Debug("🚫 event rejected: event does not tag any whitelisted pubkey", "eventID", event.ID)

	return true, "you can only post notes if you've tagged a whitelisted pubkey in this relay"
}
