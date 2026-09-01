package productserver

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"strings"
)

var (
	ErrInvalidEventStream = errors.New("product server event stream is invalid")
	ErrEventTooLarge      = errors.New("product server event is too large")
)

const (
	defaultMaxEventLineBytes = 64 << 10
	defaultMaxEventBytes     = 1 << 20
)

type Event struct {
	ID   string
	Type string
	Data string
}

// EventOptions describes one live stream. Reconnect checkpoints and replay
// journals are gone.
type EventOptions struct {
	Path          string
	Header        http.Header
	MaxLineBytes  int64
	MaxEventBytes int64
}

func (client *Client) Subscribe(ctx context.Context, options EventOptions, handle func(Event) error) error {
	if client == nil || ctx == nil || handle == nil {
		return ErrInvalidEventStream
	}
	if options.Path == "" {
		options.Path = "/"
	}
	if options.MaxLineBytes == 0 {
		options.MaxLineBytes = defaultMaxEventLineBytes
	}
	if options.MaxEventBytes == 0 {
		options.MaxEventBytes = defaultMaxEventBytes
	}
	if options.MaxLineBytes < 1 || options.MaxEventBytes < 1 {
		return ErrInvalidLimits
	}
	response, err := client.doRaw(ctx, Request{Method: http.MethodGet, Path: options.Path, Header: options.Header})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrInvalidEventStream
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), int(options.MaxLineBytes))
	event := Event{}
	data := make([]string, 0, 1)
	size := int64(0)
	dispatch := func() error {
		if len(data) == 0 {
			event = Event{}
			size = 0
			return nil
		}
		event.Data = strings.Join(data, "\n")
		if err := handle(event); err != nil {
			return err
		}
		event = Event{}
		data = data[:0]
		size = 0
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		size += int64(len(value))
		if size > options.MaxEventBytes {
			return ErrEventTooLarge
		}
		switch name {
		case "id":
			event.ID = value
		case "event":
			event.Type = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return ErrInvalidEventStream
	}
	return dispatch()
}
