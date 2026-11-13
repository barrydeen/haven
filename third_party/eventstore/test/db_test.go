package test

import (
	"context"
	"os"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/eventstore/slicestore"
	"github.com/fiatjaf/eventstore/sqlite3"
)

const (
	dbpath = "/tmp/eventstore-test"
	sk3    = "0000000000000000000000000000000000000000000000000000000000000003"
	sk4    = "0000000000000000000000000000000000000000000000000000000000000004"
)

var ctx = context.Background()

var tests = []struct {
	name string
	run  func(*testing.T, eventstore.Store)
}{
	{"first", runFirstTestOn},
	{"second", runSecondTestOn},
	{"manyauthors", manyAuthorsTest},
	{"unbalanced", unbalancedTest},
}

func TestSliceStore(t *testing.T) {
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.run(t, &slicestore.SliceStore{}) })
	}
}

func TestLMDB(t *testing.T) {
	for _, test := range tests {
		os.RemoveAll(dbpath + "lmdb")
		t.Run(test.name, func(t *testing.T) { test.run(t, &lmdb.LMDBBackend{Path: dbpath + "lmdb"}) })
	}
}

func TestBadger(t *testing.T) {
	for _, test := range tests {
		os.RemoveAll(dbpath + "badger")
		t.Run(test.name, func(t *testing.T) { test.run(t, &badger.BadgerBackend{Path: dbpath + "badger"}) })
	}
}

func TestSQLite(t *testing.T) {
	for _, test := range tests {
		os.RemoveAll(dbpath + "sqlite")
		t.Run(test.name, func(t *testing.T) {
			test.run(t, &sqlite3.SQLite3Backend{DatabaseURL: dbpath + "sqlite", QueryLimit: 1000, QueryTagsLimit: 50, QueryAuthorsLimit: 2000})
		})
	}
}

func TestPostgres(t *testing.T) {
	for _, test := range tests {
		postgres := embeddedpostgres.NewDatabase()
		err := postgres.Start()
		if err != nil {
			t.Fatalf("failed to start embedded postgres: %s", err)
			return
		}
		t.Run(test.name, func(t *testing.T) {
			test.run(t, &postgresql.PostgresBackend{DatabaseURL: "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable", QueryLimit: 1000, QueryTagsLimit: 50, QueryAuthorsLimit: 2000})
		})
		postgres.Stop()
	}
}
