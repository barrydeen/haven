package strfry

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fiatjaf/eventstore"
	"github.com/mailru/easyjson"
	"github.com/nbd-wtf/go-nostr"
)

var _ eventstore.Store = (*StrfryBackend)(nil)

type StrfryBackend struct {
	ConfigPath     string
	ExecutablePath string
}

func (s *StrfryBackend) Init() error {
	if s.ExecutablePath == "" {
		configPath := filepath.Dir(s.ConfigPath)
		os.Setenv("PATH", configPath+":"+os.Getenv("PATH"))
		exe, err := exec.LookPath("strfry")
		if err != nil {
			return fmt.Errorf("failed to find strfry executable: %w (better provide it manually)", err)
		}
		s.ExecutablePath = exe
	}

	return nil
}

func (_ StrfryBackend) Close() {}

func (s StrfryBackend) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	stdout, err := s.baseStrfryScan(ctx, filter)
	if err != nil {
		return nil, err
	}

	ch := make(chan *nostr.Event)
	go func() {
		defer close(ch)
		for {
			line, err := stdout.ReadBytes('\n')
			if err != nil {
				break
			}

			evt := &nostr.Event{}
			easyjson.Unmarshal(line, evt)
			if evt.ID == "" {
				continue
			}

			ch <- evt
		}
	}()

	return ch, nil
}

func (s *StrfryBackend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	return s.SaveEvent(ctx, evt)
}

func (s StrfryBackend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	args := make([]string, 0, 4)
	if s.ConfigPath != "" {
		args = append(args, "--config="+s.ConfigPath)
	}
	args = append(args, "import")
	args = append(args, "--show-rejected")
	args = append(args, "--no-verify")

	cmd := exec.CommandContext(ctx, s.ExecutablePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// event is sent on stdin
	j, _ := easyjson.Marshal(evt)
	cmd.Stdin = bytes.NewBuffer(j)

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf(
			"%s %s failed: %w, (%s)",
			s.ExecutablePath, strings.Join(args, " "), err, stderr.String(),
		)
	}

	return nil
}

func (s StrfryBackend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	args := make([]string, 0, 3)
	if s.ConfigPath != "" {
		args = append(args, "--config="+s.ConfigPath)
	}
	args = append(args, "delete")
	args = append(args, "--filter={\"ids\":[\""+evt.ID+"\"]}")

	cmd := exec.CommandContext(ctx, s.ExecutablePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf(
			"%s %s failed: %w, (%s)",
			s.ExecutablePath, strings.Join(args, " "), err, stderr.String(),
		)
	}

	return nil
}

func (s StrfryBackend) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	stdout, err := s.baseStrfryScan(ctx, filter)
	if err != nil {
		return 0, err
	}

	var count int64
	for {
		_, err := stdout.ReadBytes('\n')
		if err != nil {
			break
		}
		count++
	}

	return count, nil
}

func (s StrfryBackend) baseStrfryScan(ctx context.Context, filter nostr.Filter) (*bytes.Buffer, error) {
	args := make([]string, 0, 3)
	if s.ConfigPath != "" {
		args = append(args, "--config="+s.ConfigPath)
	}
	args = append(args, "scan")
	args = append(args, filter.String())

	cmd := exec.CommandContext(ctx, s.ExecutablePath, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf(
			"%s %s failed: %w, (%s)",
			s.ExecutablePath, strings.Join(args, " "), err, stderr.String(),
		)
	}

	return &stdout, nil
}
