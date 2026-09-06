package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

type recordingInput struct{ bytes.Buffer }

func (*recordingInput) Close() error { return nil }

func TestResponseBeforeEOFWins(t *testing.T) {
	id := int64(1)
	input := &recordingInput{}
	client := &acpClient{
		input:       input,
		responses:   make(chan acpFrame, 1),
		interjected: make(chan interjectionNotice),
		done:        make(chan struct{}),
	}
	client.responses <- acpFrame{ID: &id, Result: json.RawMessage(`{"value":"read"}`)}
	client.finish(io.EOF)
	var result struct {
		Value string `json:"value"`
	}
	if err := client.request(context.Background(), "example", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != "read" {
		t.Fatalf("result = %#v", result)
	}
}
