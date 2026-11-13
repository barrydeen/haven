package mongo

import (
	"context"
	"log"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	queryLimit        = 100
	queryIDsLimit     = 500
	queryAuthorsLimit = 500
	queryKindsLimit   = 10
	queryTagsLimit    = 100
)

var indexs = []mongo.IndexModel{
	{
		Keys:    bson.M{"id": 1},
		Options: options.Index().SetUnique(true),
	},
	{
		Keys:    bson.M{"pubkey": 1},
		Options: options.Index(),
	},
	{
		Keys:    bson.M{"createdat": -1},
		Options: options.Index(),
	},
	{
		Keys:    bson.M{"kind": 1},
		Options: options.Index(),
	},
	{
		Keys:    bson.D{{Key: "kind", Value: 1}, {Key: "createdat", Value: -1}},
		Options: options.Index(),
	},
}

func (m *MongoDBBackend) Init() error {
	m.ctx = context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(m.DatabaseURL))
	if err != nil {
		return err
	}
	collection := client.Database("events").Collection("events")
	for _, index := range indexs {
		_, err = collection.Indexes().CreateOne(context.TODO(), index)
		if err != nil {
			log.Fatal(err)
		}
	}
	m.Client = client
	if m.QueryAuthorsLimit == 0 {
		m.QueryAuthorsLimit = queryAuthorsLimit
	}
	if m.QueryLimit == 0 {
		m.QueryLimit = queryLimit
	}
	if m.QueryIDsLimit == 0 {
		m.QueryIDsLimit = queryIDsLimit
	}
	if m.QueryKindsLimit == 0 {
		m.QueryKindsLimit = queryKindsLimit
	}
	if m.QueryTagsLimit == 0 {
		m.QueryTagsLimit = queryTagsLimit
	}
	return nil
}
