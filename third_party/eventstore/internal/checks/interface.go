package checks

import (
	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/bluge"
	"github.com/fiatjaf/eventstore/edgedb"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/eventstore/mongo"
	"github.com/fiatjaf/eventstore/mysql"
	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/eventstore/strfry"
)

// compile-time checks to ensure all backends implement Store
var (
	_ eventstore.Store = (*badger.BadgerBackend)(nil)
	_ eventstore.Store = (*lmdb.LMDBBackend)(nil)
	_ eventstore.Store = (*edgedb.EdgeDBBackend)(nil)
	_ eventstore.Store = (*postgresql.PostgresBackend)(nil)
	_ eventstore.Store = (*mongo.MongoDBBackend)(nil)
	_ eventstore.Store = (*sqlite3.SQLite3Backend)(nil)
	_ eventstore.Store = (*strfry.StrfryBackend)(nil)
	_ eventstore.Store = (*bluge.BlugeBackend)(nil)
	_ eventstore.Store = (*mysql.MySQLBackend)(nil)
)
