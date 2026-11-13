package badger

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
)

type query struct {
	i             int
	prefix        []byte
	startingPoint []byte
	skipTimestamp bool
}

func prepareQueries(filter nostr.Filter) (
	queries []query,
	extraFilter *nostr.Filter,
	since uint32,
	err error,
) {
	// these things have to run for every result we return
	defer func() {
		if queries == nil {
			return
		}

		var until uint32 = 4294967295
		if filter.Until != nil {
			if fu := uint32(*filter.Until); fu < until {
				until = fu + 1
			}
		}

		for i, q := range queries {
			queries[i].startingPoint = binary.BigEndian.AppendUint32(q.prefix, uint32(until))
		}

		// this is where we'll end the iteration
		if filter.Since != nil {
			if fs := uint32(*filter.Since); fs > since {
				since = fs
			}
		}
	}()

	var index byte

	if len(filter.IDs) > 0 {
		queries = make([]query, len(filter.IDs))
		for i, idHex := range filter.IDs {
			prefix := make([]byte, 1+8)
			prefix[0] = indexIdPrefix
			if len(idHex) != 64 {
				return nil, nil, 0, fmt.Errorf("invalid id '%s'", idHex)
			}
			hex.Decode(prefix[1:], []byte(idHex[0:8*2]))
			queries[i] = query{i: i, prefix: prefix, skipTimestamp: true}
		}

		return queries, extraFilter, since, nil
	}

	if len(filter.Tags) > 0 {
		// we will select ONE tag to query with
		tagKey, tagValues, goodness := internal.ChooseNarrowestTag(filter)

		// we won't use a tag index for this as long as we have something else to match with
		if goodness < 3 && (len(filter.Authors) > 0 || len(filter.Kinds) > 0) {
			goto pubkeyMatching
		}

		queries = make([]query, len(tagValues))
		for i, value := range tagValues {
			// get key prefix (with full length) and offset where to write the created_at
			k, offset := getTagIndexPrefix(tagKey, value)
			// remove the last parts part to get just the prefix we want here
			prefix := k[0:offset]
			queries[i] = query{i: i, prefix: prefix}
			i++
		}

		extraFilter = &nostr.Filter{
			Kinds:   filter.Kinds,
			Authors: filter.Authors,
			Tags:    internal.CopyMapWithoutKey(filter.Tags, tagKey),
		}

		return queries, extraFilter, since, nil
	}

pubkeyMatching:
	if len(filter.Authors) > 0 {
		if len(filter.Kinds) == 0 {
			queries = make([]query, len(filter.Authors))
			for i, pubkeyHex := range filter.Authors {
				if len(pubkeyHex) != 64 {
					return nil, nil, 0, fmt.Errorf("invalid pubkey '%s'", pubkeyHex)
				}
				prefix := make([]byte, 1+8)
				prefix[0] = indexPubkeyPrefix
				hex.Decode(prefix[1:], []byte(pubkeyHex[0:8*2]))
				queries[i] = query{i: i, prefix: prefix}
			}
		} else {
			queries = make([]query, len(filter.Authors)*len(filter.Kinds))
			i := 0
			for _, pubkeyHex := range filter.Authors {
				for _, kind := range filter.Kinds {
					if len(pubkeyHex) != 64 {
						return nil, nil, 0, fmt.Errorf("invalid pubkey '%s'", pubkeyHex)
					}

					prefix := make([]byte, 1+8+2)
					prefix[0] = indexPubkeyKindPrefix
					hex.Decode(prefix[1:], []byte(pubkeyHex[0:8*2]))
					binary.BigEndian.PutUint16(prefix[1+8:], uint16(kind))
					queries[i] = query{i: i, prefix: prefix}
					i++
				}
			}
		}
		extraFilter = &nostr.Filter{Tags: filter.Tags}
	} else if len(filter.Kinds) > 0 {
		index = indexKindPrefix
		queries = make([]query, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			prefix := make([]byte, 1+2)
			prefix[0] = index
			binary.BigEndian.PutUint16(prefix[1:], uint16(kind))
			queries[i] = query{i: i, prefix: prefix}
		}
		extraFilter = &nostr.Filter{Tags: filter.Tags}
	} else {
		index = indexCreatedAtPrefix
		queries = make([]query, 1)
		prefix := make([]byte, 1)
		prefix[0] = index
		queries[0] = query{i: 0, prefix: prefix}
		extraFilter = nil
	}

	return queries, extraFilter, since, nil
}
