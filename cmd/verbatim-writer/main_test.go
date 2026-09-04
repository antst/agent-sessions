package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestWriteDeliveryJSONL(t *testing.T) {
	var out bytes.Buffer
	if err := writeDelivery(&out, json.RawMessage(`{"message_id":"m1","from":{"uuid":"u","name":"n\t","groups":["g\n"],"product":"p"},"body":"b\n\t"}`)); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Time string `json:"time"`
		productruntime.NativeMessage
	}
	if json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got) != nil || bytes.Count(out.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("invalid JSONL %q", out.String())
	}
	if _, err := time.Parse(time.RFC3339Nano, got.Time); err != nil || got.ID != "m1" || got.From.Name != "n\t" || got.From.Groups[0] != "g\n" || got.Body != "b\n\t" {
		t.Fatalf("unexpected record %#v: %v", got, err)
	}
}
