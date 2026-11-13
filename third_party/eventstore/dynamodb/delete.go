package dynamodb

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/nbd-wtf/go-nostr"
)

func (d *DynamoDBBackend) DeleteEvent(ctx context.Context, event *nostr.Event) error {
	println(event.ID)
	_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String("events"),
		Key: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{
				Value: event.ID,
			},
		},
	})
	return err
}
