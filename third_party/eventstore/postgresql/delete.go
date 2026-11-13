package postgresql

import (
	"context"

	"github.com/nbd-wtf/go-nostr"
)

func (b *PostgresBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	_, err := b.DB.ExecContext(ctx, "DELETE FROM event WHERE id = $1", evt.ID)
	return err
}
