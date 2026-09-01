package productserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventsReconnectWithLastIDAndDeduplicateReplay(t *testing.T) {
	auth := testAuth(t)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		switch connections.Add(1) {
		case 1:
			if got := request.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("initial Last-Event-ID = %q", got)
			}
			_, _ = fmt.Fprint(response, "id: 1\nevent: update\ndata: first\n\nid: 2\ndata: second\n\n")
		case 2:
			if got := request.Header.Get("Last-Event-ID"); got != "2" {
				t.Errorf("reconnect Last-Event-ID = %q, want 2", got)
			}
			_, _ = fmt.Fprint(response, "id: 2\ndata: duplicate\n\nid: 3\ndata: third\n\n")
		default:
			t.Errorf("unexpected reconnect %d", connections.Load())
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{})
	complete := errors.New("complete")
	var got []Event
	err := client.Subscribe(context.Background(), EventOptions{
		Path:           "/events",
		MaxReconnects:  2,
		ReconnectDelay: time.Millisecond,
		DedupWindow:    4,
	}, func(event Event) error {
		got = append(got, event)
		if event.ID == "3" {
			return complete
		}
		return nil
	})
	if !errors.Is(err, complete) {
		t.Fatalf("Subscribe error = %v, want callback stop", err)
	}
	want := []Event{
		{ID: "1", Type: "update", Data: "first"},
		{ID: "2", Data: "second"},
		{ID: "3", Data: "third"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestEventsDoNotCheckpointOrDispatchUnterminatedEventAtEOF(t *testing.T) {
	auth := testAuth(t)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		switch connections.Add(1) {
		case 1:
			if got := request.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("initial Last-Event-ID = %q", got)
			}
			_, _ = fmt.Fprint(response, "id: replay-id\ndata: truncated\n")
		case 2:
			if got := request.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("truncated event checkpointed Last-Event-ID = %q", got)
			}
			_, _ = fmt.Fprint(response, "id: replay-id\ndata: complete replay\n\n")
		default:
			t.Errorf("unexpected reconnect %d", connections.Load())
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{})
	complete := errors.New("complete")
	var got []Event
	err := client.Subscribe(context.Background(), EventOptions{
		MaxReconnects: 2, ReconnectDelay: time.Millisecond,
	}, func(event Event) error {
		got = append(got, event)
		return complete
	})
	if !errors.Is(err, complete) {
		t.Fatalf("Subscribe error = %v, want callback stop", err)
	}
	want := []Event{{ID: "replay-id", Data: "complete replay"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want only complete replay %#v", got, want)
	}
}

func TestEventsBoundLinesEventsDecompressionAndReconnects(t *testing.T) {
	auth := testAuth(t)
	tests := []struct {
		name    string
		handler http.HandlerFunc
		options EventOptions
		want    error
	}{
		{
			name: "line",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(response, "data: %s\n\n", bytes.Repeat([]byte("x"), 32))
			},
			options: EventOptions{MaxLineBytes: 16, MaxEventBytes: 64},
			want:    ErrEventTooLarge,
		},
		{
			name: "event",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(response, "data: 12345678\ndata: 12345678\n\n")
			},
			options: EventOptions{MaxLineBytes: 32, MaxEventBytes: 12},
			want:    ErrEventTooLarge,
		},
		{
			name: "gzip bomb",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				response.Header().Set("Content-Encoding", "gzip")
				writer := gzip.NewWriter(response)
				_, _ = fmt.Fprintf(writer, "data: %s\n\n", bytes.Repeat([]byte("z"), 256))
				_ = writer.Close()
			},
			options: EventOptions{MaxLineBytes: 32, MaxEventBytes: 32},
			want:    ErrEventTooLarge,
		},
		{
			name: "reconnect",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				response.WriteHeader(http.StatusServiceUnavailable)
			},
			options: EventOptions{MaxReconnects: 2, ReconnectDelay: time.Millisecond},
			want:    ErrReconnectLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client := newTestClient(t, server.URL, auth, Limits{})
			err := client.Subscribe(context.Background(), test.options, func(Event) error { return nil })
			if !errors.Is(err, test.want) {
				t.Fatalf("Subscribe error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestEventsRejectWrongContentTypeAndCallerLastEventID(t *testing.T) {
	auth := testAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte("{}"))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{})
	if err := client.Subscribe(context.Background(), EventOptions{Path: "/"}, func(Event) error { return nil }); !errors.Is(err, ErrInvalidEventStream) {
		t.Fatalf("content type error = %v, want ErrInvalidEventStream", err)
	}
	if err := client.Subscribe(context.Background(), EventOptions{
		Path:   "/",
		Header: http.Header{"Last-Event-ID": []string{"attacker"}},
	}, func(Event) error { return nil }); !errors.Is(err, ErrEventHeaderConflict) {
		t.Fatalf("Last-Event-ID conflict = %v, want ErrEventHeaderConflict", err)
	}
}

func TestEventsClearLastIDAndBoundCommentOnlyBlocks(t *testing.T) {
	auth := testAuth(t)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		switch connections.Add(1) {
		case 1:
			_, _ = fmt.Fprint(response, "id: 1\ndata: first\n\n")
		case 2:
			if got := request.Header.Get("Last-Event-ID"); got != "1" {
				t.Errorf("second Last-Event-ID = %q, want 1", got)
			}
			_, _ = fmt.Fprint(response, "id:\n\n")
		case 3:
			if got := request.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("cleared Last-Event-ID = %q", got)
			}
			_, _ = fmt.Fprint(response, "id: 2\ndata: second\n\n")
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{})
	complete := errors.New("complete")
	if err := client.Subscribe(context.Background(), EventOptions{
		MaxReconnects: 3, ReconnectDelay: time.Millisecond,
	}, func(event Event) error {
		if event.ID == "2" {
			return complete
		}
		return nil
	}); !errors.Is(err, complete) {
		t.Fatalf("clear Last-Event-ID stream error = %v", err)
	}

	comments := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(response, ": 12345678\n: 12345678\n")
	}))
	defer comments.Close()
	commentClient := newTestClient(t, comments.URL, auth, Limits{})
	if err := commentClient.Subscribe(context.Background(), EventOptions{
		MaxLineBytes: 32, MaxEventBytes: 16,
	}, func(Event) error { return nil }); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("comment-only block error = %v, want ErrEventTooLarge", err)
	}
}
