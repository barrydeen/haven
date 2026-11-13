package postgresql

import (
	"sync"

	"github.com/jmoiron/sqlx"
)

type PostgresBackend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL              string
	QueryLimit               int
	QueryIDsLimit            int
	QueryAuthorsLimit        int
	QueryKindsLimit          int
	QueryTagsLimit           int
	KeepRecentEvents         bool
	FullTextSearchConfig     string // text search configuration for to_tsvector/to_tsquery, defaults to "simple"
	FullTextSearchMaxLength  int    // maximum content length for full-text search, 0 means no limit
	FullTextSearchColumn     string // column to search in, defaults to "content"
}

func (b *PostgresBackend) Close() {
	b.DB.Close()
}
