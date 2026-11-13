package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
	"github.com/mailru/easyjson"
	"github.com/nbd-wtf/go-nostr"
)

var save = &cli.Command{
	Name:        "save",
	ArgsUsage:   "[<event-json>]",
	Usage:       "stores an event",
	Description: "takes either an event as an argument or reads a stream of events from stdin and inserts those in the currently opened eventstore.\ndoesn't perform any kind of signature checking or replacement.",
	Action: func(ctx context.Context, c *cli.Command) error {
		hasError := false
		for line := range getStdinLinesOrFirstArgument(c) {
			var event nostr.Event
			if err := easyjson.Unmarshal([]byte(line), &event); err != nil {
				fmt.Fprintf(os.Stderr, "invalid event '%s': %s\n", line, err)
				hasError = true
				continue
			}

			if err := db.SaveEvent(ctx, &event); err != nil {
				fmt.Fprintf(os.Stderr, "failed to save event '%s': %s\n", line, err)
				hasError = true
				continue
			}

			fmt.Fprintf(os.Stderr, "saved %s\n", event.ID)
		}

		if hasError {
			os.Exit(123)
		}
		return nil
	},
}
