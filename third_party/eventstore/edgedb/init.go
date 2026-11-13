package edgedb

import (
	"context"
	"errors"

	"github.com/edgedb/edgedb-go"
	"github.com/fiatjaf/eventstore"
)

var _ eventstore.Store = (*EdgeDBBackend)(nil)

const (
	queryLimit        = 100
	queryIDsLimit     = 500
	queryAuthorsLimit = 500
	queryKindsLimit   = 10
	queryTagsLimit    = 100
)

var (
	initialMigration = `CREATE MIGRATION {
  CREATE MODULE events IF NOT EXISTS;
  CREATE TYPE events::Event {
      CREATE PROPERTY content: std::str;
      CREATE REQUIRED PROPERTY createdAt: std::datetime;
      CREATE REQUIRED PROPERTY eventId: std::str {
          CREATE CONSTRAINT std::exclusive;
      };
      CREATE REQUIRED PROPERTY kind: std::int64;
      CREATE REQUIRED PROPERTY pubkey: std::str;
      CREATE REQUIRED PROPERTY sig: std::str {
          CREATE CONSTRAINT std::exclusive;
      };
      CREATE PROPERTY tags: array<std::json>;
  };
};`
)

// Init implements the Init method of the eventstore.Store inteface.
// It establishes the connection with Edgedb
func (b *EdgeDBBackend) Init() error {
	opts := edgedb.Options{}
	if b.TLSSkipVerify {
		opts.TLSOptions = edgedb.TLSOptions{SecurityMode: edgedb.TLSModeInsecure}
	}
	dbConn, err := edgedb.CreateClientDSN(context.Background(), b.DatabaseURI, opts)
	if err != nil {
		return err
	}
	// perform initial migration. NOTE: we check for SchemaError since that is the type of error returned when there's a duplicate schema. Kind of dumb
	var dbErr edgedb.Error
	if err := dbConn.Execute(context.Background(), initialMigration); err != nil && errors.As(err, &dbErr) && !dbErr.Category(edgedb.SchemaError) {
		return err
	}
	b.Client = dbConn
	if b.QueryAuthorsLimit == 0 {
		b.QueryAuthorsLimit = queryAuthorsLimit
	}
	if b.QueryLimit == 0 {
		b.QueryLimit = queryLimit
	}
	if b.QueryIDsLimit == 0 {
		b.QueryIDsLimit = queryIDsLimit
	}
	if b.QueryKindsLimit == 0 {
		b.QueryKindsLimit = queryKindsLimit
	}
	if b.QueryTagsLimit == 0 {
		b.QueryTagsLimit = queryTagsLimit
	}
	return nil
}
