package mysql

import (
	"context"

	"github.com/nbd-wtf/go-nostr"
)

func (b MySQLBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	_, err := b.DB.ExecContext(ctx, "DELETE FROM event WHERE id = ?", evt.ID)
	return err
}
