package bluge

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/blugelabs/bluge"
	"github.com/blugelabs/bluge/search"
	"github.com/nbd-wtf/go-nostr"
)

func (b *BlugeBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)

	if len(filter.Search) < 2 {
		close(ch)
		return ch, nil
	}

	reader, err := b.writer.Reader()
	if err != nil {
		close(ch)
		return nil, fmt.Errorf("unable to open reader: %w", err)
	}

	searchQ := bluge.NewMatchQuery(filter.Search)
	searchQ.SetField(contentField)
	var q bluge.Query = searchQ

	complicatedQuery := bluge.NewBooleanQuery().AddMust(searchQ)

	if len(filter.Kinds) > 0 {
		eitherKind := bluge.NewBooleanQuery()
		eitherKind.SetMinShould(1)
		for _, kind := range filter.Kinds {
			kindQ := bluge.NewTermQuery(strconv.Itoa(kind))
			kindQ.SetField(kindField)
			eitherKind.AddShould(kindQ)
		}
		complicatedQuery.AddMust(eitherKind)
		q = complicatedQuery
	}

	if len(filter.Authors) > 0 {
		eitherPubkey := bluge.NewBooleanQuery()
		eitherPubkey.SetMinShould(1)
		for _, pubkey := range filter.Authors {
			if len(pubkey) != 64 {
				continue
			}
			pubkeyQ := bluge.NewTermQuery(pubkey[56:])
			pubkeyQ.SetField(pubkeyField)
			eitherPubkey.AddShould(pubkeyQ)
		}
		complicatedQuery.AddMust(eitherPubkey)
		q = complicatedQuery
	}

	if filter.Since != nil || filter.Until != nil {
		min := 0.0
		if filter.Since != nil {
			min = float64(*filter.Since)
		}
		max := float64(nostr.Now())
		if filter.Until != nil {
			max = float64(*filter.Until)
		}
		dateRangeQ := bluge.NewNumericRangeInclusiveQuery(min, max, true, true)
		dateRangeQ.SetField(createdAtField)
		complicatedQuery.AddMust(dateRangeQ)
		q = complicatedQuery
	}

	limit := 40
	if filter.Limit != 0 {
		limit = filter.Limit
		if filter.Limit > 150 {
			limit = 150
		}
	}

	req := bluge.NewTopNSearch(limit, q)

	dmi, err := reader.Search(context.Background(), req)
	if err != nil {
		close(ch)
		reader.Close()
		return ch, fmt.Errorf("error executing search: %w", err)
	}

	go func() {
		defer reader.Close()
		defer close(ch)

		var next *search.DocumentMatch
		for next, err = dmi.Next(); next != nil; next, err = dmi.Next() {
			next.VisitStoredFields(func(field string, value []byte) bool {
				id := hex.EncodeToString(value)
				rawch, err := b.RawEventStore.QueryEvents(ctx, nostr.Filter{IDs: []string{id}})
				if err != nil {
					return false
				}
				for evt := range rawch {
					ch <- evt
				}
				return false
			})
		}
		if err != nil {
			return
		}
	}()

	return ch, nil
}
