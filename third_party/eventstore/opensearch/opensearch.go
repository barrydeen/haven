package opensearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v4/opensearchutil"
)

var _ eventstore.Store = (*OpensearchStorage)(nil)

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

type OpensearchStorage struct {
	sync.Mutex
	URL       string
	IndexName string
	Insecure  bool

	client *opensearchapi.Client
	bi     opensearchutil.BulkIndexer
}

func (oss *OpensearchStorage) Close() {}

func (oss *OpensearchStorage) Init() error {
	if oss.IndexName == "" {
		oss.IndexName = "events"
	}

	cfg := opensearchapi.Config{}
	if oss.URL != "" {
		cfg.Client.Addresses = strings.Split(oss.URL, ",")
	}
	if oss.Insecure {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		cfg.Client.Transport = transport
	}

	client, err := opensearchapi.NewClient(cfg)
	if err != nil {
		return err
	}

	ctx := context.Background()
	createIndexResponse, err := client.Indices.Create(
		ctx,
		opensearchapi.IndicesCreateReq{
			Index: oss.IndexName,
			Body:  strings.NewReader(indexMapping),
		},
	)
	if err != nil {
		var opensearchError *opensearch.StructError

		// Load err into opensearch.Error to access the fields and tolerate if the index already exists
		if errors.As(err, &opensearchError) {
			if opensearchError.Err.Type != "resource_already_exists_exception" {
				return err
			}
		} else {
			return err
		}
	}
	log.Printf("Created Index: %s\n  Shards Acknowledged: %t\n", createIndexResponse.Index, createIndexResponse.ShardsAcknowledged)

	// bulk indexer
	bi, err := opensearchutil.NewBulkIndexer(opensearchutil.BulkIndexerConfig{
		Index:         oss.IndexName,
		Client:        client,
		NumWorkers:    2,
		FlushInterval: 3 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("error creating the indexer: %s", err)
	}

	oss.client = client
	oss.bi = bi

	return nil
}

func (oss *OpensearchStorage) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	done := make(chan error)
	err := oss.bi.Add(
		ctx,
		opensearchutil.BulkIndexerItem{
			Action:     "delete",
			DocumentID: evt.ID,
			OnSuccess: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchapi.BulkRespItem) {
				close(done)
			},
			OnFailure: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchapi.BulkRespItem, err error) {
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

func (oss *OpensearchStorage) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	oss.Lock()
	defer oss.Unlock()

	filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
	if nostr.IsAddressableKind(evt.Kind) {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	ch, err := oss.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to query before replacing: %w", err)
	}

	shouldStore := true
	for previous := range ch {
		if internal.IsOlder(previous, evt) {
			if err := oss.DeleteEvent(ctx, previous); err != nil {
				return fmt.Errorf("failed to delete event for replacing: %w", err)
			}
		} else {
			shouldStore = false
		}
	}

	if shouldStore {
		if err := oss.SaveEvent(ctx, evt); err != nil && err != eventstore.ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	return nil
}

func (oss *OpensearchStorage) SaveEvent(ctx context.Context, evt *nostr.Event) error {
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
	err = oss.bi.Add(
		ctx,
		opensearchutil.BulkIndexerItem{
			Action:     "index",
			DocumentID: evt.ID,
			Body:       bytes.NewReader(data),
			OnSuccess: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchapi.BulkRespItem) {
				close(done)
			},
			OnFailure: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchapi.BulkRespItem, err error) {
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
