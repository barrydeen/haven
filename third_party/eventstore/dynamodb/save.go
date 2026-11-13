package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/nbd-wtf/go-nostr"
)

func (d *DynamoDBBackend) SaveEvent(ctx context.Context, event *nostr.Event) error {
	tags, err := json.Marshal(event.Tags)
	if err != nil {
		return err
	}
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("events"),
		Item: map[string]types.AttributeValue{
			"id": &types.AttributeValueMemberS{
				Value: event.ID,
			},
			"pubkey": &types.AttributeValueMemberS{
				Value: event.PubKey,
			},
			"created_at": &types.AttributeValueMemberN{
				Value: fmt.Sprint(event.CreatedAt),
			},
			"kind": &types.AttributeValueMemberN{
				Value: fmt.Sprint(event.Kind),
			},
			"tags": &types.AttributeValueMemberS{
				Value: string(tags),
			},
			"content": &types.AttributeValueMemberS{
				Value: event.Content,
			},
			"sig": &types.AttributeValueMemberS{
				Value: event.Sig,
			},
		},
	})
	if err != nil {
		return err
	}
	return err
}
