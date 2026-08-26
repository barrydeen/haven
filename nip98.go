package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// nip98MaxClockSkew is how far the auth event's created_at may be from ours.
// NIP-98 suggests 60 seconds; we apply it in both directions, so a client with
// a fast clock fails loudly instead of being quietly accepted forever.
const nip98MaxClockSkew = 60

// verifyNIP98 validates the Authorization header of an HTTP request the way
// NIP-98 prescribes, with NIP-86's addition that the payload tag is mandatory
// rather than a SHOULD. It returns the pubkey that signed the auth event.
//
// khatru has its own copy of this, but it only bounds the past side of the
// clock skew, never checks the kind or the method tag, panics on an auth event
// with no u tag, and has no way to be pointed at one of haven's four service
// URLs, so we do it here.
func verifyNIP98(r *http.Request, body []byte, serviceURL string) (string, error) {
	scheme, encoded, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Nostr") {
		return "", errors.New("missing Nostr authorization header")
	}

	// the NIP-98 example header is unpadded, but plenty of clients pad
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			return "", errors.New("authorization header is not valid base64")
		}
	}

	var event nostr.Event
	if err := json.Unmarshal(decoded, &event); err != nil {
		return "", errors.New("authorization header is not a nostr event")
	}

	if event.Kind != nostr.KindHTTPAuth {
		return "", fmt.Errorf("auth event must be kind %d, got %d", nostr.KindHTTPAuth, event.Kind)
	}
	if !event.CheckID() {
		return "", errors.New("auth event id does not match its contents")
	}
	if ok, err := event.CheckSignature(); !ok || err != nil {
		return "", errors.New("auth event signature is invalid")
	}

	uTag := event.Tags.Find("u")
	if uTag == nil {
		return "", errors.New("auth event has no u tag")
	}
	if !sameServiceURL(uTag[1], serviceURL) {
		return "", fmt.Errorf("auth event u tag is %q, expected %q", uTag[1], serviceURL)
	}

	methodTag := event.Tags.Find("method")
	if methodTag == nil {
		return "", errors.New("auth event has no method tag")
	}
	if !strings.EqualFold(methodTag[1], r.Method) {
		return "", fmt.Errorf("auth event method tag is %q, expected %q", methodTag[1], r.Method)
	}

	sum := sha256.Sum256(body)
	payloadTag := event.Tags.Find("payload")
	if payloadTag == nil {
		return "", errors.New("auth event has no payload tag")
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(payloadTag[1])), []byte(hex.EncodeToString(sum[:]))) != 1 {
		return "", errors.New("auth event payload tag does not match the request body")
	}

	now := nostr.Now()
	if event.CreatedAt < now-nip98MaxClockSkew || event.CreatedAt > now+nip98MaxClockSkew {
		return "", fmt.Errorf("auth event created_at is %ds away from the relay's clock, more than the %ds allowed",
			abs(int64(event.CreatedAt)-int64(now)), nip98MaxClockSkew)
	}

	return event.PubKey, nil
}

// sameServiceURL reports whether a NIP-98 u tag addresses this relay. The
// ws/wss distinction is dropped on purpose: haven sits behind a TLS terminating
// proxy and cannot know whether the client spoke http or https, and getHTTPScheme
// assumes https for anything that is not an onion address, so a plain HTTP
// deployment would otherwise never match. Host, port and path are what bind the
// auth event to one endpoint, and those are still compared.
func sameServiceURL(a, b string) bool {
	strip := func(u string) string {
		// NormalizeURL lowercases the host, drops a trailing slash and maps
		// http(s) onto ws(s); it returns "" for anything it cannot parse
		n := nostr.NormalizeURL(u)
		n = strings.TrimPrefix(n, "wss://")
		return strings.TrimPrefix(n, "ws://")
	}
	stripped := strip(a)
	return stripped != "" && stripped == strip(b)
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
