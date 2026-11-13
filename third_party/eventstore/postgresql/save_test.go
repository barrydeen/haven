package postgresql

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/stretchr/testify/assert"
)

func TestSaveEventSql(t *testing.T) {
	now := nostr.Now()
	tests := []struct {
		name   string
		event  *nostr.Event
		query  string
		params []any
		err    error
	}{
		{
			name: "basic",
			event: &nostr.Event{
				ID:        "id",
				PubKey:    "pk",
				CreatedAt: now,
				Kind:      nostr.KindTextNote,
				Content:   "test",
				Sig:       "sig",
			},
			query: `INSERT INTO event (
	id, pubkey, created_at, kind, tags, content, sig)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (id) DO NOTHING`,
			params: []any{"id", "pk", now, nostr.KindTextNote, []byte("null"), "test", "sig"},
			err:    nil,
		},
		{
			name: "tags",
			event: &nostr.Event{
				ID:        "id",
				PubKey:    "pk",
				CreatedAt: now,
				Kind:      nostr.KindTextNote,
				Tags:      nostr.Tags{nostr.Tag{"foo", "bar"}},
				Content:   "test",
				Sig:       "sig",
			},
			query: `INSERT INTO event (
	id, pubkey, created_at, kind, tags, content, sig)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (id) DO NOTHING`,
			params: []any{"id", "pk", now, nostr.KindTextNote, []byte("[[\"foo\",\"bar\"]]"), "test", "sig"},
			err:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, params, err := saveEventSql(tt.event)
			assert.Equal(t, clean(tt.query), clean(query))
			assert.Equal(t, tt.params, params)
			assert.Equal(t, tt.err, err)
		})
	}
}
