package productserver

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidEventStream  = errors.New("product server event stream is invalid")
	ErrEventTooLarge       = errors.New("product server event is too large")
	ErrReconnectLimit      = errors.New("product server event reconnect limit reached")
	ErrEventHeaderConflict = errors.New("product server event header conflicts with reconnect state")
)

const (
	defaultMaxEventLineBytes int64 = 64 << 10
	defaultMaxEventBytes     int64 = 1 << 20
	defaultReconnects              = 8
	defaultReconnectDelay          = 100 * time.Millisecond
	defaultMaxReconnectDelay       = 5 * time.Second
	defaultDedupWindow             = 256
	hardMaxEventLineBytes    int64 = 1 << 20
	hardMaxEventBytes        int64 = 8 << 20
	hardMaxReconnects              = 64
	hardMaxDedupWindow             = 4096
	hardMaxReconnectDelay          = time.Minute
)

// Event is one bounded server-sent event. An empty Type is the SSE default
// event type; typed product packages decide how to interpret it.
type Event struct {
	ID   string
	Type string
	Data string
}

// EventOptions controls mechanical event-stream bounds and reconnection. Zero
// values select defaults. MaxReconnects counts reconnects after the initial
// connection.
type EventOptions struct {
	Path              string
	Header            http.Header
	MaxLineBytes      int64
	MaxEventBytes     int64
	MaxReconnects     int
	ReconnectDelay    time.Duration
	MaxReconnectDelay time.Duration
	DedupWindow       int
}

type normalizedEventOptions struct {
	EventOptions
	delay time.Duration
}

func (options EventOptions) normalized() (normalizedEventOptions, error) {
	if options.Path == "" {
		options.Path = "/"
	}
	if len(headerValuesFold(options.Header, "Last-Event-ID")) != 0 {
		return normalizedEventOptions{}, ErrEventHeaderConflict
	}
	if options.MaxLineBytes < 0 || options.MaxEventBytes < 0 || options.MaxReconnects < 0 || options.ReconnectDelay < 0 || options.MaxReconnectDelay < 0 || options.DedupWindow < 0 {
		return normalizedEventOptions{}, ErrInvalidLimits
	}
	if options.MaxLineBytes == 0 {
		options.MaxLineBytes = defaultMaxEventLineBytes
	}
	if options.MaxEventBytes == 0 {
		options.MaxEventBytes = defaultMaxEventBytes
	}
	if options.MaxReconnects == 0 {
		options.MaxReconnects = defaultReconnects
	}
	if options.ReconnectDelay == 0 {
		options.ReconnectDelay = defaultReconnectDelay
	}
	if options.MaxReconnectDelay == 0 {
		options.MaxReconnectDelay = defaultMaxReconnectDelay
	}
	if options.DedupWindow == 0 {
		options.DedupWindow = defaultDedupWindow
	}
	if options.MaxLineBytes > hardMaxEventLineBytes || options.MaxEventBytes > hardMaxEventBytes ||
		options.MaxReconnects > hardMaxReconnects || options.DedupWindow > hardMaxDedupWindow ||
		options.MaxReconnectDelay > hardMaxReconnectDelay {
		return normalizedEventOptions{}, ErrInvalidLimits
	}
	if headerSize(options.Header) > defaultMaxHeaderBytes {
		return normalizedEventOptions{}, ErrHeadersTooLarge
	}
	initialDelay := clampDuration(options.ReconnectDelay, time.Millisecond, options.MaxReconnectDelay)
	return normalizedEventOptions{EventOptions: options, delay: initialDelay}, nil
}

type retryableEventError struct{ cause error }

func (eventError retryableEventError) Error() string { return eventError.cause.Error() }
func (eventError retryableEventError) Unwrap() error { return eventError.cause }

var errStopEventStream = errors.New("product server event stream stopped")

// Subscribe consumes events synchronously, reconnecting with Last-Event-ID and
// suppressing a bounded window of replayed non-empty IDs. It returns callback
// errors unchanged.
func (client *Client) Subscribe(ctx context.Context, options EventOptions, handle func(Event) error) error {
	if client == nil || handle == nil || ctx == nil {
		return ErrInvalidEventStream
	}
	normalized, err := options.normalized()
	if err != nil {
		return err
	}
	dedup := newEventDeduper(normalized.DedupWindow)
	lastID := ""
	reconnects := 0
	delay := normalized.delay
	for {
		streamLastID, serverDelay, consumeErr := client.consumeEvents(ctx, normalized, lastID, dedup, handle)
		lastID = streamLastID
		if serverDelay > 0 {
			delay = clampDuration(serverDelay, time.Millisecond, normalized.MaxReconnectDelay)
		}
		if consumeErr == nil || errors.Is(consumeErr, errStopEventStream) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var retryable retryableEventError
		if !errors.As(consumeErr, &retryable) {
			return consumeErr
		}
		if reconnects >= normalized.MaxReconnects {
			return ErrReconnectLimit
		}
		reconnects++
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
}

func (client *Client) consumeEvents(
	ctx context.Context,
	options normalizedEventOptions,
	lastID string,
	dedup *eventDeduper,
	handle func(Event) error,
) (string, time.Duration, error) {
	header := options.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Accept", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	if lastID != "" {
		header.Set("Last-Event-ID", lastID)
	}
	response, err := client.doRaw(ctx, Request{Method: http.MethodGet, Path: options.Path, Header: header})
	if err != nil {
		if errors.Is(err, ErrRedirectRefused) || errors.Is(err, ErrAuthConflict) || errors.Is(err, ErrInvalidRequestTarget) ||
			errors.Is(err, ErrHeadersTooLarge) || errors.Is(err, ErrRequestTooLarge) || errors.Is(err, ErrUnsupportedEncoding) {
			return lastID, 0, err
		}
		return lastID, 0, retryableEventError{cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return lastID, 0, errStopEventStream
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode >= 500 {
			return lastID, 0, retryableEventError{cause: ErrInvalidEventStream}
		}
		return lastID, 0, ErrInvalidEventStream
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return lastID, 0, ErrInvalidEventStream
	}
	if charset := parameters["charset"]; charset != "" && !strings.EqualFold(charset, "utf-8") {
		return lastID, 0, ErrInvalidEventStream
	}
	reader, closeReader, err := eventBodyReader(response)
	if err != nil {
		return lastID, 0, err
	}
	if closeReader != nil {
		defer closeReader.Close()
	}
	parsedID, retry, err := parseEventStream(reader, options.MaxLineBytes, options.MaxEventBytes, lastID, dedup, handle)
	if err != nil {
		return parsedID, retry, err
	}
	return parsedID, retry, retryableEventError{cause: io.EOF}
}

func eventBodyReader(response *http.Response) (io.Reader, io.Closer, error) {
	switch normalizedEncoding(response.Header) {
	case "", "identity":
		return response.Body, nil, nil
	case "gzip":
		reader, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, nil, ErrInvalidEventStream
		}
		return reader, reader, nil
	default:
		return nil, nil, ErrUnsupportedEncoding
	}
}

func parseEventStream(
	reader io.Reader,
	maxLine int64,
	maxEvent int64,
	lastID string,
	dedup *eventDeduper,
	handle func(Event) error,
) (string, time.Duration, error) {
	bufferSize := 4096
	if maxLine+1 < int64(bufferSize) {
		bufferSize = int(maxLine + 1)
	}
	buffered := bufio.NewReaderSize(reader, bufferSize)
	var eventType string
	var data strings.Builder
	var blockBytes int64
	var retry time.Duration
	firstLine := true
	dispatch := func() error {
		if data.Len() == 0 {
			eventType = ""
			blockBytes = 0
			return nil
		}
		value := data.String()
		value = strings.TrimSuffix(value, "\n")
		event := Event{ID: lastID, Type: eventType, Data: value}
		data.Reset()
		eventType = ""
		blockBytes = 0
		if event.ID != "" && dedup.Seen(event.ID) {
			return nil
		}
		if event.ID != "" {
			dedup.Add(event.ID)
		}
		return handle(event)
	}
	for {
		line, err := readEventLine(buffered, maxLine)
		if errors.Is(err, ErrEventTooLarge) {
			return lastID, retry, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return lastID, retry, ErrInvalidEventStream
		}
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if !utf8.ValidString(line) {
			return lastID, retry, ErrInvalidEventStream
		}
		if line == "" {
			if dispatchErr := dispatch(); dispatchErr != nil {
				return lastID, retry, dispatchErr
			}
		} else {
			blockBytes += int64(len(line) + 1)
			if blockBytes > maxEvent {
				return lastID, retry, ErrEventTooLarge
			}
		}
		if line != "" && line[0] != ':' {
			field, value, found := strings.Cut(line, ":")
			if !found {
				value = ""
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data.WriteString(value)
				data.WriteByte('\n')
			case "event":
				eventType = value
			case "id":
				if !strings.ContainsRune(value, '\x00') {
					lastID = value
				}
			case "retry":
				milliseconds, parseErr := strconv.ParseInt(value, 10, 64)
				if parseErr == nil && milliseconds >= 0 && milliseconds <= int64((24*time.Hour)/time.Millisecond) {
					retry = time.Duration(milliseconds) * time.Millisecond
				}
			}
		}
		if errors.Is(err, io.EOF) {
			if data.Len() != 0 {
				if dispatchErr := dispatch(); dispatchErr != nil {
					return lastID, retry, dispatchErr
				}
			}
			return lastID, retry, nil
		}
	}
}

func readEventLine(reader *bufio.Reader, maximum int64) (string, error) {
	var line []byte
	for {
		fragment, prefix, err := reader.ReadLine()
		if int64(len(line))+int64(len(fragment)) > maximum {
			return "", ErrEventTooLarge
		}
		line = append(line, fragment...)
		if !prefix {
			return string(line), err
		}
		if err != nil {
			return "", err
		}
	}
}

type eventDeduper struct {
	maximum int
	order   []string
	seen    map[string]struct{}
}

func newEventDeduper(maximum int) *eventDeduper {
	return &eventDeduper{maximum: maximum, seen: make(map[string]struct{}, maximum)}
}

func (deduper *eventDeduper) Seen(id string) bool {
	_, ok := deduper.seen[id]
	return ok
}

func (deduper *eventDeduper) Add(id string) {
	if deduper.maximum == 0 {
		return
	}
	if _, exists := deduper.seen[id]; exists {
		return
	}
	deduper.seen[id] = struct{}{}
	deduper.order = append(deduper.order, id)
	if len(deduper.order) > deduper.maximum {
		delete(deduper.seen, deduper.order[0])
		deduper.order = deduper.order[1:]
	}
}

func clampDuration(value, minimum, maximum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
