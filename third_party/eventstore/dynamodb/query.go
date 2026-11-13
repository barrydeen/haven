package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/nbd-wtf/go-nostr"
)

var (
	ErrTooManyIDs       = errors.New("too many ids")
	ErrTooManyAuthors   = errors.New("too many authors")
	ErrTooManyKinds     = errors.New("too many kinds")
	ErrEmptyTagSet      = errors.New("empty tag set")
	ErrTooManyTagValues = errors.New("too many tag values")
)

func buildBuilder(filter nostr.Filter) expression.Builder {
	builder := expression.NewBuilder()
	if len(filter.IDs) > 0 {
		var operands []expression.OperandBuilder
		for _, v := range filter.IDs {
			operands = append(operands, expression.Value(v))
		}
		if len(operands) > 0 {
			builder = builder.WithFilter(expression.Name("id").In(operands[0], operands[1:]...))
		}
	}
	if len(filter.Authors) > 0 {
		var operands []expression.OperandBuilder
		for _, v := range filter.Authors {
			operands = append(operands, expression.Value(v))
		}
		if len(operands) > 0 {
			builder = builder.WithFilter(expression.Name("pubkey").In(operands[0], operands[1:]...))
		}
	}
	if len(filter.Kinds) > 0 {
		var operands []expression.OperandBuilder
		for _, v := range filter.Kinds {
			operands = append(operands, expression.Value(v))
		}
		if len(operands) > 0 {
			builder = builder.WithFilter(expression.Name("kind").In(operands[0], operands[1:]...))
		}
	}
	if filter.Since != nil {
		builder = builder.WithFilter(expression.Name("created_at").GreaterThanEqual(expression.Value(*filter.Since)))
	}
	if filter.Until != nil {
		builder = builder.WithFilter(expression.Name("created_at").LessThan(expression.Value(*filter.Until)))
	}
	if len(filter.Tags) > 0 {
		tfilt := expression.ConditionBuilder{}
		for k, tag := range filter.Tags {
			b, err := json.Marshal(append([]string{k}, tag...))
			if err != nil {
				continue
			}

			if tfilt.IsSet() {
				tfilt = expression.Or(tfilt, expression.Name("tags").Contains(string(b)))
			} else {
				tfilt = expression.Name("tags").Contains(string(b))
			}
		}
		builder = builder.WithFilter(tfilt)
	}
	if filter.Search != "" {
		builder = builder.WithFilter(expression.Name("content").Contains(filter.Search))
	}

	return builder
}

func (d *DynamoDBBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	limit := filter.Limit
	if filter.Limit < 1 || filter.Limit > d.QueryLimit {
		limit = d.QueryLimit
	}
	ch := make(chan *nostr.Event)
	go func() {
		defer close(ch)

		expr, err := buildBuilder(filter).Build()
		if err != nil {
			log.Println(err)
			return
		}
		resp, err := d.Client.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String("events"),
			Limit:                     aws.Int32(int32(limit)),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			FilterExpression:          expr.Filter(),
			ProjectionExpression:      expr.Projection(),
		})
		if err != nil {
			log.Println(err)
			return
		}

		for _, item := range resp.Items {
			var evt nostr.Event

			for k, v := range item {
				switch k {
				case "id":
					evt.ID = v.(*types.AttributeValueMemberS).Value
				case "pubkey":
					evt.PubKey = v.(*types.AttributeValueMemberS).Value
				case "kind":
					if n, err := strconv.Atoi(v.(*types.AttributeValueMemberN).Value); err == nil {
						evt.Kind = int(n)
					}
				case "created_at":
					if n, err := strconv.Atoi(v.(*types.AttributeValueMemberN).Value); err == nil {
						evt.CreatedAt = nostr.Timestamp(n)
					}
				case "tags":
					err = json.Unmarshal([]byte(v.(*types.AttributeValueMemberS).Value), &evt.Tags)
					if err != nil {
						return
					}
				case "content":
					evt.Content = v.(*types.AttributeValueMemberS).Value
				case "sig":
					evt.Sig = v.(*types.AttributeValueMemberS).Value
				}
			}
			select {
			case ch <- &evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (d *DynamoDBBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	expr, err := buildBuilder(filter).Build()
	if err != nil {
		return 0, err
	}
	resp, err := d.Client.Scan(ctx, &dynamodb.ScanInput{
		TableName:                 aws.String("events"),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		FilterExpression:          expr.Filter(),
		ProjectionExpression:      expr.Projection(),
		Select:                    types.SelectCount,
	})
	if err != nil {
		return 0, err
	}
	return int64(resp.Count), nil
}
