package main

import (
	"context"
	"log/slog"

	"github.com/barrydeen/haven/pkg/wot"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

func MustBeWhitelistedToQuery(ctx context.Context, _ nostr.Filter) (bool, string) {
	authenticatedUser := khatru.GetAuthed(ctx)
	if _, ok := config.WhitelistedPubKeys[authenticatedUser]; !ok {
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
	if _, ok := config.WhitelistedPubKeys[event.PubKey]; ok {
		return false, ""
	}
	authenticatedUser := khatru.GetAuthed(ctx)
	if authenticatedUser == "" {
		return true, "auth-required: you must be authenticated to post to this relay"
	}
	if _, ok := config.WhitelistedPubKeys[authenticatedUser]; !ok {
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
		if _, ok := config.WhitelistedPubKeys[tag[1]]; ok {
			return false, ""
		}
	}

	slog.Debug("🚫 event rejected: event does not tag any whitelisted pubkey", "eventID", event.ID)

	return true, "you can only post notes if you've tagged a whitelisted pubkey in this relay"
}
