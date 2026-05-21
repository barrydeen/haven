package nip05namecoin

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestIsValidIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"example.bit", true},
		{"alice@example.bit", true},
		{"_@example.bit", true},
		{"Example.Bit", true},
		{" example.bit ", true},
		{"d/example", true},
		{"id/alice", true},
		{"D/example", true},
		{"alice@example.com", false},
		{"example.com", false},
		{"", false},
		{"   ", false},
		{"npub1xyz", false},
		{"nostr:alice@example.bit", true},
		{"nostr:example.bit", true},
		{"nostr:npub1xyz", false},
	}
	for _, tc := range cases {
		if got := IsValidIdentifier(tc.in); got != tc.want {
			t.Errorf("IsValidIdentifier(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want *ParsedIdentifier
	}{
		{"example.bit", &ParsedIdentifier{"d/example", "_", true}},
		{"alice@example.bit", &ParsedIdentifier{"d/example", "alice", true}},
		{"_@example.bit", &ParsedIdentifier{"d/example", "_", true}},
		{"ALICE@Example.Bit", &ParsedIdentifier{"d/example", "alice", true}},
		{"d/example", &ParsedIdentifier{"d/example", "_", true}},
		{"id/alice", &ParsedIdentifier{"id/alice", "_", false}},
		{"nostr:alice@example.bit", &ParsedIdentifier{"d/example", "alice", true}},
		{"Nostr:example.bit", &ParsedIdentifier{"d/example", "_", true}},
		{".bit", nil},
		{"@.bit", nil},
		{"not a name", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := ParseIdentifier(tc.in)
		if tc.want == nil {
			if got != nil {
				t.Errorf("ParseIdentifier(%q) = %+v, want nil", tc.in, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("ParseIdentifier(%q) = nil, want %+v", tc.in, tc.want)
			continue
		}
		if got.NamecoinName != tc.want.NamecoinName ||
			got.LocalPart != tc.want.LocalPart ||
			got.IsDomain != tc.want.IsDomain {
			t.Errorf("ParseIdentifier(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestBuildNameIndexScript(t *testing.T) {
	// Layout: OP_NAME_UPDATE | 0x08 | "d/testls" (8 bytes) | 0x00 (empty push) | OP_2DROP | OP_DROP | OP_RETURN
	// Expected hex: 53 08 642f746573746c73 00 6d 75 6a
	name := []byte("d/testls")
	got := buildNameIndexScript(name)
	want, _ := hex.DecodeString("5308642f746573746c73006d756a")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("name-index script mismatch:\n  got:  %x\n  want: %x", got, want)
	}
}

func TestPushData(t *testing.T) {
	if got := pushData([]byte{1, 2, 3}); hex.EncodeToString(got) != "03010203" {
		t.Errorf("pushData(3 bytes) = %x, want 03010203", got)
	}
	data := make([]byte, 0x4c)
	got := pushData(data)
	if got[0] != opPushData1 || got[1] != 0x4c {
		t.Errorf("pushData(0x4c bytes) should start with OP_PUSHDATA1 0x4c, got %x", got[:2])
	}
	big := make([]byte, 256)
	got = pushData(big)
	if got[0] != opPushData2 || got[1] != 0x00 || got[2] != 0x01 {
		t.Errorf("pushData(256 bytes) should start with OP_PUSHDATA2 0x00 0x01, got %x", got[:3])
	}
}

func TestElectrumScriptHash(t *testing.T) {
	// SHA-256("") reversed.
	got := electrumScriptHash([]byte{})
	want := "55b852781b9995a44c939b64e441ae2724b96f99c8f4fb9a141cfc9842c4b0e3"
	if got != want {
		t.Errorf("electrumScriptHash(empty) = %s, want %s", got, want)
	}
}

func TestElectrumScriptHashDTestLS(t *testing.T) {
	// "d/testls" scripthash matches the long-standing reference value
	// served by public Namecoin ElectrumX servers.
	script := buildNameIndexScript([]byte("d/testls"))
	got := electrumScriptHash(script)
	want := "b519574e96740a4b3627674a0708e71a73e654a95117fc828b8e177a0579ab42"
	if got != want {
		t.Errorf("electrumScriptHash(d/testls) = %s, want %s", got, want)
	}
}

func TestReadPushData(t *testing.T) {
	script, _ := hex.DecodeString("04deadbeef")
	data, next, err := readPushData(script, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data) != "deadbeef" || next != 5 {
		t.Errorf("direct push: got data=%x next=%d", data, next)
	}

	script, _ = hex.DecodeString("4c03aabbcc")
	data, next, err = readPushData(script, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data) != "aabbcc" || next != 5 {
		t.Errorf("OP_PUSHDATA1: got data=%x next=%d", data, next)
	}

	script, _ = hex.DecodeString("4d02001122")
	data, next, err = readPushData(script, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data) != "1122" || next != 5 {
		t.Errorf("OP_PUSHDATA2: got data=%x next=%d", data, next)
	}

	script = []byte{0x00}
	data, next, err = readPushData(script, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 || next != 1 {
		t.Errorf("OP_0: got data=%x next=%d", data, next)
	}
}

func TestParseNameScript(t *testing.T) {
	value := `{"nostr":"6cdebccabda1dfa058ab85352a79509b592b2bdfa0370325e28ec1cb4f18667d"}`
	script := []byte{opNameUpdate}
	script = append(script, pushData([]byte("d/testls"))...)
	script = append(script, pushData([]byte(value))...)
	script = append(script, op2Drop, opDrop)
	script = append(script, []byte{0x76, 0xa9, 0x14}...)

	name, gotValue, err := parseNameScript(script)
	if err != nil {
		t.Fatalf("parseNameScript: %v", err)
	}
	if name != "d/testls" {
		t.Errorf("name = %q, want d/testls", name)
	}
	if gotValue != value {
		t.Errorf("value mismatch: got %q want %q", gotValue, value)
	}
}

func TestExtractNostrFromValue_SimpleForm(t *testing.T) {
	value := `{"nostr":"b0635d6a9851d3aed0cd6c495b282167acf761729078d975fc341b22650b07b9"}`
	pubkey, relays, err := extractNostrFromValue(value, &ParsedIdentifier{"d/example", "_", true})
	if err != nil {
		t.Fatal(err)
	}
	if pubkey != "b0635d6a9851d3aed0cd6c495b282167acf761729078d975fc341b22650b07b9" {
		t.Errorf("pubkey = %q", pubkey)
	}
	if len(relays) != 0 {
		t.Errorf("expected no relays, got %v", relays)
	}

	if _, _, err := extractNostrFromValue(value, &ParsedIdentifier{"d/example", "alice", true}); err == nil {
		t.Error("expected error for simple-form + non-root local-part")
	}
}

func TestExtractNostrFromValue_ExtendedForm(t *testing.T) {
	value := `{
	  "nostr": {
	    "names": {
	      "_":     "aaaa000000000000000000000000000000000000000000000000000000000001",
	      "alice": "bbbb000000000000000000000000000000000000000000000000000000000002"
	    },
	    "relays": {
	      "bbbb000000000000000000000000000000000000000000000000000000000002": ["wss://relay.example.com"]
	    }
	  }
	}`
	pk, _, err := extractNostrFromValue(value, &ParsedIdentifier{"d/example", "_", true})
	if err != nil {
		t.Fatal(err)
	}
	if pk != "aaaa000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("root pubkey = %q", pk)
	}
	pk, relays, err := extractNostrFromValue(value, &ParsedIdentifier{"d/example", "alice", true})
	if err != nil {
		t.Fatal(err)
	}
	if pk != "bbbb000000000000000000000000000000000000000000000000000000000002" {
		t.Errorf("alice pubkey = %q", pk)
	}
	if len(relays) != 1 || relays[0] != "wss://relay.example.com" {
		t.Errorf("alice relays = %v", relays)
	}
}

func TestExtractNostrFromValue_FallbackToRoot(t *testing.T) {
	value := `{"nostr":{"names":{"_":"aaaa000000000000000000000000000000000000000000000000000000000001"}}}`
	pk, _, err := extractNostrFromValue(value, &ParsedIdentifier{"d/example", "nonexistent", true})
	if err != nil {
		t.Fatal(err)
	}
	if pk != "aaaa000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("pubkey = %q", pk)
	}
}

func TestExtractNostrFromValue_IdentityObject(t *testing.T) {
	value := `{"nostr":{"pubkey":"dddd000000000000000000000000000000000000000000000000000000000004","relays":["wss://relay.id.example"]}}`
	pk, relays, err := extractNostrFromValue(value, &ParsedIdentifier{"id/alice", "_", false})
	if err != nil {
		t.Fatal(err)
	}
	if pk != "dddd000000000000000000000000000000000000000000000000000000000004" {
		t.Errorf("id pubkey = %q", pk)
	}
	if len(relays) != 1 || relays[0] != "wss://relay.id.example" {
		t.Errorf("id relays = %v", relays)
	}
}

func TestExtractNostrFromValue_RejectsInvalidPubkey(t *testing.T) {
	if _, _, err := extractNostrFromValue(`{"nostr":"abcdef"}`, &ParsedIdentifier{"d/x", "_", true}); err == nil {
		t.Error("expected error for short pubkey")
	}
	if _, _, err := extractNostrFromValue(
		`{"nostr":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`,
		&ParsedIdentifier{"d/x", "_", true},
	); err == nil {
		t.Error("expected error for non-hex pubkey")
	}
}

func TestPinnedCertsParse(t *testing.T) {
	pool := buildPinnedCertPool()
	if pool == nil {
		t.Fatal("nil cert pool")
	}
	for i, pem := range PinnedElectrumXCerts {
		if !strings.Contains(pem, "BEGIN CERTIFICATE") || !strings.Contains(pem, "END CERTIFICATE") {
			t.Errorf("pinned cert %d missing BEGIN/END markers", i)
		}
	}
	if fps := pinnedFingerprints(); len(fps) != len(PinnedElectrumXCerts) {
		t.Errorf("pinned fingerprints = %d, want %d (some certs failed to parse)", len(fps), len(PinnedElectrumXCerts))
	}
}
