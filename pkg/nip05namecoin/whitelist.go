package nip05namecoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// ResolverFunc is the resolver signature consumed by
// ResolveNamesToPubkeys. Extracted so callers (and tests) can swap in
// a fake. The default production resolver is QueryIdentifier.
type ResolverFunc func(ctx context.Context, identifier string) (*nostr.ProfilePointer, error)

// Logger is the minimal logging surface used by ResolveNamesToPubkeys.
// HAVEN's main package wires its standard logger in; tests use a
// no-op.
type Logger interface {
	Printf(format string, args ...any)
}

// ResolveOptions tunes the startup resolution pass. Zero values are
// sensible defaults.
type ResolveOptions struct {
	// PerNameTimeout bounds each individual Namecoin lookup. Defaults
	// to 30s when zero.
	PerNameTimeout time.Duration
	// Resolver overrides the default QueryIdentifier. Useful for
	// tests; production code can leave this nil.
	Resolver ResolverFunc
	// Logger overrides the destination for status logs. nil means
	// "discard".
	Logger Logger
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// ResolveNamesToPubkeys resolves each .bit / d/ / id/ identifier in
// `names` to its underlying hex pubkey and writes the result into
// `target`. Failures are logged and skipped — they never abort the
// caller. The same target map can be passed across multiple calls;
// existing entries are preserved.
func ResolveNamesToPubkeys(target map[string]struct{}, names map[string]struct{}, opts ResolveOptions) {
	if len(names) == 0 {
		return
	}
	if target == nil {
		return
	}
	timeout := opts.PerNameTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = QueryIdentifier
	}
	logger := opts.Logger
	if logger == nil {
		logger = discardLogger{}
	}

	logger.Printf("resolving %d Namecoin (.bit) whitelist name(s) via ElectrumX", len(names))
	for name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		pp, err := resolver(ctx, name)
		cancel()
		if err != nil {
			logger.Printf("WARN: failed to resolve Namecoin name %q: %v", name, err)
			continue
		}
		if pp == nil || pp.PublicKey == "" {
			logger.Printf("WARN: Namecoin name %q resolved to empty pubkey", name)
			continue
		}
		target[pp.PublicKey] = struct{}{}
		logger.Printf("resolved Namecoin name %q to pubkey %s", name, pp.PublicKey)
	}
}

// LoadNamesFile reads a JSON array of Namecoin identifiers from
// `filePath` and returns them as a set. Whitespace is trimmed and
// empty entries dropped. Returns an empty (non-nil) map when
// filePath == "".
func LoadNamesFile(filePath string) (map[string]struct{}, error) {
	names := map[string]struct{}{}
	if filePath == "" {
		return names, nil
	}
	file, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("nip05namecoin: read names file: %w", err)
	}
	var list []string
	if err := json.Unmarshal(file, &list); err != nil {
		return nil, fmt.Errorf("nip05namecoin: parse names file: %w", err)
	}
	for _, name := range list {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		names[name] = struct{}{}
	}
	return names, nil
}

// ErrEmptyPubkey is returned when a resolver succeeds but yields an
// empty PublicKey. Exposed mostly for tests; production callers treat
// it the same as any other resolution failure.
var ErrEmptyPubkey = errors.New("nip05namecoin: resolver returned empty pubkey")
