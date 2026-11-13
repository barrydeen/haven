package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
)

const (
	LINE_PROCESSING_ERROR = iota
)

func detect(dir string) (string, error) {
	f, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !f.IsDir() {
		f, err := os.Open(dir)
		if err != nil {
			return "", err
		}
		buf := make([]byte, 15)
		f.Read(buf)
		if string(buf) == "SQLite format 3" {
			return "sqlite", nil
		}

		return "", fmt.Errorf("unknown db format")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".mdb") {
			return "lmdb", nil
		}
		if strings.HasSuffix(entry.Name(), ".vlog") {
			return "badger", nil
		}
	}

	return "", fmt.Errorf("undetected")
}

func getStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		read := bytes.NewBuffer(make([]byte, 0, 1000))
		_, err := io.Copy(read, os.Stdin)
		if err == nil {
			return read.String()
		}
	}
	return ""
}

func isPiped() bool {
	stat, _ := os.Stdin.Stat()
	return stat.Mode()&os.ModeCharDevice == 0
}

func getStdinLinesOrFirstArgument(c *cli.Command) chan string {
	// try the first argument
	target := c.Args().First()
	if target != "" {
		single := make(chan string, 1)
		single <- target
		close(single)
		return single
	}

	// try the stdin
	multi := make(chan string)
	writeStdinLinesOrNothing(multi)
	return multi
}

func getStdinLinesOrBlank() chan string {
	multi := make(chan string)
	if hasStdinLines := writeStdinLinesOrNothing(multi); !hasStdinLines {
		single := make(chan string, 1)
		single <- ""
		close(single)
		return single
	} else {
		return multi
	}
}

func writeStdinLinesOrNothing(ch chan string) (hasStdinLines bool) {
	if isPiped() {
		// piped
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				ch <- strings.TrimSpace(scanner.Text())
			}
			close(ch)
		}()
		return true
	} else {
		// not piped
		return false
	}
}
