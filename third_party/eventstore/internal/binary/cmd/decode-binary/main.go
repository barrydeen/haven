package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/fiatjaf/eventstore/internal/binary"
	"github.com/nbd-wtf/go-nostr"
)

func main() {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read from stdin: %s\n", err)
		os.Exit(1)
		return
	}
	b = bytes.TrimSpace(b)

	if bytes.HasPrefix(b, []byte("0x")) {
		fromHex := make([]byte, (len(b)-2)/2)
		_, err := hex.Decode(fromHex, b[2:])
		if err == nil {
			b = fromHex
		}
	}

	var evt nostr.Event
	err = binary.Unmarshal(b, &evt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to decode: %s\n", err)
		os.Exit(1)
		return
	}
	fmt.Println(evt.String())
}
