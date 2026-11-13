package postgresql

import (
	"context"
	"encoding/json"

	"github.com/fiatjaf/eventstore"
	"github.com/nbd-wtf/go-nostr"
)

func (b *PostgresBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	sql, params, _ := saveEventSql(evt)
	res, err := b.DB.ExecContext(ctx, sql, params...)
	if err != nil {
		return err
	}

	nr, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if nr == 0 {
		return eventstore.ErrDupEvent
	}

	return nil
}

func (b *PostgresBackend) BeforeSave(ctx context.Context, evt *nostr.Event) {
	// do nothing
}

func (b *PostgresBackend) AfterSave(evt *nostr.Event) {
	if b.KeepRecentEvents {
		return
	}
	// delete all but the 100 most recent ones for each key
	b.DB.Exec(`DELETE FROM event WHERE pubkey = $1 AND kind = $2 AND created_at < (
      SELECT created_at FROM event WHERE pubkey = $1
      ORDER BY created_at DESC, id OFFSET 100 LIMIT 1
    )`, evt.PubKey, evt.Kind)
}

func saveEventSql(evt *nostr.Event) (string, []any, error) {
	const query = `INSERT INTO event (
	id, pubkey, created_at, kind, tags, content, sig)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (id) DO NOTHING`

	var (
		tagsj, _ = json.Marshal(evt.Tags)
		params   = []any{evt.ID, evt.PubKey, evt.CreatedAt, evt.Kind, tagsj, evt.Content, evt.Sig}
	)

	return query, params, nil
}
