package host

import (
	"encoding/json"
	"os"
	"testing"

	sessionkit "github.com/antst/sessionbus/bus/sdk/go"
)

func TestRendererMatchesC5Fixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-message-envelope.json")
	must(t, err)
	var fixture struct {
		Message  sessionkit.DeliveryRequest `json:"message"`
		Rendered string                     `json:"rendered"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	got, err := RenderNativeMessage(fixture.Message)
	must(t, err)
	check(t, got == fixture.Rendered, "rendered = %q, want %q", got, fixture.Rendered)
	fixture.Message.From.SessionID = ""
	_, err = RenderNativeMessage(fixture.Message)
	check(t, err != nil, "incomplete sender was rendered")
}
