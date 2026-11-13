package edgedb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

var (
	ErrTooManyIDs       = errors.New("too many ids")
	ErrTooManyAuthors   = errors.New("too many authors")
	ErrTooManyKinds     = errors.New("too many kinds")
	ErrEmptyTagSet      = errors.New("empty tag set")
	ErrTooManyTagValues = errors.New("too many tag values")
)

// QueryEvents is an implementation of the QueryEvents method of the eventstore.Store interfac for edgedb
func (b *EdgeDBBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	query, args, err := b.queryEventsEdgeql(filter, false)
	if err != nil {
		return nil, err
	}
	var events []Event
	if err := b.Query(ctx, query, &events, args); err != nil {
		return nil, fmt.Errorf("failed to fetch events using query %s: %w", query, err)
	}
	ch := make(chan *nostr.Event)
	go func() {
		defer close(ch)
		for _, event := range events {
			e, err := EdgeDBEventToNostrEvent(event)
			if err != nil {
				panic(fmt.Errorf("failed to fetch events using query %s: %w", query, err))
			}
			select {
			case ch <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// queryEventsEdgeql builds the edgeql query based on the applied filters
func (b *EdgeDBBackend) queryEventsEdgeql(filter nostr.Filter, doCount bool) (string, map[string]interface{}, error) {
	var (
		conditions []string
		query      string
	)
	args := map[string]interface{}{}
	if len(filter.IDs) > 0 {
		if len(filter.IDs) > b.QueryIDsLimit {
			return query, args, ErrTooManyIDs
		}
		conditions = append(conditions, `events::Event.eventId IN array_unpack(<array<str>>$ids)`)
		args["ids"] = filter.IDs
	}

	if len(filter.Authors) > 0 {
		if len(filter.Authors) > b.QueryAuthorsLimit {
			return query, args, ErrTooManyAuthors
		}
		conditions = append(conditions, `events::Event.pubkey IN array_unpack(<array<str>>$authors)`)
		args["authors"] = filter.Authors
	}

	if len(filter.Kinds) > 0 {
		if len(filter.Kinds) > b.QueryKindsLimit {
			return query, args, ErrTooManyKinds
		}
		conditions = append(conditions, `events::Event.kind IN array_unpack(<array<int64>>$kinds)`)
		int64Kinds := []int64{}
		for _, k := range filter.Kinds {
			int64Kinds = append(int64Kinds, int64(k))
		}
		args["kinds"] = int64Kinds
	}
	/*
		SELECT events::Event {*} FILTER (
			with ts := (
				for tag in array_unpack(.tags) UNION (
					SELECT tag[1] if count(json_array_unpack(tag)) > 1 else <json>'' FILTER tag[0] = <json>'x'
				)
			)
			SELECT EXISTS (SELECT ts INTERSECT {<json>'y', <json>'z'})
		);
	*/
	for letter, values := range filter.Tags {
		if len(values) == 0 {
			return query, args, ErrEmptyTagSet
		}
		if len(values) > b.QueryTagsLimit {
			return query, args, ErrTooManyTagValues
		}
		jsonSet := func(vals []string) string {
			var set []string
			for _, val := range vals {
				set = append(set, "<json>'"+val+"'")
			}
			return strings.Join(set, ", ")
		}
		conditions = append(conditions, fmt.Sprintf(`(
			with ts := (
				for tag in array_unpack(events::Event.tags) UNION (
					SELECT tag[1] if count(json_array_unpack(tag)) > 1 else <json>'' FILTER (tag[0] if count(json_array_unpack(tag)) > 0 else <json>'') = <json>'%s'
				)
			)
			SELECT EXISTS (SELECT ts INTERSECT {%s})
		)`, letter, jsonSet(values)))

	}

	if filter.Since != nil {
		conditions = append(conditions, `events::Event.createdAt >= <datetime>$since`)
		args["since"] = filter.Since.Time()
	}
	if filter.Until != nil {
		conditions = append(conditions, `events::Event.createdAt <= <datetime>$until`)
		args["until"] = filter.Until.Time()
	}
	if filter.Search != "" {
		conditions = append(conditions, `events::Event.content LIKE <str>$search`)
		args["search"] = "%" + strings.ReplaceAll(filter.Search, "%", `\%`) + "%"
	}
	query = "SELECT events::Event {*}"
	if doCount {
		query = "SELECT count(events::Event)"
	}
	if len(conditions) > 0 {
		query += " FILTER " + strings.Join(conditions, " AND ")
	}
	if !doCount {
		query += " ORDER BY events::Event.createdAt DESC"
	}
	query += " LIMIT <int64>$limit"
	args["limit"] = int64(filter.Limit)
	if filter.Limit < 1 || filter.Limit > b.QueryLimit {
		args["limit"] = int64(b.QueryLimit)
	}
	return query, args, nil
}
