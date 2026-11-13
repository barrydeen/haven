package eventstore

import "context"

var negentropySessionKey = struct{}{}

func IsNegentropySession(ctx context.Context) bool {
	return ctx.Value(negentropySessionKey) != nil
}

func SetNegentropy(ctx context.Context) context.Context {
	return context.WithValue(ctx, negentropySessionKey, struct{}{})
}
