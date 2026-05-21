package nip05namecoin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestLoadNamesFile_Empty(t *testing.T) {
	names, err := LoadNamesFile("")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty map for empty path, got %v", names)
	}
}

func TestLoadNamesFile_Parses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "names.json")
	body := `["me@me.bit", "d/example", "  id/alice  ", ""]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	names, err := LoadNamesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"me@me.bit", "d/example", "id/alice"}
	if len(names) != len(want) {
		t.Fatalf("got %d entries, want %d (%v)", len(names), len(want), names)
	}
	for _, w := range want {
		if _, ok := names[w]; !ok {
			t.Errorf("missing entry %q in %v", w, names)
		}
	}
}

func TestLoadNamesFile_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "names.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNamesFile(path); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestLoadNamesFile_Missing(t *testing.T) {
	if _, err := LoadNamesFile("/nonexistent/path/should/not/exist.json"); err == nil {
		t.Error("expected error for missing file")
	}
}

// captureLogger records Printf calls so tests can assert on log output.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// We don't care about the formatted output here, only the count;
	// the format string itself is descriptive enough.
	_ = format
	_ = args
	l.lines = append(l.lines, format)
}

func TestResolveNamesToPubkeys_SuccessAndFailure(t *testing.T) {
	target := map[string]struct{}{
		"existing-pubkey": {},
	}
	names := map[string]struct{}{
		"me@example.bit":    {},
		"broken@broken.bit": {},
	}
	resolved := "b0635d6a9851d3aed0cd6c495b282167acf761729078d975fc341b22650b07b9"
	fake := func(_ context.Context, identifier string) (*nostr.ProfilePointer, error) {
		switch identifier {
		case "me@example.bit":
			return &nostr.ProfilePointer{PublicKey: resolved}, nil
		case "broken@broken.bit":
			return nil, errors.New("transport failure")
		default:
			return nil, errors.New("unexpected: " + identifier)
		}
	}
	logger := &captureLogger{}

	ResolveNamesToPubkeys(target, names, ResolveOptions{
		Resolver: fake,
		Logger:   logger,
	})

	if _, ok := target[resolved]; !ok {
		t.Errorf("expected resolved pubkey %s in target, got %v", resolved, target)
	}
	if _, ok := target["existing-pubkey"]; !ok {
		t.Errorf("existing entries should be preserved, got %v", target)
	}
	if len(target) != 2 {
		t.Errorf("expected exactly 2 entries (existing + resolved), got %d: %v", len(target), target)
	}

	// We expect a "resolving N names" line + a warning + a success line.
	if len(logger.lines) < 3 {
		t.Errorf("expected at least 3 log lines, got %d: %v", len(logger.lines), logger.lines)
	}
}

func TestResolveNamesToPubkeys_NoNames(t *testing.T) {
	target := map[string]struct{}{"existing": {}}
	called := false
	fake := func(_ context.Context, _ string) (*nostr.ProfilePointer, error) {
		called = true
		return nil, nil
	}
	ResolveNamesToPubkeys(target, map[string]struct{}{}, ResolveOptions{Resolver: fake})
	if called {
		t.Error("resolver should not be called when no names are configured")
	}
	if _, ok := target["existing"]; !ok {
		t.Error("existing target entries should be preserved")
	}
}

func TestResolveNamesToPubkeys_EmptyPubkeyIgnored(t *testing.T) {
	target := map[string]struct{}{}
	names := map[string]struct{}{"empty@empty.bit": {}}
	fake := func(_ context.Context, _ string) (*nostr.ProfilePointer, error) {
		return &nostr.ProfilePointer{PublicKey: ""}, nil
	}
	ResolveNamesToPubkeys(target, names, ResolveOptions{Resolver: fake})
	if len(target) != 0 {
		t.Errorf("expected empty target when resolver returns empty pubkey, got %v", target)
	}
}

func TestResolveNamesToPubkeys_NilTarget(t *testing.T) {
	// A nil target is a no-op (defensive): the helper just returns.
	// This guards against a panic in case the caller forgets to
	// initialise the whitelist map.
	names := map[string]struct{}{"me@me.bit": {}}
	called := false
	fake := func(_ context.Context, _ string) (*nostr.ProfilePointer, error) {
		called = true
		return &nostr.ProfilePointer{PublicKey: "x"}, nil
	}
	ResolveNamesToPubkeys(nil, names, ResolveOptions{Resolver: fake})
	if called {
		t.Error("resolver should not be called when target is nil")
	}
}
