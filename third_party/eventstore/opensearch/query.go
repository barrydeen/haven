package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/aquasecurity/esquery"
	"github.com/nbd-wtf/go-nostr"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v4/opensearchutil"
)

func buildDsl(filter nostr.Filter) ([]byte, error) {
	dsl := esquery.Bool()

	prefixFilter := func(fieldName string, values []string) {
		if len(values) == 0 {
			return
		}
		prefixQ := esquery.Bool()
		for _, v := range values {
			if len(v) < 64 {
				prefixQ.Should(esquery.Prefix(fieldName, v))
			} else {
				prefixQ.Should(esquery.Term(fieldName, v))
			}
		}
		dsl.Must(prefixQ)
	}

	// ids
	prefixFilter("event.id", filter.IDs)

	// authors
	prefixFilter("event.pubkey", filter.Authors)

	// kinds
	if len(filter.Kinds) > 0 {
		dsl.Must(esquery.Terms("event.kind", toInterfaceSlice(filter.Kinds)...))
	}

	// tags
	if len(filter.Tags) > 0 {
		tagQ := esquery.Bool()
		for char, terms := range filter.Tags {
			vs := toInterfaceSlice(append(terms, char))
			tagQ.Should(esquery.Terms("event.tags", vs...))
		}
		dsl.Must(tagQ)
	}

	// since
	if filter.Since != nil {
		dsl.Must(esquery.Range("event.created_at").Gte(filter.Since))
	}

	// until
	if filter.Until != nil {
		dsl.Must(esquery.Range("event.created_at").Lte(filter.Until))
	}

	// search
	if filter.Search != "" {
		dsl.Must(esquery.Match("content_search", filter.Search))
	}

	return json.Marshal(esquery.Query(dsl))
}

func (oss *OpensearchStorage) getByID(filter nostr.Filter) ([]*nostr.Event, error) {
	ctx := context.Background()
	mgetResponse, err := oss.client.MGet(
		ctx,
		opensearchapi.MGetReq{
			Body:  opensearchutil.NewJSONReader(filter),
			Index: oss.IndexName,
		},
	)
	if err != nil {
		return nil, err
	}

	events := make([]*nostr.Event, 0, len(mgetResponse.Docs))
	for _, e := range mgetResponse.Docs {
		if e.Found {
			if b, err := e.Source.MarshalJSON(); err == nil {
				var payload struct {
					Event nostr.Event `json:"event"`
				}
				if err = json.Unmarshal(b, &payload); err == nil {
					events = append(events, &payload.Event)
				}
			}
		}
	}

	return events, nil
}

func (oss *OpensearchStorage) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)

	// optimization: get by id
	if isGetByID(filter) {
		if evts, err := oss.getByID(filter); err == nil {
			for _, evt := range evts {
				ch <- evt
			}
			close(ch)
			ch = nil
		} else {
			return nil, fmt.Errorf("error getting by id: %w", err)
		}
	}

	dsl, err := buildDsl(filter)
	if err != nil {
		return nil, err
	}

	limit := 1000
	if filter.Limit > 0 && filter.Limit < limit {
		limit = filter.Limit
	}

	ctx = context.Background()
	searchResponse, err := oss.client.Search(
		ctx,
		&opensearchapi.SearchReq{
			Indices: []string{oss.IndexName},
			Body:    bytes.NewReader(dsl),
			Params: opensearchapi.SearchParams{
				Size: opensearchapi.ToPointer(limit),
				Sort: []string{"event.created_at:desc", "event.id"},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	go func() {
		for _, e := range searchResponse.Hits.Hits {
			if b, err := e.Source.MarshalJSON(); err == nil {
				var payload struct {
					Event nostr.Event `json:"event"`
				}
				if err = json.Unmarshal(b, &payload); err == nil {
					ch <- &payload.Event
				}
			}
		}
		if ch != nil {
			close(ch)
			ch = nil
		}
	}()

	return ch, nil
}

func isGetByID(filter nostr.Filter) bool {
	isGetById := len(filter.IDs) > 0 &&
		len(filter.Authors) == 0 &&
		len(filter.Kinds) == 0 &&
		len(filter.Tags) == 0 &&
		len(filter.Search) == 0 &&
		filter.Since == nil &&
		filter.Until == nil

	if isGetById {
		for _, id := range filter.IDs {
			if len(id) != 64 {
				return false
			}
		}
	}
	return isGetById
}

// from: https://stackoverflow.com/a/12754757
func toInterfaceSlice(slice interface{}) []interface{} {
	s := reflect.ValueOf(slice)
	if s.Kind() != reflect.Slice {
		panic("InterfaceSlice() given a non-slice type")
	}

	// Keep the distinction between nil and empty slice input
	if s.IsNil() {
		return nil
	}

	ret := make([]interface{}, s.Len())

	for i := 0; i < s.Len(); i++ {
		ret[i] = s.Index(i).Interface()
	}

	return ret
}

func (oss *OpensearchStorage) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	count := int64(0)

	// optimization: get by id
	if isGetByID(filter) {
		if evts, err := oss.getByID(filter); err == nil {
			count += int64(len(evts))
		} else {
			return 0, fmt.Errorf("error getting by id: %w", err)
		}
	}

	dsl, err := buildDsl(filter)
	if err != nil {
		return 0, err
	}

	ctx = context.Background()
	countRes, err := oss.client.Indices.Count(
		ctx,
		&opensearchapi.IndicesCountReq{
			Indices: []string{oss.IndexName},
			Body:    bytes.NewReader(dsl),
		},
	)
	if err != nil {
		return 0, err
	}

	return int64(countRes.Count) + count, nil
}
