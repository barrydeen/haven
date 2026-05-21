// Package nip05namecoin implements Namecoin (.bit) name resolution for
// NIP-05 identifiers, returning the underlying Nostr pubkey via public
// ElectrumX servers.
//
// HAVEN uses this package at startup to expand its whitelist with
// pubkeys resolved from operator-supplied .bit names, in addition to
// the existing npub-based whitelist file.
//
// Spec reference: https://github.com/nostr-protocol/nips/pull/2349
package nip05namecoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

var hexPubKeyRegex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// IsValidIdentifier reports whether an identifier should be routed to
// Namecoin resolution instead of DNS-based NIP-05. It matches:
//
//   - "<anything>.bit"
//   - "alice@<anything>.bit"
//   - "d/<name>"
//   - "id/<name>"
//
// The name mirrors nip05.IsValidIdentifier so callers can use the two
// as a chained check.
func IsValidIdentifier(identifier string) bool {
	if identifier == "" {
		return false
	}
	norm := strings.ToLower(strings.TrimSpace(identifier))
	norm = strings.TrimPrefix(norm, "nostr:")
	if strings.HasPrefix(norm, "d/") || strings.HasPrefix(norm, "id/") {
		return true
	}
	return strings.HasSuffix(norm, ".bit")
}

// ParsedIdentifier captures the Namecoin name we need to query and the
// local-part within its value. Exported so tests in other packages can
// inspect parser output if needed; most callers should use
// ParseIdentifier and pass the result back to QueryIdentifier directly.
type ParsedIdentifier struct {
	NamecoinName string // e.g. "d/example" or "id/alice"
	LocalPart    string // e.g. "alice", or "_" for the root
	IsDomain     bool   // true for d/ names, false for id/ names
}

// ParseIdentifier breaks a user-supplied identifier into the Namecoin
// name + local-part pair. Returns nil for anything that isn't a valid
// .bit / d/ / id/ identifier.
func ParseIdentifier(raw string) *ParsedIdentifier {
	input := strings.TrimSpace(raw)
	// Strip an optional NIP-21 "nostr:" URI prefix.
	if len(input) >= 6 && strings.EqualFold(input[:6], "nostr:") {
		input = input[6:]
	}
	lower := strings.ToLower(input)

	// Explicit namespace references.
	if strings.HasPrefix(lower, "d/") {
		return &ParsedIdentifier{NamecoinName: lower, LocalPart: "_", IsDomain: true}
	}
	if strings.HasPrefix(lower, "id/") {
		return &ParsedIdentifier{NamecoinName: lower, LocalPart: "_", IsDomain: false}
	}

	// NIP-05 shape: user@domain.bit
	if strings.Contains(input, "@") && strings.HasSuffix(lower, ".bit") {
		parts := strings.SplitN(input, "@", 2)
		if len(parts) != 2 {
			return nil
		}
		local := strings.ToLower(parts[0])
		if local == "" {
			local = "_"
		}
		domain := strings.TrimSuffix(strings.ToLower(parts[1]), ".bit")
		if domain == "" {
			return nil
		}
		return &ParsedIdentifier{
			NamecoinName: "d/" + domain,
			LocalPart:    local,
			IsDomain:     true,
		}
	}

	// Bare domain: example.bit
	if strings.HasSuffix(lower, ".bit") {
		domain := strings.TrimSuffix(lower, ".bit")
		if domain == "" {
			return nil
		}
		return &ParsedIdentifier{
			NamecoinName: "d/" + domain,
			LocalPart:    "_",
			IsDomain:     true,
		}
	}

	return nil
}

// QueryIdentifier resolves a Namecoin .bit (or d/ / id/) identifier
// into a nostr.ProfilePointer. The signature mirrors
// nip05.QueryIdentifier from nbd-wtf/go-nostr so callers can fall
// through from one to the other without reshaping their code.
//
// The context deadline is respected: ElectrumX calls honour the same
// timeout the caller passed in.
func QueryIdentifier(ctx context.Context, identifier string) (*nostr.ProfilePointer, error) {
	parsed := ParseIdentifier(identifier)
	if parsed == nil {
		return nil, fmt.Errorf("nip05namecoin: not a Namecoin identifier: %q", identifier)
	}

	client := NewElectrumClient()
	result, err := client.NameShowWithFallback(ctx, parsed.NamecoinName, DefaultElectrumXServers)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, ErrNameNotFound
	}

	pubkeyHex, relays, err := extractNostrFromValue(result.Value, parsed)
	if err != nil {
		return nil, err
	}

	if !nostr.IsValidPublicKey(pubkeyHex) {
		return nil, fmt.Errorf("nip05namecoin: invalid pubkey %q in name value", pubkeyHex)
	}
	return &nostr.ProfilePointer{
		PublicKey: pubkeyHex,
		Relays:    relays,
	}, nil
}

// extractNostrFromValue parses the Namecoin name value JSON and pulls
// the relevant nostr pubkey + relay list out of it. Supports both the
// simple `"nostr": "hex"` form and the extended
// `"nostr": { "names": {...}, "relays": {...} }` form used by Amethyst.
func extractNostrFromValue(valueJSON string, parsed *ParsedIdentifier) (string, []string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valueJSON), &root); err != nil {
		return "", nil, fmt.Errorf("nip05namecoin: name value is not valid JSON: %w", err)
	}
	nostrRaw, ok := root["nostr"]
	if !ok {
		return "", nil, errors.New(`nip05namecoin: name value has no "nostr" field`)
	}

	// Simple form: "nostr": "hex-pubkey"
	var asString string
	if err := json.Unmarshal(nostrRaw, &asString); err == nil {
		if parsed.IsDomain && parsed.LocalPart != "_" {
			return "", nil, fmt.Errorf("nip05namecoin: simple nostr field only supports root lookup, got local-part %q", parsed.LocalPart)
		}
		if !hexPubKeyRegex.MatchString(asString) {
			return "", nil, errors.New("nip05namecoin: nostr field is not a 32-byte hex pubkey")
		}
		return strings.ToLower(asString), nil, nil
	}

	// Extended form: object with "names" and optional "relays".
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(nostrRaw, &asObject); err != nil {
		return "", nil, fmt.Errorf("nip05namecoin: nostr field is neither string nor object: %w", err)
	}

	if parsed.IsDomain {
		return extractFromDomainNamesObject(asObject, parsed)
	}
	return extractFromIdentityObject(asObject, parsed)
}

func extractFromDomainNamesObject(obj map[string]json.RawMessage, parsed *ParsedIdentifier) (string, []string, error) {
	namesRaw, ok := obj["names"]
	if !ok {
		return "", nil, errors.New(`nip05namecoin: extended nostr object lacks "names"`)
	}
	var names map[string]string
	if err := json.Unmarshal(namesRaw, &names); err != nil {
		return "", nil, fmt.Errorf("nip05namecoin: parse names map: %w", err)
	}

	// Match priority: exact local-part → "_" root → first entry (only
	// when the caller asked for root).
	var pickedPubkey string
	if v, ok := names[parsed.LocalPart]; ok && hexPubKeyRegex.MatchString(v) {
		pickedPubkey = v
	} else if v, ok := names["_"]; ok && hexPubKeyRegex.MatchString(v) {
		pickedPubkey = v
	} else if parsed.LocalPart == "_" {
		// First entry (map iteration order is non-deterministic, weak
		// fallback — we accept the first valid pubkey).
		for _, v := range names {
			if hexPubKeyRegex.MatchString(v) {
				pickedPubkey = v
				break
			}
		}
	}
	if pickedPubkey == "" {
		return "", nil, fmt.Errorf("nip05namecoin: no valid pubkey for local-part %q", parsed.LocalPart)
	}

	relays := extractRelays(obj, pickedPubkey)
	return strings.ToLower(pickedPubkey), relays, nil
}

func extractFromIdentityObject(obj map[string]json.RawMessage, parsed *ParsedIdentifier) (string, []string, error) {
	// Try "pubkey" field.
	if raw, ok := obj["pubkey"]; ok {
		var pk string
		if err := json.Unmarshal(raw, &pk); err == nil && hexPubKeyRegex.MatchString(pk) {
			var relays []string
			if r, ok := obj["relays"]; ok {
				_ = json.Unmarshal(r, &relays)
			}
			return strings.ToLower(pk), relays, nil
		}
	}

	// Fall back to NIP-05-like "names" with "_" root.
	if raw, ok := obj["names"]; ok {
		var names map[string]string
		if err := json.Unmarshal(raw, &names); err == nil {
			if v, ok := names["_"]; ok && hexPubKeyRegex.MatchString(v) {
				relays := extractRelays(obj, v)
				return strings.ToLower(v), relays, nil
			}
		}
	}

	_ = parsed
	return "", nil, errors.New("nip05namecoin: id/ nostr object has no valid pubkey")
}

func extractRelays(obj map[string]json.RawMessage, pubkey string) []string {
	raw, ok := obj["relays"]
	if !ok {
		return nil
	}
	var relayMap map[string][]string
	if err := json.Unmarshal(raw, &relayMap); err != nil {
		return nil
	}
	if v, ok := relayMap[strings.ToLower(pubkey)]; ok {
		return v
	}
	if v, ok := relayMap[pubkey]; ok {
		return v
	}
	return nil
}
