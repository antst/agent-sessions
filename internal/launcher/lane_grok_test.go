package launcher

import (
	"reflect"
	"testing"
)

func TestReplaceLaneEnvironmentPinsValidatedGrok(t *testing.T) {
	t.Parallel()

	got := replaceLaneEnvironment([]string{
		"PATH=/usr/bin",
		"GROK_PEER_GROK_BIN=/wrong/grok",
		"KEEP=value",
	}, "GROK_PEER_GROK_BIN", "/opt/grok-build/grok")
	want := []string{
		"PATH=/usr/bin",
		"KEEP=value",
		"GROK_PEER_GROK_BIN=/opt/grok-build/grok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}
