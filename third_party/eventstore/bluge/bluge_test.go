package bluge

import (
	"context"
	"os"
	"testing"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/nbd-wtf/go-nostr"
	"github.com/stretchr/testify/assert"
)

func TestBlugeFlow(t *testing.T) {
	os.RemoveAll("/tmp/blugetest-badger")
	os.RemoveAll("/tmp/blugetest-bluge")

	bb := &badger.BadgerBackend{Path: "/tmp/blugetest-badger"}
	bb.Init()
	defer bb.Close()

	bl := BlugeBackend{
		Path:          "/tmp/blugetest-bluge",
		RawEventStore: bb,
	}
	bl.Init()
	defer bl.Close()

	ctx := context.Background()

	willDelete := make([]*nostr.Event, 0, 3)

	for i, content := range []string{
		"good morning mr paper maker",
		"good night",
		"I'll see you again in the paper house",
		"tonight we dine in my house",
		"the paper in this house if very good, mr",
	} {
		evt := &nostr.Event{Content: content, Tags: nostr.Tags{}}
		evt.Sign("0000000000000000000000000000000000000000000000000000000000000001")

		bb.SaveEvent(ctx, evt)
		bl.SaveEvent(ctx, evt)

		if i%2 == 0 {
			willDelete = append(willDelete, evt)
		}
	}

	{
		ch, err := bl.QueryEvents(ctx, nostr.Filter{Search: "good"})
		if err != nil {
			t.Fatalf("QueryEvents error: %s", err)
			return
		}
		n := 0
		for range ch {
			n++
		}
		assert.Equal(t, 3, n)
	}

	for _, evt := range willDelete {
		bl.DeleteEvent(ctx, evt)
	}

	{
		ch, err := bl.QueryEvents(ctx, nostr.Filter{Search: "good"})
		if err != nil {
			t.Fatalf("QueryEvents error: %s", err)
			return
		}
		n := 0
		for res := range ch {
			n++
			assert.Equal(t, res.Content, "good night")
			assert.Equal(t, res.PubKey, "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
		}
		assert.Equal(t, 1, n)
	}
}
