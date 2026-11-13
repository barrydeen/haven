package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
)

var _ eventstore.Store = (*ElasticsearchStorage)(nil)

type IndexedEvent struct {
	Event         nostr.Event `json:"event"`
	ContentSearch string      `json:"content_search"`
}

var indexMapping = `
{
	"settings": {
		"number_of_shards": 1,
		"number_of_replicas": 0
	},
	"mappings": {
		"dynamic": false,
		"properties": {
			"event": {
				"dynamic": false,
				"properties": {
					"id": {"type": "keyword"},
					"pubkey": {"type": "keyword"},
					"kind": {"type": "integer"},
					"tags": {"type": "keyword"},
					"created_at": {"type": "date"}
				}
			},
			"content_search": {"type": "text"}
		}
	}
}
`

type ElasticsearchStorage struct {
	sync.Mutex
	URL       string
	IndexName string

	es *elasticsearch.Client
	bi esutil.BulkIndexer
}

func (ess *ElasticsearchStorage) Close() {}

func (ess *ElasticsearchStorage) Init() error {
	if ess.IndexName == "" {
		ess.IndexName = "events"
	}

	cfg := elasticsearch.Config{}
	if ess.URL != "" {
		cfg.Addresses = strings.Split(ess.URL, ",")
	}
	es, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return err
	}

	res, err := es.Indices.Create(ess.IndexName, es.Indices.Create.WithBody(strings.NewReader(indexMapping)))
	if err != nil {
		return err
	}
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		txt := string(body)
		if !strings.Contains(txt, "resource_already_exists_exception") {
			return fmt.Errorf("%s", txt)
		}
	}

	// bulk indexer
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         ess.IndexName,
		Client:        es,
		NumWorkers:    2,
		FlushInterval: 3 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("error creating the indexer: %s", err)
	}

	ess.es = es
	ess.bi = bi

	return nil
}

func (ess *ElasticsearchStorage) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	done := make(chan error)
	err := ess.bi.Add(
		ctx,
		esutil.BulkIndexerItem{
			Action:     "delete",
			DocumentID: evt.ID,
			OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem) {
				close(done)
			},
			OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
				if err != nil {
					done <- err
				} else {
					// ok if deleted item not found
					if res.Status == 404 {
						close(done)
						return
					}
					txt, _ := json.Marshal(res)
					err := fmt.Errorf("ERROR: %s", txt)
					done <- err
				}
			},
		},
	)
	if err != nil {
		return err
	}

	err = <-done
	return err
}

func (ess *ElasticsearchStorage) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	ess.Lock()
	defer ess.Unlock()

	filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
	if nostr.IsAddressableKind(evt.Kind) {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	ch, err := ess.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to query before replacing: %w", err)
	}

	shouldStore := true
	for previous := range ch {
		if internal.IsOlder(previous, evt) {
			if err := ess.DeleteEvent(ctx, previous); err != nil {
				return fmt.Errorf("failed to delete event for replacing: %w", err)
			}
		} else {
			shouldStore = false
		}
	}

	if shouldStore {
		if err := ess.SaveEvent(ctx, evt); err != nil && err != eventstore.ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	return nil
}

func (ess *ElasticsearchStorage) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	ie := &IndexedEvent{
		Event: *evt,
	}

	// post processing: index for FTS
	// some ideas:
	// - index kind=0 fields a set of dedicated mapped fields
	//   (or use a separate index for profiles with a dedicated mapping)
	// - if it's valid JSON just index the "values" and not the keys
	// - more content introspection: language detection
	// - denormalization... attach profile + ranking signals to events
	if evt.Kind != 4 {
		ie.ContentSearch = evt.Content
	}

	data, err := json.Marshal(ie)
	if err != nil {
		return err
	}

	done := make(chan error)

	// adapted from:
	// https://github.com/elastic/go-elasticsearch/blob/main/_examples/bulk/indexer.go#L196
	err = ess.bi.Add(
		ctx,
		esutil.BulkIndexerItem{
			Action:     "index",
			DocumentID: evt.ID,
			Body:       bytes.NewReader(data),
			OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem) {
				close(done)
			},
			OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
				if err != nil {
					done <- err
				} else {
					err := fmt.Errorf("ERROR: %s: %s", res.Error.Type, res.Error.Reason)
					done <- err
				}
			},
		},
	)
	if err != nil {
		return err
	}

	err = <-done
	return err
}
