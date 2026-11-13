package mongo

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

type MongoDBBackend struct {
	sync.Mutex
	*mongo.Client
	ctx               context.Context
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (m *MongoDBBackend) Close() {
	m.Client.Disconnect(m.ctx)
}
