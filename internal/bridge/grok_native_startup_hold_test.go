package bridge

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokNativeStartupHoldUsesAuthenticatedNativeClientAndClosesOnce(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "grok")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$GROK_FAKE_TEST_BINARY\" -test.run='^TestGrokFakeProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "requests.jsonl")
	environment := append(os.Environ(),
		grokFakeProcessEnv+"=1",
		"GROK_FAKE_TEST_BINARY="+testBinary,
		"GROK_FAKE_RECORD="+record,
	)
	hold, err := OpenGrokNativeStartupHold(
		context.Background(), wrapper, root, "/tmp/grok-native-startup-hold-test.sock", environment, io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	hold.Close()
	hold.Close()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	requests := string(body)
	if strings.Count(requests, `"method":"initialize"`) != 1 || strings.Count(requests, `"method":"authenticate"`) != 1 {
		t.Fatalf("startup hold requests = %s", requests)
	}
}

func TestGrokNativeStartupHoldRejectsFailedAuthentication(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "grok")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$GROK_FAKE_TEST_BINARY\" -test.run='^TestGrokFakeProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), grokFakeProcessEnv+"=1", "GROK_FAKE_TEST_BINARY="+testBinary, "GROK_FAKE_AUTH_REJECT=1")
	if _, err := OpenGrokNativeStartupHold(
		context.Background(), wrapper, root, "/tmp/grok-native-startup-hold-test.sock", environment, io.Discard,
	); err == nil || !strings.Contains(err.Error(), "bad cached token") {
		t.Fatalf("authentication error = %v", err)
	}
}
