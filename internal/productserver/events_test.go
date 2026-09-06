package productserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/antst/sessionbus/internal/productruntime"
)

func TestSubscribeStreamsServerSentEventsOnce(t *testing.T) {
	auth, _ := NewMemoryAuth("X-Test", productruntime.NewSensitiveValue("stream"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("id: one\nevent: turn\ndata: first\ndata: second\n\ndata: done\n\n"))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	err = client.Subscribe(context.Background(), EventOptions{Path: "/events"}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{{ID: "one", Type: "turn", Data: "first\nsecond"}, {Data: "done"}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}
