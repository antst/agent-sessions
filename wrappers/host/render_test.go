package host

import (
	"encoding/json"
	"os"
	"testing"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

func TestRendererMatchesC5Fixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-message-envelope.json")
	must(t, err)
	var fixture struct {
		Message  sessionkit.DeliveryRequest `json:"message"`
		Rendered string                     `json:"rendered"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	got, err := render(fixture.Message)
	must(t, err)
	if got != fixture.Rendered {
		t.Fatalf("rendered = %q, want %q", got, fixture.Rendered)
	}
	fixture.Message.From.SessionID = ""
	if _, err := render(fixture.Message); err == nil {
		t.Fatal("incomplete sender was rendered")
	}
}
