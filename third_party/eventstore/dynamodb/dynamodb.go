package dynamodb

import (
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"sync"
)

type DynamoDBBackend struct {
	sync.Mutex
	*dynamodb.Client
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (m *DynamoDBBackend) Close() {
}
