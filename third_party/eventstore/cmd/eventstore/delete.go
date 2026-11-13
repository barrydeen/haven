package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
	"github.com/nbd-wtf/go-nostr"
)

var delete_ = &cli.Command{
	Name:        "delete",
	ArgsUsage:   "[<id>]",
	Usage:       "deletes an event by id and all its associated index entries",
	Description: "takes an id either as an argument or reads a stream of ids from stdin and deletes them from the currently open eventstore.",
	Action: func(ctx context.Context, c *cli.Command) error {
		hasError := false
		for line := range getStdinLinesOrFirstArgument(c) {
			f := nostr.Filter{IDs: []string{line}}
			ch, err := db.QueryEvents(ctx, f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error querying for %s: %s\n", f, err)
				hasError = true
			}
			for evt := range ch {
				if err := db.DeleteEvent(ctx, evt); err != nil {
					fmt.Fprintf(os.Stderr, "error deleting %s: %s\n", evt, err)
					hasError = true
				}
			}
		}

		if hasError {
			os.Exit(123)
		}
		return nil
	},
}
