package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
	"github.com/nbd-wtf/go-nostr"
)

// this is the default command when no subcommands are given, we will just try everything
var queryOrSave = &cli.Command{
	Hidden: true,
	Name:   "query-or-save",
	Action: func(ctx context.Context, c *cli.Command) error {
		line := getStdin()

		ee := &nostr.EventEnvelope{}
		re := &nostr.ReqEnvelope{}
		e := &nostr.Event{}
		f := &nostr.Filter{}
		if json.Unmarshal([]byte(line), ee) == nil && ee.Event.ID != "" {
			e = &ee.Event
			return doSave(ctx, line, e)
		}
		if json.Unmarshal([]byte(line), e) == nil && e.ID != "" {
			return doSave(ctx, line, e)
		}
		if json.Unmarshal([]byte(line), re) == nil && len(re.Filters) > 0 {
			f = &re.Filters[0]
			return doQuery(ctx, f)
		}
		if json.Unmarshal([]byte(line), f) == nil && len(f.String()) > 2 {
			return doQuery(ctx, f)
		}

		return fmt.Errorf("couldn't parse input '%s'", line)
	},
}

func doSave(ctx context.Context, line string, e *nostr.Event) error {
	if err := db.SaveEvent(ctx, e); err != nil {
		return fmt.Errorf("failed to save event '%s': %s", line, err)
	}
	fmt.Fprintf(os.Stderr, "saved %s", e.ID)
	return nil
}

func doQuery(ctx context.Context, f *nostr.Filter) error {
	ch, err := db.QueryEvents(ctx, *f)
	if err != nil {
		return fmt.Errorf("error querying: %w", err)
	}

	for evt := range ch {
		fmt.Println(evt)
	}
	return nil
}
