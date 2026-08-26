package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// signedManagementRequest builds the exact request the admin page sends: a NIP-86
// body, and a NIP-98 event over it in the Authorization header.
func signedManagementRequest(t *testing.T, sk, serviceURL, path, method string, params []any) *http.Request {
	t.Helper()

	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	sum := sha256.Sum256(body)

	auth := &nostr.Event{
		Kind:      nostr.KindHTTPAuth,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags: nostr.Tags{
			nostr.Tag{"u", serviceURL},
			nostr.Tag{"method", "POST"},
			nostr.Tag{"payload", hex.EncodeToString(sum[:])},
		},
	}
	if err := auth.Sign(sk); err != nil {
		t.Fatalf("sign auth: %v", err)
	}
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/nostr+json+rpc")
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(encoded))
	return req
}

// TestManagementListEventsOverHTTP drives the whole path the browser drives:
// media type detection, the NIP-98 check, the owner comparison, the dispatch
// switch and listevents itself. Everything below handleManagementRequest is
// covered elsewhere; this is the part that only breaks when they are wired
// together.
func TestManagementListEventsOverHTTP(t *testing.T) {
	db := withTestRelay(t, 50)
	seedNotes(t, db, 3, 1700000000)

	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}

	// the config is a package level value, so it is borrowed and put back
	originalOwner := config.OwnerPubKey
	originalEnabled := config.ManagementAPIEnabled
	config.OwnerPubKey = pk
	config.ManagementAPIEnabled = true
	t.Cleanup(func() {
		config.OwnerPubKey = originalOwner
		config.ManagementAPIEnabled = originalEnabled
	})

	relay := khatru.NewRelay()
	relay.ServiceURL = "https://relay.example.com"

	call := func(t *testing.T, method string, params []any) map[string]any {
		t.Helper()
		req := signedManagementRequest(t, sk, relay.ServiceURL, "/", method, params)
		rec := httptest.NewRecorder()
		handleManagementRequest(rec, req, relay, relayOutbox, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d — %s", method, rec.Code, rec.Body.String())
		}
		var envelope map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: bad JSON %v — %s", method, err, rec.Body.String())
		}
		if msg, ok := envelope["error"]; ok && msg != "" {
			t.Fatalf("%s: relay returned error %v", method, msg)
		}
		result, ok := envelope["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: result was not an object — %s", method, rec.Body.String())
		}
		return result
	}

	// supportedmethods must advertise the new ones, because the admin page uses
	// that list to decide the console is usable at all
	req := signedManagementRequest(t, sk, relay.ServiceURL, "/", "supportedmethods", nil)
	rec := httptest.NewRecorder()
	handleManagementRequest(rec, req, relay, relayOutbox, true)
	var supported struct {
		Result []string `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &supported); err != nil {
		t.Fatalf("supportedmethods: %v", err)
	}
	for _, want := range []string{"listevents", "getevent", "deleteevent", "deleteevents"} {
		found := false
		for _, have := range supported.Result {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("supportedmethods does not advertise %q", want)
		}
	}

	// listevents with the object parameter the page sends
	result := call(t, "listevents", []any{map[string]any{"limit": float64(2)}})
	events, _ := result["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("listevents returned %d events, want 2", len(events))
	}
	if result["total"].(float64) != 3 {
		t.Errorf("total = %v, want 3", result["total"])
	}
	if result["next_cursor"] == nil || result["next_cursor"] == "" {
		t.Error("no cursor was offered despite a third event being unread")
	}
	if _, ok := result["profiles"]; !ok {
		t.Error("no profiles map came back; the page needs it to avoid a second signature")
	}

	first := events[0].(map[string]any)
	id, _ := first["id"].(string)
	if len(id) != 64 {
		t.Fatalf("event id looks wrong: %q", id)
	}

	// an unknown filter option must be refused rather than silently ignored: a
	// mistyped "kind" for "kinds" would otherwise show every event on the relay
	badReq := signedManagementRequest(t, sk, relay.ServiceURL, "/", "listevents", []any{map[string]any{"kind": float64(1)}})
	badRec := httptest.NewRecorder()
	handleManagementRequest(badRec, badReq, relay, relayOutbox, true)
	var bad map[string]any
	_ = json.Unmarshal(badRec.Body.Bytes(), &bad)
	if bad["error"] == nil {
		t.Error("an unknown filter option was accepted")
	}

	// getevent, then delete it, then confirm it is gone
	detail := call(t, "getevent", []any{id})
	if detail["event"] == nil {
		t.Fatal("getevent returned no event")
	}

	deleted := call(t, "deleteevents", []any{[]any{id}})
	if deleted["removed"].(float64) != 1 {
		t.Errorf("removed = %v, want 1", deleted["removed"])
	}
	if deleted["permanent"].(float64) != 0 {
		t.Errorf("permanent = %v, want 0 — a plain delete is not a ban", deleted["permanent"])
	}
	if deleted["warning"] == nil || deleted["warning"] == "" {
		t.Error("no warning that a deleted event can be published again")
	}

	after := call(t, "listevents", []any{map[string]any{}})
	if after["total"].(float64) != 2 {
		t.Errorf("total after delete = %v, want 2", after["total"])
	}
}

// TestManagementRejectsAStranger: the whole console is owner-only, and that check
// is the only thing standing between a stranger and a delete button.
func TestManagementRejectsAStranger(t *testing.T) {
	withTestRelay(t, 50)

	owner := nostr.GeneratePrivateKey()
	ownerPk, _ := nostr.GetPublicKey(owner)
	stranger := nostr.GeneratePrivateKey()

	original := config.OwnerPubKey
	config.OwnerPubKey = ownerPk
	t.Cleanup(func() { config.OwnerPubKey = original })

	relay := khatru.NewRelay()
	relay.ServiceURL = "https://relay.example.com"

	req := signedManagementRequest(t, stranger, relay.ServiceURL, "/", "listevents", []any{map[string]any{}})
	rec := httptest.NewRecorder()
	handleManagementRequest(rec, req, relay, relayOutbox, true)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a stranger got HTTP %d, want 401 — %s", rec.Code, rec.Body.String())
	}

	// and the same call signed for a different URL must fail too, or a signature
	// captured from one relay would work against another
	wrongURL := signedManagementRequest(t, owner, "https://somewhere.else", "/", "listevents", []any{map[string]any{}})
	wrongRec := httptest.NewRecorder()
	handleManagementRequest(wrongRec, wrongURL, relay, relayOutbox, true)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Errorf("a signature for another service URL got HTTP %d, want 401", wrongRec.Code)
	}
}
