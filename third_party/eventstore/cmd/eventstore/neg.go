package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/urfave/cli/v3"
	"github.com/mailru/easyjson"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip77/negentropy"
	"github.com/nbd-wtf/go-nostr/nip77/negentropy/storage/vector"
)

var neg = &cli.Command{
	Name:        "neg",
	ArgsUsage:   "<filter-json> [<negentropy-message-hex>]",
	Usage:       "initiates a negentropy session with a filter or reconciles a received negentropy message",
	Description: "applies the filter to the currently open eventstore. if no negentropy message was given it will initiate the process and emit one, if one was given either as an argument or via stdin, it will be reconciled against the current eventstore.\nthe next reconciliation message will be emitted on stdout.\na stream of need/have ids (or nothing) will be emitted to stderr.",
	Flags: []cli.Flag{
		&cli.UintFlag{
			Name: "frame-size-limit",
		},
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		jfilter := c.Args().First()
		if jfilter == "" {
			return fmt.Errorf("missing filter argument")
		}

		filter := nostr.Filter{}
		if err := easyjson.Unmarshal([]byte(jfilter), &filter); err != nil {
			return fmt.Errorf("invalid filter %s: %s\n", jfilter, err)
		}

		frameSizeLimit := int(c.Uint("frame-size-limit"))
		if frameSizeLimit == 0 {
			frameSizeLimit = math.MaxInt
		}

		// create negentropy object and initialize it with events
		vec := vector.New()
		neg := negentropy.New(vec, frameSizeLimit)
		ch, err := db.QueryEvents(ctx, filter)
		if err != nil {
			return fmt.Errorf("error querying: %s\n", err)
		}
		for evt := range ch {
			vec.Insert(evt.CreatedAt, evt.ID)
		}

		wg := sync.WaitGroup{}
		go func() {
			defer wg.Done()
			for item := range neg.Haves {
				fmt.Fprintf(os.Stderr, "have %s", item)
			}
		}()
		go func() {
			defer wg.Done()
			for item := range neg.HaveNots {
				fmt.Fprintf(os.Stderr, "need %s", item)
			}
		}()

		// get negentropy message from argument or stdin pipe
		var msg string
		if isPiped() {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("failed to read from stdin: %w", err)
			}
			msg = string(data)
		} else {
			msg = c.Args().Get(1)
		}

		if msg == "" {
			// initiate the process
			out := neg.Start()
			fmt.Println(out)
		} else {
			// process the message
			out, err := neg.Reconcile(msg)
			if err != nil {
				return fmt.Errorf("negentropy failed: %s", err)
			}
			fmt.Println(out)
		}

		wg.Wait()
		return nil
	},
}
