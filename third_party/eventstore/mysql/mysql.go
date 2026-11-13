package mysql

import (
	"sync"

	"github.com/jmoiron/sqlx"
)

type MySQLBackend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (b *MySQLBackend) Close() {
	b.DB.Close()
}
