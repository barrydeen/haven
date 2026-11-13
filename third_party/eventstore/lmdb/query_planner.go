package lmdb

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/fiatjaf/eventstore/internal"
	"github.com/nbd-wtf/go-nostr"
)

type query struct {
	i             int
	dbi           lmdb.DBI
	prefix        []byte
	results       chan *nostr.Event
	keySize       int
	timestampSize int
	startingPoint []byte
}

func (b *LMDBBackend) prepareQueries(filter nostr.Filter) (
	queries []query,
	extraAuthors [][32]byte,
	extraKinds [][2]byte,
	extraTagKey string,
	extraTagValues []string,
	since uint32,
	err error,
) {
	// we will apply this to every query we return
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
			sp := make([]byte, len(q.prefix))
			sp = sp[0:len(q.prefix)]
			copy(sp, q.prefix)
			queries[i].startingPoint = binary.BigEndian.AppendUint32(sp, uint32(until))
			queries[i].results = make(chan *nostr.Event, 12)
		}
	}()

	if filter.IDs != nil {
		// when there are ids we ignore everything else
		queries = make([]query, len(filter.IDs))
		for i, idHex := range filter.IDs {
			if len(idHex) != 64 {
				return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid id '%s'", idHex)
			}
			prefix := make([]byte, 8)
			if _, err := hex.Decode(prefix[0:8], []byte(idHex[0:8*2])); err != nil {
				return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid id '%s'", idHex)
			}
			queries[i] = query{i: i, dbi: b.indexId, prefix: prefix[0:8], keySize: 8, timestampSize: 0}
		}
		return queries, nil, nil, "", nil, 0, nil
	}

	// this is where we'll end the iteration
	if filter.Since != nil {
		if fs := uint32(*filter.Since); fs > since {
			since = fs
		}
	}

	if len(filter.Tags) > 0 {
		// we will select ONE tag to query for and ONE extra tag to do further narrowing, if available
		tagKey, tagValues, goodness := internal.ChooseNarrowestTag(filter)

		// we won't use a tag index for this as long as we have something else to match with
		if goodness < 2 && (len(filter.Authors) > 0 || len(filter.Kinds) > 0) {
			goto pubkeyMatching
		}

		// only "p" tag has a goodness of 2, so
		if goodness == 2 {
			// this means we got a "p" tag, so we will use the ptag-kind index
			i := 0
			if filter.Kinds != nil {
				queries = make([]query, len(tagValues)*len(filter.Kinds))
				for _, value := range tagValues {
					if len(value) != 64 {
						return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid 'p' tag '%s'", value)
					}

					for _, kind := range filter.Kinds {
						k := make([]byte, 8+2)
						if _, err := hex.Decode(k[0:8], []byte(value[0:8*2])); err != nil {
							return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid 'p' tag '%s'", value)
						}
						binary.BigEndian.PutUint16(k[8:8+2], uint16(kind))
						queries[i] = query{i: i, dbi: b.indexPTagKind, prefix: k[0 : 8+2], keySize: 8 + 2 + 4, timestampSize: 4}
						i++
					}
				}
			} else {
				// even if there are no kinds, in that case we will just return any kind and not care
				queries = make([]query, len(tagValues))
				for i, value := range tagValues {
					if len(value) != 64 {
						return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid 'p' tag '%s'", value)
					}

					k := make([]byte, 8)
					if _, err := hex.Decode(k[0:8], []byte(value[0:8*2])); err != nil {
						return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid 'p' tag '%s'", value)
					}
					queries[i] = query{i: i, dbi: b.indexPTagKind, prefix: k[0:8], keySize: 8 + 2 + 4, timestampSize: 4}
				}
			}
		} else {
			// otherwise we will use a plain tag index
			queries = make([]query, len(tagValues))
			for i, value := range tagValues {
				// get key prefix (with full length) and offset where to write the created_at
				dbi, k, offset := b.getTagIndexPrefix(tagKey, value)
				// remove the last parts part to get just the prefix we want here
				prefix := k[0:offset]
				queries[i] = query{i: i, dbi: dbi, prefix: prefix, keySize: len(prefix) + 4, timestampSize: 4}
				i++
			}

			// add an extra kind filter if available (only do this on plain tag index, not on ptag-kind index)
			if filter.Kinds != nil {
				extraKinds = make([][2]byte, len(filter.Kinds))
				for i, kind := range filter.Kinds {
					binary.BigEndian.PutUint16(extraKinds[i][0:2], uint16(kind))
				}
			}
		}

		// add an extra author search if possible
		if filter.Authors != nil {
			extraAuthors = make([][32]byte, len(filter.Authors))
			for i, pk := range filter.Authors {
				hex.Decode(extraAuthors[i][:], []byte(pk))
			}
		}

		// add an extra useless tag if available
		filter.Tags = internal.CopyMapWithoutKey(filter.Tags, tagKey)
		if len(filter.Tags) > 0 {
			extraTagKey, extraTagValues, _ = internal.ChooseNarrowestTag(filter)
		}

		return queries, extraAuthors, extraKinds, extraTagKey, extraTagValues, since, nil
	}

pubkeyMatching:
	if len(filter.Authors) > 0 {
		if len(filter.Kinds) == 0 {
			// will use pubkey index
			queries = make([]query, len(filter.Authors))
			for i, pubkeyHex := range filter.Authors {
				if len(pubkeyHex) != 64 {
					return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid author '%s'", pubkeyHex)
				}
				prefix := make([]byte, 8)
				if _, err := hex.Decode(prefix[0:8], []byte(pubkeyHex[0:8*2])); err != nil {
					return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid author '%s'", pubkeyHex)
				}
				queries[i] = query{i: i, dbi: b.indexPubkey, prefix: prefix[0:8], keySize: 8 + 4, timestampSize: 4}
			}
		} else {
			// will use pubkeyKind index
			queries = make([]query, len(filter.Authors)*len(filter.Kinds))
			i := 0
			for _, pubkeyHex := range filter.Authors {
				for _, kind := range filter.Kinds {
					if len(pubkeyHex) != 64 {
						return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid author '%s'", pubkeyHex)
					}
					prefix := make([]byte, 8+2)
					if _, err := hex.Decode(prefix[0:8], []byte(pubkeyHex[0:8*2])); err != nil {
						return nil, nil, nil, "", nil, 0, fmt.Errorf("invalid author '%s'", pubkeyHex)
					}
					binary.BigEndian.PutUint16(prefix[8:8+2], uint16(kind))
					queries[i] = query{i: i, dbi: b.indexPubkeyKind, prefix: prefix[0 : 8+2], keySize: 10 + 4, timestampSize: 4}
					i++
				}
			}
		}

		// potentially with an extra useless tag filtering
		extraTagKey, extraTagValues, _ = internal.ChooseNarrowestTag(filter)
		return queries, nil, nil, extraTagKey, extraTagValues, since, nil
	}

	if len(filter.Kinds) > 0 {
		// will use a kind index
		queries = make([]query, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			prefix := make([]byte, 2)
			binary.BigEndian.PutUint16(prefix[0:2], uint16(kind))
			queries[i] = query{i: i, dbi: b.indexKind, prefix: prefix[0:2], keySize: 2 + 4, timestampSize: 4}
		}

		// potentially with an extra useless tag filtering
		tagKey, tagValues, _ := internal.ChooseNarrowestTag(filter)
		return queries, nil, nil, tagKey, tagValues, since, nil
	}

	// if we got here our query will have nothing to filter with
	queries = make([]query, 1)
	prefix := make([]byte, 0)
	queries[0] = query{i: 0, dbi: b.indexCreatedAt, prefix: prefix, keySize: 0 + 4, timestampSize: 4}
	return queries, nil, nil, "", nil, since, nil
}
