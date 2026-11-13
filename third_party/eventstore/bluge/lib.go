package bluge

import (
	"fmt"
	"sync"

	"github.com/blugelabs/bluge"
	"github.com/blugelabs/bluge/analysis/token"
	"github.com/fiatjaf/eventstore"
	"golang.org/x/text/unicode/norm"
)

var _ eventstore.Store = (*BlugeBackend)(nil)

type BlugeBackend struct {
	sync.Mutex
	// Path is where the index will be saved
	Path string

	// RawEventStore is where we'll fetch the raw events from
	// bluge will only store ids, so the actual events must be somewhere else
	RawEventStore eventstore.Store

	searchConfig bluge.Config
	writer       *bluge.Writer
}

func (b *BlugeBackend) Close() {
	defer b.writer.Close()
}

func (b *BlugeBackend) Init() error {
	if b.Path == "" {
		return fmt.Errorf("missing Path")
	}
	if b.RawEventStore == nil {
		return fmt.Errorf("missing RawEventStore")
	}

	b.searchConfig = bluge.DefaultConfig(b.Path)
	b.searchConfig.DefaultSearchAnalyzer.TokenFilters = append(b.searchConfig.DefaultSearchAnalyzer.TokenFilters,
		token.NewUnicodeNormalizeFilter(norm.NFKC),
	)

	var err error
	b.writer, err = bluge.OpenWriter(b.searchConfig)
	if err != nil {
		return fmt.Errorf("error opening writer: %w", err)
	}

	return nil
}
