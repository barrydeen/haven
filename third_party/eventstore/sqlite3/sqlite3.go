package sqlite3

import (
	"sync"

	"github.com/jmoiron/sqlx"
)

type SQLite3Backend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (b *SQLite3Backend) Close() {
	b.DB.Close()
}
