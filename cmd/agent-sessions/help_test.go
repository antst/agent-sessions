package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/daemon"
)

func TestAdministrativeHelpUsesTheCanonicalParsedOptionInventory(t *testing.T) {
	tests := []struct {
		key  string
		args []string
	}{
		{key: "host.help", args: []string{"help"}},
		{key: "host.status", args: []string{"status"}},
		{key: "host.doctor", args: []string{"doctor"}},
		{key: "host.remove.inspect", args: []string{"remove", "inspect"}},
		{key: "host.purge.inspect", args: []string{"purge", "inspect"}},
		{key: "host.purge.apply", args: []string{"purge", "apply"}},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			parsed, err := clihelp.ParserOptionNames(test.key)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run("agent-sessions", append(append([]string(nil), test.args...), "--help"), &stdout, &stderr); code != 0 {
				t.Fatalf("help exit = %d, stderr = %q", code, stderr.String())
			}
			assertCLIExactStringSet(t, "rendered help options", helpOptionNames(stdout.String()), parsed)
		})
	}
}

func TestAdministrativeJSONUsesCanonicalUnavailableEnvelope(t *testing.T) {
	previousQuery := queryHostAdmin
	queryCalls := 0
	queryHostAdmin = func(_ context.Context, _ string) (json.RawMessage, error) {
		queryCalls++
		return nil, &daemon.UnavailableError{
			Endpoint: "/test/agent-sessions.sock", Cause: errors.New("fixture daemon is absent"), NextAction: "inspect fixture service",
		}
	}
	t.Cleanup(func() { queryHostAdmin = previousQuery })
	for _, args := range [][]string{{"status", "--json"}, {"doctor", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run("agent-sessions", args, &stdout, &stderr); code != 3 {
			t.Fatalf("%v exit = %d, want unavailable", args, code)
		}
		if stderr.Len() != 0 {
			t.Fatalf("%v wrote stderr %q", args, stderr.String())
		}
		var envelope map[string]any
		decoder := json.NewDecoder(&stdout)
		if err := decoder.Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			t.Fatalf("trailing JSON: %v", err)
		}
		assertCLIExactStringSet(t, "failure envelope", cliMapKeys(envelope), clihelp.Contract().JSONEnvelope.FailureFields)
	}
	if queryCalls != 2 {
		t.Fatalf("online administrative transport calls = %d, want 2", queryCalls)
	}
}

func TestAdministrativeParserRejectsUndocumentedOptionsBeforeDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("agent-sessions", []string{"status", "--not-a-real-option"}, &stdout, &stderr); code != 2 {
		t.Fatalf("undocumented status option exit = %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run("agent-sessions", []string{"purge", "apply", "--plan"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing --plan value exit = %d", code)
	}
}

func TestCanonicalConnectorModeDispatchesOneProductRelay(t *testing.T) {
	previous := runConnectorRelay
	t.Cleanup(func() { runConnectorRelay = previous })
	called := ""
	runConnectorRelay = func(product string, _ io.Reader, _, _ io.Writer) int {
		called = product
		return 17
	}
	var stdout, stderr bytes.Buffer
	if code := run("agent-sessions", []string{"connector", "grok", "mcp"}, &stdout, &stderr); code != 17 || called != "grok" {
		t.Fatalf("connector exit/product = %d/%q", code, called)
	}
}

func TestMachineHelpProjectsCanonicalEnvironmentJSONAndExitContracts(t *testing.T) {
	contract := clihelp.Contract()
	wantEnvironment := []string{
		"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "QWEN_HOME", "QWEN_RUNTIME_DIR",
	}
	if !slices.Equal(contract.EnvironmentNames, wantEnvironment) {
		t.Errorf("canonical environment = %q, want %q", contract.EnvironmentNames, wantEnvironment)
	}
	var stdout, stderr bytes.Buffer
	if code := run("agent-sessions", []string{"help", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help --json exit = %d, stderr=%q", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertCLIExactStringSet(t, "help envelope fields", cliMapKeys(envelope), contract.JSONEnvelope.SuccessFields)
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("help result = %#v", envelope["result"])
	}
	assertCLIExactStringSet(t, "help result fields", cliMapKeys(result), contract.JSONResultFields["help"])
}

func helpOptionNames(help string) []string {
	var names []string
	for _, line := range strings.Split(help, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-") {
			names = append(names, strings.Fields(line)[0])
		}
	}
	return names
}

func cliMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func assertCLIExactStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	got, want = append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want exactly %q", label, got, want)
	}
}
