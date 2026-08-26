package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip86"
)

// contentTypeNIP86 marks a relay management request. khatru would answer these
// itself, but its handler reports auth failures as HTTP 200 where NIP-86 asks
// for a 401, never checks the auth event's kind or method tag, panics on an
// auth event with no u tag, and has no way to tell haven's four relays apart.
const contentTypeNIP86 = "application/nostr+json+rpc"

// maxManagementBody caps the request body. This endpoint is reachable before
// anything has been authenticated, so it must not read whatever turns up.
const maxManagementBody = 64 << 10

// maxReasonRunes caps the free text stored against a ban or a block. Reasons
// are handed back to whoever lists them, so they don't get to be unbounded.
const maxReasonRunes = 500

// nip86SupportedMethods is what supportedmethods answers. It is a literal list
// rather than reflection over a struct, so it cannot drift from the switch in
// callNIP86 without somebody noticing.
//
// Not implemented: the roles API (NIP-86 gives roles no permission semantics
// and haven has no role concept), listeventsneedingmoderation (haven has no
// moderation queue), and grantadmin/revokeadmin (not in the spec at all — only
// the owner can use this API).
var nip86SupportedMethods = []string{
	"supportedmethods",
	"banpubkey",
	"unbanpubkey",
	"listbannedpubkeys",
	"allowpubkey",
	"unallowpubkey",
	"listallowedpubkeys",
	"banevent",
	"allowevent",
	"listbannedevents",
	"listallowedevents",
	"changerelayname",
	"changerelaydescription",
	"changerelayicon",
	"allowkind",
	"disallowkind",
	"listallowedkinds",
	"listdisallowedkinds",
	"blockip",
	"unblockip",
	"listblockedips",
	"listblobs",
	"blobstats",
	"deleteblob",
	"deleteblobs",
	"blockblob",
	"unblockblob",
	"listblockedblobs",
	"listorphanblobs",
	"deleteorphanblobs",

	// The event browser. Relay scoped, like the ban and kind methods: the
	// relayName these are dispatched with comes from the URL path, so a call
	// signed for /private can only ever read or delete from the private store.
	// Not NIP-86's listeventsneedingmoderation — haven has no moderation queue
	// and these are not a queue.
	"listevents",
	"getevent",
	"deleteevent",
	"deleteevents",

	// The dashboard. Relay wide and answered identically on all four paths, like
	// the blob methods: the page reports on every relay at once, so answering on
	// only one of them would make supportedmethods a lie on the other three.
	"dashboard",

	"stats",
}

// isNIP86Request reports whether a request is addressed to the relay management
// API. The media type is parsed rather than compared whole, so a client that
// appends a charset still gets through.
func isNIP86Request(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == contentTypeNIP86
}

// handleManagementRequest answers a NIP-86 call for one relay. exact is false
// when the request landed on a path that only reaches this relay because it is
// the catch-all — a blossom blob, say — and the management API does not live
// there: NIP-86 puts it on the relay's own URI and nowhere else.
func handleManagementRequest(w http.ResponseWriter, r *http.Request, relay *khatru.Relay, relayName string, exact bool) {
	w.Header().Set("Content-Type", contentTypeNIP86)
	// khatru wraps its own NIP-86 handler in permissive CORS, and we intercept
	// ahead of it, so without this every browser based management client breaks.
	// The OPTIONS preflight carries no Content-Type and so still falls through
	// to khatru's middleware, which answers it.
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !exact {
		writeNIP86Error(w, http.StatusNotFound, "the relay management API is not served at this path")
		return
	}
	if !config.ManagementAPIEnabled {
		writeNIP86Error(w, http.StatusForbidden, "the relay management API is disabled on this relay")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeNIP86Error(w, http.StatusMethodNotAllowed, "the relay management API only accepts POST")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManagementBody))
	if err != nil {
		writeNIP86Error(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}

	pubKey, err := verifyNIP98(r, body, relay.ServiceURL)
	if err != nil {
		slog.Warn("🚫 relay management call rejected", "relay", relayName, "error", err)
		writeNIP86Error(w, http.StatusUnauthorized, err.Error())
		return
	}
	if pubKey != config.OwnerPubKey {
		slog.Warn("🚫 relay management call from somebody who is not the owner", "relay", relayName, "pubkey", pubKey)
		writeNIP86Error(w, http.StatusUnauthorized, "restricted: only the relay owner can use the management API")
		return
	}

	var req nip86.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeNIP86Response(w, nip86.Response{Error: "invalid json body"})
		return
	}

	slog.Info("🛡️ relay management call", "method", req.Method, "relay", relayName)

	result, err := callNIP86(r.Context(), relayName, req)
	if err != nil {
		slog.Info("🚫 relay management call failed", "method", req.Method, "relay", relayName, "error", err)
		writeNIP86Response(w, nip86.Response{Error: err.Error()})
		return
	}
	writeNIP86Response(w, nip86.Response{Result: result})
}

// writeNIP86Error answers with an HTTP status. Only authorization failures and
// requests that never reach a method get one: NIP-86 asks for a 401 when the
// Authorization header is missing or invalid, and everything else is reported
// in band at 200.
func writeNIP86Error(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	writeNIP86Response(w, nip86.Response{Error: message})
}

func writeNIP86Response(w http.ResponseWriter, resp nip86.Response) {
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Debug("🚫 error writing a relay management response", "error", err)
	}
}

// callNIP86 runs one management method. A nil error means the result is what
// goes back; anything else becomes the response's error field.
func callNIP86(ctx context.Context, relayName string, req nip86.Request) (any, error) {
	switch req.Method {
	case "supportedmethods":
		return nip86SupportedMethods, nil

	case "banpubkey":
		pubKey, err := paramPubKey(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(banPubKey(pubKey, paramReason(req.Params, 1)))

	case "unbanpubkey":
		pubKey, err := paramPubKey(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(unbanPubKey(pubKey))

	case "listbannedpubkeys":
		return listBannedPubKeys(), nil

	case "allowpubkey":
		pubKey, err := paramPubKey(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(allowPubKey(pubKey, paramReason(req.Params, 1)))

	case "unallowpubkey":
		pubKey, err := paramPubKey(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(unallowPubKey(pubKey))

	case "listallowedpubkeys":
		return listAllowedPubKeys(), nil

	case "banevent":
		id, err := paramEventID(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(banEvent(ctx, relayName, id, paramReason(req.Params, 1)))

	case "allowevent":
		id, err := paramEventID(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(allowEvent(relayName, id, paramReason(req.Params, 1)))

	case "listbannedevents":
		return listBannedEvents(relayName), nil

	case "listallowedevents":
		return listAllowedEvents(relayName), nil

	case "changerelayname":
		name, err := paramString(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(changeRelayName(relayName, name))

	case "changerelaydescription":
		description, err := paramString(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(changeRelayDescription(relayName, description))

	case "changerelayicon":
		icon, err := paramString(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(changeRelayIcon(relayName, icon))

	case "allowkind":
		kind, err := paramKind(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(allowKind(relayName, kind))

	case "disallowkind":
		kind, err := paramKind(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(disallowKind(relayName, kind))

	case "listallowedkinds":
		return listAllowedKinds(relayName), nil

	case "listdisallowedkinds":
		return listDisallowedKinds(relayName), nil

	case "blockip":
		ip, err := paramIP(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(blockIP(ip, paramReason(req.Params, 1)))

	case "unblockip":
		ip, err := paramIP(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(unblockIP(ip))

	case "listblockedips":
		return listBlockedIPs(), nil

	// The blob methods below are relay-wide. Blossom is served by the outbox
	// relay alone, but there is one blob directory, one index and one blocklist
	// behind all four, and supportedmethods is a single list served identically
	// by every one of them — answering these on only one relay would make that
	// list a lie on the other three. Every URL they return is absolute, so a
	// call made on /private still comes back with links that name the right one.

	case "listblobs":
		offset, err := paramIndex(req.Params, 0, 0)
		if err != nil {
			return nil, err
		}
		limit, err := paramIndex(req.Params, 1, maxBlobsPerList)
		if err != nil {
			return nil, err
		}
		result, err := listBlobs(ctx, offset, limit)
		if err != nil {
			return nil, err
		}
		return result, nil

	case "blobstats":
		stats, err := blobStatistics(ctx)
		if err != nil {
			return nil, err
		}
		return stats, nil

	case "deleteblob":
		hash, err := paramSHA256(req.Params, 0)
		if err != nil {
			return nil, err
		}
		results, err := deleteBlobs(ctx, []string{hash}, paramFlag(req.Params, 1), paramReason(req.Params, 2))
		if err != nil {
			return nil, err
		}
		return results[0], nil

	case "deleteblobs":
		hashes, err := paramSHA256List(req.Params, 0)
		if err != nil {
			return nil, err
		}
		results, err := deleteBlobs(ctx, hashes, paramFlag(req.Params, 1), paramReason(req.Params, 2))
		if err != nil {
			return nil, err
		}
		return summariseDeletes(results), nil

	case "blockblob":
		hash, err := paramSHA256(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(blockBlob(hash, paramReason(req.Params, 1)))

	case "unblockblob":
		hash, err := paramSHA256(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return done(unblockBlob(hash))

	case "listblockedblobs":
		return listBlockedBlobs(), nil

	case "listorphanblobs":
		report, err := blobOrphans(ctx)
		if err != nil {
			return nil, err
		}
		return report, nil

	case "deleteorphanblobs":
		mode, err := paramString(req.Params, 0)
		if err != nil {
			return nil, err
		}
		result, err := deleteOrphanBlobs(ctx, strings.ToLower(strings.TrimSpace(mode)))
		if err != nil {
			return nil, err
		}
		return result, nil

	case "listevents":
		opts, err := paramEventListOptions(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return listEvents(ctx, relayName, opts)

	case "getevent":
		id, err := paramEventID(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return getEvent(ctx, relayName, id)

	case "deleteevent":
		id, err := paramEventID(req.Params, 0)
		if err != nil {
			return nil, err
		}
		results, err := deleteEvents(ctx, relayName, []string{id})
		if err != nil {
			return nil, err
		}
		// the single result rather than the bulk envelope, mirroring deleteblob
		return results.Deleted[0], nil

	case "deleteevents":
		ids, err := paramEventIDList(req.Params, 0)
		if err != nil {
			return nil, err
		}
		return deleteEvents(ctx, relayName, ids)

	case "dashboard":
		return buildDashboard(ctx), nil

	case "stats":
		return relayStats(ctx, relayName), nil

	default:
		return nil, fmt.Errorf("method %q is not supported by this relay", req.Method)
	}
}

// done turns a mutating method into the boolean true NIP-86 wants as its result.
func done(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return true, nil
}

//
// parameter decoding
//

func paramString(params []any, i int) (string, error) {
	if i >= len(params) {
		return "", fmt.Errorf("parameter %d is missing", i+1)
	}
	s, ok := params[i].(string)
	if !ok {
		return "", fmt.Errorf("parameter %d must be a string", i+1)
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("parameter %d is not valid UTF-8", i+1)
	}
	return s, nil
}

func paramPubKey(params []any, i int) (string, error) {
	s, err := paramString(params, i)
	if err != nil {
		return "", err
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if !nostr.IsValidPublicKey(s) {
		return "", errors.New("not a valid 32 byte hex public key")
	}
	return s, nil
}

func paramEventID(params []any, i int) (string, error) {
	s, err := paramString(params, i)
	if err != nil {
		return "", err
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if !nostr.IsValid32ByteHex(s) {
		return "", errors.New("not a valid 32 byte hex event id")
	}
	return s, nil
}

func paramKind(params []any, i int) (int, error) {
	if i >= len(params) {
		return 0, fmt.Errorf("parameter %d is missing", i+1)
	}
	// encoding/json hands every number back as a float64, so a kind arrives
	// here as one and has to be checked for a fractional part
	f, ok := params[i].(float64)
	if !ok || math.Trunc(f) != f {
		return 0, fmt.Errorf("parameter %d must be a whole number", i+1)
	}
	if f < 0 || f > 65535 {
		return 0, errors.New("kind must be between 0 and 65535")
	}
	return int(f), nil
}

func paramIP(params []any, i int) (net.IP, error) {
	s, err := paramString(params, i)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return nil, errors.New("not a valid IP address")
	}
	return ip, nil
}

// paramReason reads the optional reason NIP-86 allows on most methods. A
// missing or unusable one is simply empty: the call is about the ban, not about
// the note attached to it.
// paramSHA256 reads a blob hash. It is the same 32 bytes of hex an event id is,
// but the error has to say so in the caller's own terms.
func paramSHA256(params []any, i int) (string, error) {
	s, err := paramString(params, i)
	if err != nil {
		return "", err
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if !nostr.IsValid32ByteHex(s) {
		return "", errors.New("not a valid 32 byte hex blob hash")
	}
	return s, nil
}

// paramSHA256List reads the array of hashes the bulk methods take, deduplicated
// and capped. The cap is not arbitrary: the request body is limited to 64 KiB,
// and a list long enough to exceed it would come back as a bare 413 with nothing
// in it to read.
func paramSHA256List(params []any, i int) ([]string, error) {
	if i >= len(params) {
		return nil, fmt.Errorf("parameter %d is missing", i+1)
	}
	raw, ok := params[i].([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %d must be an array of blob hashes", i+1)
	}
	if len(raw) == 0 {
		return nil, errors.New("no blob hashes were given")
	}
	if len(raw) > maxBlobsPerCall {
		return nil, fmt.Errorf("at most %d blobs can be deleted in one call, and %d were given", maxBlobsPerCall, len(raw))
	}

	seen := make(map[string]struct{}, len(raw))
	hashes := make([]string, 0, len(raw))
	for n, value := range raw {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("blob hash %d must be a string", n+1)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if !nostr.IsValid32ByteHex(s) {
			return nil, fmt.Errorf("blob hash %d is not 32 bytes of hex", n+1)
		}
		if _, dupe := seen[s]; dupe {
			continue
		}
		seen[s] = struct{}{}
		hashes = append(hashes, s)
	}
	return hashes, nil
}

// paramFlag reads an optional boolean, the way paramReason reads an optional
// string. Missing, or of the wrong type, means false: the call is about the
// delete, not about the switch attached to it.
func paramFlag(params []any, i int) bool {
	if i >= len(params) {
		return false
	}
	value, _ := params[i].(bool)
	return value
}

// paramIndex reads an optional non-negative whole number — an offset, or a page
// size — falling back to def when it is missing. encoding/json hands every
// number back as a float64, so a fractional one has to be caught here the way
// paramKind catches it.
func paramIndex(params []any, i, def int) (int, error) {
	if i >= len(params) {
		return def, nil
	}
	f, ok := params[i].(float64)
	if !ok || math.Trunc(f) != f {
		return 0, fmt.Errorf("parameter %d must be a whole number", i+1)
	}
	if f < 0 {
		return 0, fmt.Errorf("parameter %d cannot be negative", i+1)
	}
	if f > math.MaxInt32 {
		return 0, fmt.Errorf("parameter %d is out of range", i+1)
	}
	return int(f), nil
}

func paramReason(params []any, i int) string {
	s, err := paramString(params, i)
	if err != nil {
		return ""
	}
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxReasonRunes {
		s = string(runes[:maxReasonRunes])
	}
	return s
}

//
// parameters for the event browser
//

// paramEventIDList reads the array of ids a bulk delete takes, deduplicated and
// capped. It is paramSHA256List with a different noun and different errors, kept
// separate because "blob hash 3 is not 32 bytes of hex" is the wrong sentence to
// show somebody deleting notes.
func paramEventIDList(params []any, i int) ([]string, error) {
	if i >= len(params) {
		return nil, fmt.Errorf("parameter %d is missing", i+1)
	}
	raw, ok := params[i].([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %d must be an array of event ids", i+1)
	}
	if len(raw) == 0 {
		return nil, errors.New("no event ids were given")
	}
	if len(raw) > maxEventsPerDelete {
		return nil, fmt.Errorf("at most %d events can be deleted in one call, and %d were given", maxEventsPerDelete, len(raw))
	}

	seen := make(map[string]struct{}, len(raw))
	ids := make([]string, 0, len(raw))
	for n, value := range raw {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("event id %d must be a string", n+1)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if !nostr.IsValid32ByteHex(s) {
			return nil, fmt.Errorf("event id %d is not 32 bytes of hex", n+1)
		}
		if _, dupe := seen[s]; dupe {
			continue
		}
		seen[s] = struct{}{}
		ids = append(ids, s)
	}
	return ids, nil
}

// paramEventListOptions reads listevents' single object parameter.
//
// NIP-86's params array is positional and this method has eight options;
// listblobs already stretched the positional form to its limit at two. The NIP
// says params is an array and says nothing about what its elements may be, and
// deleteblobs already passes an array as params[0], so an object is no further a
// stretch. encoding/json decodes it into map[string]any, which is what the
// readers below take apart.
//
// Unlike paramFlag and paramReason, a value of the wrong type is an error and an
// unrecognised key is an error. Those two are forgiving because a missing flag or
// reason has an obviously safe default; a mistyped "kind" where "kinds" was meant
// has no safe default at all — it would quietly show the owner every event on the
// relay with a delete button beside each one.
func paramEventListOptions(params []any, i int) (eventListOptions, error) {
	var opts eventListOptions
	if i >= len(params) {
		// every field is optional, so no object at all is a valid request for the
		// newest page with no filter
		return opts, nil
	}
	raw, ok := params[i].(map[string]any)
	if !ok {
		return opts, fmt.Errorf("parameter %d must be an object of filter options", i+1)
	}

	allowed := map[string]struct{}{
		"kinds": {}, "authors": {}, "ids": {}, "since": {}, "until": {},
		"search": {}, "limit": {}, "cursor": {},
	}
	for key := range raw {
		if _, ok := allowed[key]; !ok {
			return opts, fmt.Errorf("unknown filter option %q", key)
		}
	}

	var err error
	if opts.Kinds, err = optKinds(raw, "kinds"); err != nil {
		return opts, err
	}
	if opts.Authors, err = optHexList(raw, "authors", maxFilterAuthors, "author"); err != nil {
		return opts, err
	}
	if opts.IDs, err = optHexList(raw, "ids", maxFilterIDs, "event id"); err != nil {
		return opts, err
	}
	if opts.Since, err = optTimestamp(raw, "since"); err != nil {
		return opts, err
	}
	if opts.Until, err = optTimestamp(raw, "until"); err != nil {
		return opts, err
	}
	if opts.Search, err = optSearch(raw, "search"); err != nil {
		return opts, err
	}
	if opts.Limit, err = optInt(raw, "limit", 0, 0, maxEventsPerPage); err != nil {
		return opts, err
	}
	if opts.Cursor, err = optString(raw, "cursor"); err != nil {
		return opts, err
	}
	return opts, nil
}

// optNumber reads one JSON number. encoding/json hands every number back as a
// float64, so a fractional one has to be caught here the way paramKind catches
// it.
func optNumber(opts map[string]any, key string) (float64, bool, error) {
	value, ok := opts[key]
	if !ok || value == nil {
		return 0, false, nil
	}
	f, ok := value.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%q must be a number", key)
	}
	if math.Trunc(f) != f {
		return 0, false, fmt.Errorf("%q must be a whole number", key)
	}
	return f, true, nil
}

func optInt(opts map[string]any, key string, def, lo, hi int) (int, error) {
	f, ok, err := optNumber(opts, key)
	if err != nil || !ok {
		return def, err
	}
	if f < float64(lo) || f > float64(hi) {
		return 0, fmt.Errorf("%q must be between %d and %d", key, lo, hi)
	}
	return int(f), nil
}

func optTimestamp(opts map[string]any, key string) (*nostr.Timestamp, error) {
	f, ok, err := optNumber(opts, key)
	if err != nil || !ok {
		return nil, err
	}
	// the range the backends can represent: both query planners narrow a bound to
	// uint32, so anything outside this wraps and the query silently comes back
	// empty. Refused here rather than clamped, because a caller that asked for a
	// year 2200 cursor has a bug and should hear about it.
	if f < 0 || f > float64(math.MaxUint32) {
		return nil, fmt.Errorf("%q is outside the range this relay can store", key)
	}
	ts := nostr.Timestamp(int64(f))
	return &ts, nil
}

func optString(opts map[string]any, key string) (string, error) {
	value, ok := opts[key]
	if !ok || value == nil {
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string", key)
	}
	return strings.TrimSpace(s), nil
}

func optSearch(opts map[string]any, key string) (string, error) {
	s, err := optString(opts, key)
	if err != nil {
		return "", err
	}
	if runes := []rune(s); len(runes) > maxSearchRunes {
		return "", fmt.Errorf("%q is longer than %d characters", key, maxSearchRunes)
	}
	return s, nil
}

func optKinds(opts map[string]any, key string) ([]int, error) {
	value, ok := opts[key]
	if !ok || value == nil {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be an array of kind numbers", key)
	}
	if len(raw) > maxFilterKinds {
		return nil, fmt.Errorf("%q holds more than %d kinds", key, maxFilterKinds)
	}

	seen := make(map[int]struct{}, len(raw))
	kinds := make([]int, 0, len(raw))
	for n, item := range raw {
		f, ok := item.(float64)
		if !ok || math.Trunc(f) != f {
			return nil, fmt.Errorf("kind %d must be a whole number", n+1)
		}
		if f < 0 || f > 65535 {
			return nil, fmt.Errorf("kind %d is out of range", n+1)
		}
		k := int(f)
		if _, dupe := seen[k]; dupe {
			continue
		}
		seen[k] = struct{}{}
		kinds = append(kinds, k)
	}
	// nil rather than an empty slice: the query planners branch on the field
	// being non-nil, so an empty but non-nil list matches nothing at all
	if len(kinds) == 0 {
		return nil, nil
	}
	return kinds, nil
}

func optHexList(opts map[string]any, key string, max int, what string) ([]string, error) {
	value, ok := opts[key]
	if !ok || value == nil {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be an array of %s values", key, what)
	}
	if len(raw) > max {
		return nil, fmt.Errorf("%q holds more than %d entries", key, max)
	}

	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for n, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s %d must be a string", what, n+1)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if !nostr.IsValid32ByteHex(s) {
			return nil, fmt.Errorf("%s %d is not 32 bytes of hex", what, n+1)
		}
		if _, dupe := seen[s]; dupe {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
