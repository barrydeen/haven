package lmdb

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"iter"
	"strconv"
	"strings"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/nbd-wtf/go-nostr"
	"golang.org/x/exp/slices"
)

// this iterator always goes backwards
type iterator struct {
	cursor *lmdb.Cursor
	key    []byte
	valIdx []byte
	err    error
}

func (it *iterator) seek(key []byte) {
	if _, _, errsr := it.cursor.Get(key, nil, lmdb.SetRange); errsr != nil {
		if operr, ok := errsr.(*lmdb.OpError); !ok || operr.Errno != lmdb.NotFound {
			// in this case it's really an error
			panic(operr)
		} else {
			// we're at the end and we just want notes before this,
			// so we just need to set the cursor the last key, this is not a real error
			it.key, it.valIdx, it.err = it.cursor.Get(nil, nil, lmdb.Last)
		}
	} else {
		// move one back as the first step
		it.key, it.valIdx, it.err = it.cursor.Get(nil, nil, lmdb.Prev)
	}
}

func (it *iterator) next() {
	// move one back (we'll look into k and v and err in the next iteration)
	it.key, it.valIdx, it.err = it.cursor.Get(nil, nil, lmdb.Prev)
}

type key struct {
	dbi lmdb.DBI
	key []byte
}

func (b *LMDBBackend) keyName(key key) string {
	return fmt.Sprintf("<dbi=%s key=%x>", b.dbiName(key.dbi), key.key)
}

func (b *LMDBBackend) getIndexKeysForEvent(evt *nostr.Event) iter.Seq[key] {
	return func(yield func(key) bool) {
		{
			// ~ by id
			k := make([]byte, 8)
			hex.Decode(k[0:8], []byte(evt.ID[0:8*2]))
			if !yield(key{dbi: b.indexId, key: k[0:8]}) {
				return
			}
		}

		{
			// ~ by pubkey+date
			k := make([]byte, 8+4)
			hex.Decode(k[0:8], []byte(evt.PubKey[0:8*2]))
			binary.BigEndian.PutUint32(k[8:8+4], uint32(evt.CreatedAt))
			if !yield(key{dbi: b.indexPubkey, key: k[0 : 8+4]}) {
				return
			}
		}

		{
			// ~ by kind+date
			k := make([]byte, 2+4)
			binary.BigEndian.PutUint16(k[0:2], uint16(evt.Kind))
			binary.BigEndian.PutUint32(k[2:2+4], uint32(evt.CreatedAt))
			if !yield(key{dbi: b.indexKind, key: k[0 : 2+4]}) {
				return
			}
		}

		{
			// ~ by pubkey+kind+date
			k := make([]byte, 8+2+4)
			hex.Decode(k[0:8], []byte(evt.PubKey[0:8*2]))
			binary.BigEndian.PutUint16(k[8:8+2], uint16(evt.Kind))
			binary.BigEndian.PutUint32(k[8+2:8+2+4], uint32(evt.CreatedAt))
			if !yield(key{dbi: b.indexPubkeyKind, key: k[0 : 8+2+4]}) {
				return
			}
		}

		// ~ by tagvalue+date
		// ~ by p-tag+kind+date
		for i, tag := range evt.Tags {
			if len(tag) < 2 || len(tag[0]) != 1 || len(tag[1]) == 0 || len(tag[1]) > 100 {
				// not indexable
				continue
			}
			firstIndex := slices.IndexFunc(evt.Tags, func(t nostr.Tag) bool {
				return len(t) >= 2 && t[0] == tag[0] && t[1] == tag[1]
			})
			if firstIndex != i {
				// duplicate
				continue
			}

			// get key prefix (with full length) and offset where to write the created_at
			dbi, k, offset := b.getTagIndexPrefix(tag[0], tag[1])
			binary.BigEndian.PutUint32(k[offset:], uint32(evt.CreatedAt))
			if !yield(key{dbi: dbi, key: k}) {
				return
			}

			// now the p-tag+kind+date
			if dbi == b.indexTag32 && tag[0] == "p" {
				k := make([]byte, 8+2+4)
				hex.Decode(k[0:8], []byte(tag[1][0:8*2]))
				binary.BigEndian.PutUint16(k[8:8+2], uint16(evt.Kind))
				binary.BigEndian.PutUint32(k[8+2:8+2+4], uint32(evt.CreatedAt))
				dbi := b.indexPTagKind
				if !yield(key{dbi: dbi, key: k[0 : 8+2+4]}) {
					return
				}
			}
		}

		{
			// ~ by date only
			k := make([]byte, 4)
			binary.BigEndian.PutUint32(k[0:4], uint32(evt.CreatedAt))
			if !yield(key{dbi: b.indexCreatedAt, key: k[0:4]}) {
				return
			}
		}
	}
}

func (b *LMDBBackend) getTagIndexPrefix(tagName string, tagValue string) (lmdb.DBI, []byte, int) {
	var k []byte   // the key with full length for created_at and idx at the end, but not filled with these
	var offset int // the offset -- i.e. where the prefix ends and the created_at and idx would start
	var dbi lmdb.DBI

	letterPrefix := byte(int(tagName[0]) % 256)

	// if it's 32 bytes as hex, save it as bytes
	if len(tagValue) == 64 {
		// but we actually only use the first 8 bytes, with letter (tag name) prefix
		k = make([]byte, 1+8+4)
		if _, err := hex.Decode(k[1:1+8], []byte(tagValue[0:8*2])); err == nil {
			k[0] = letterPrefix
			offset = 1 + 8
			dbi = b.indexTag32
			return dbi, k[0 : 1+8+4], offset
		}
	}

	// if it looks like an "a" tag, index it in this special format, with letter (tag name) prefix
	spl := strings.Split(tagValue, ":")
	if len(spl) == 3 && len(spl[1]) == 64 {
		k = make([]byte, 1+2+8+30+4)
		if _, err := hex.Decode(k[1+2:1+2+8], []byte(tagValue[0:8*2])); err == nil {
			if kind, err := strconv.ParseUint(spl[0], 10, 16); err == nil {
				k[0] = byte(letterPrefix)
				k[1] = byte(kind >> 8)
				k[2] = byte(kind)
				// limit "d" identifier to 30 bytes (so we don't have to grow our byte slice)
				n := copy(k[1+2+8:1+2+8+30], spl[2])
				offset = 1 + 2 + 8 + n
				dbi = b.indexTagAddr
				return dbi, k[0 : offset+4], offset
			}
		}
	}

	// index whatever else as a md5 hash of the contents, with letter (tag name) prefix
	h := md5.New()
	h.Write([]byte(tagValue))
	k = make([]byte, 1, 1+16+4)
	k[0] = letterPrefix
	k = h.Sum(k)
	offset = 1 + 16
	dbi = b.indexTag

	return dbi, k[0 : 1+16+4], offset
}

func (b *LMDBBackend) dbiName(dbi lmdb.DBI) string {
	switch dbi {
	case b.hllCache:
		return "hllCache"
	case b.settingsStore:
		return "settingsStore"
	case b.rawEventStore:
		return "rawEventStore"
	case b.indexCreatedAt:
		return "indexCreatedAt"
	case b.indexId:
		return "indexId"
	case b.indexKind:
		return "indexKind"
	case b.indexPubkey:
		return "indexPubkey"
	case b.indexPubkeyKind:
		return "indexPubkeyKind"
	case b.indexTag:
		return "indexTag"
	case b.indexTag32:
		return "indexTag32"
	case b.indexTagAddr:
		return "indexTagAddr"
	case b.indexPTagKind:
		return "indexPTagKind"
	default:
		return "<unexpected>"
	}
}
