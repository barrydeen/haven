package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/dynamodb"
	"github.com/fiatjaf/eventstore/elasticsearch"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/eventstore/mysql"
	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/eventstore/slicestore"
	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/eventstore/strfry"
	"github.com/nbd-wtf/go-nostr"
	"github.com/urfave/cli/v3"
)

var db eventstore.Store

var app = &cli.Command{
	Name:      "eventstore",
	Usage:     "a CLI for all the eventstore backends",
	UsageText: "eventstore -d ./data/sqlite <query|save|delete> ...",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "store",
			Aliases:  []string{"d"},
			Usage:    "path to the database file or directory or database connection uri",
			Required: true,
		},
		&cli.StringFlag{
			Name:    "type",
			Aliases: []string{"t"},
			Usage:   "store type ('sqlite', 'lmdb', 'badger', 'postgres', 'mysql', 'elasticsearch', 'dynamodb')",
		},
	},
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		path := strings.Trim(c.String("store"), "/")
		typ := c.String("type")
		if typ != "" {
			// bypass automatic detection
			// this also works for creating disk databases from scratch
		} else {
			// try to detect based on url scheme
			switch {
			case strings.HasPrefix(path, "postgres://"), strings.HasPrefix(path, "postgresql://"):
				typ = "postgres"
			case strings.HasPrefix(path, "mysql://"):
				typ = "mysql"
			case strings.HasPrefix(path, "https://"):
				// if we ever add something else that uses URLs we'll have to modify this
				typ = "elasticsearch"
			case strings.HasSuffix(path, ".conf"):
				typ = "strfry"
			case strings.HasSuffix(path, ".jsonl"):
				typ = "file"
			default:
				// try to detect based on the form and names of disk files
				dbname, err := detect(path)
				if err != nil {
					if os.IsNotExist(err) {
						return ctx, fmt.Errorf(
							"'%s' does not exist, to create a store there specify the --type argument", path)
					}
					return ctx, fmt.Errorf("failed to detect store type: %w", err)
				}
				typ = dbname
			}
		}

		switch typ {
		case "sqlite":
			db = &sqlite3.SQLite3Backend{
				DatabaseURL:       path,
				QueryLimit:        1_000_000,
				QueryAuthorsLimit: 1_000_000,
				QueryKindsLimit:   1_000_000,
				QueryIDsLimit:     1_000_000,
				QueryTagsLimit:    1_000_000,
			}
		case "lmdb":
			db = &lmdb.LMDBBackend{Path: path, MaxLimit: 1_000_000}
		case "badger":
			db = &badger.BadgerBackend{Path: path, MaxLimit: 1_000_000}
		case "postgres", "postgresql":
			db = &postgresql.PostgresBackend{
				DatabaseURL:       path,
				QueryLimit:        1_000_000,
				QueryAuthorsLimit: 1_000_000,
				QueryKindsLimit:   1_000_000,
				QueryIDsLimit:     1_000_000,
				QueryTagsLimit:    1_000_000,
			}
		case "mysql":
			db = &mysql.MySQLBackend{
				DatabaseURL:       path,
				QueryLimit:        1_000_000,
				QueryAuthorsLimit: 1_000_000,
				QueryKindsLimit:   1_000_000,
				QueryIDsLimit:     1_000_000,
				QueryTagsLimit:    1_000_000,
			}
		case "elasticsearch":
			db = &elasticsearch.ElasticsearchStorage{URL: path}
		case "strfry":
			db = &strfry.StrfryBackend{ConfigPath: path}
		case "dynamodb":
			db = &dynamodb.DynamoDBBackend{
				DatabaseURL: path,
			}
		case "file":
			db = &slicestore.SliceStore{}

			// run this after we've called db.Init()
			defer func() {
				f, err := os.Open(path)
				if err != nil {
					log.Printf("failed to file at '%s': %s\n", path, err)
					os.Exit(3)
				}
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 16*1024*1024), 256*1024*1024)
				i := 0
				for scanner.Scan() {
					var evt nostr.Event
					if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
						log.Printf("invalid event read at line %d: %s (`%s`)\n", i, err, scanner.Text())
					}
					db.SaveEvent(ctx, &evt)
					i++
				}
			}()
		case "":
			return ctx, fmt.Errorf("couldn't determine store type, you can use --type to specify it manually")
		default:
			return ctx, fmt.Errorf("'%s' store type is not supported by this CLI", typ)
		}

		if err := db.Init(); err != nil {
			return ctx, err
		}

		return ctx, nil
	},
	Commands: []*cli.Command{
		queryOrSave,
		query,
		save,
		delete_,
		neg,
	},
	DefaultCommand: "query-or-save",
}

func main() {
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
