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
		{key: "host.migrate.inspect", args: []string{"migrate", "inspect"}},
		{key: "host.migrate.status", args: []string{"migrate", "status"}},
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
			actual := helpOptionNames(stdout.String())
			assertCLIExactStringSet(t, "rendered help options", actual, parsed)
		})
	}
}

func TestAdministrativeJSONUsesCanonicalEnvelopeSchemaAndExitClass(t *testing.T) {
	previousInspect, previousStatus := runHostMigrationInspect, runHostMigrationStatus
	inspectCalls, statusCalls := 0, 0
	runHostMigrationInspect = func(context.Context) (daemon.MigrationInspectProjection, error) {
		inspectCalls++
		return daemon.MigrationInspectProjection{
			Candidates: []daemon.LegacyRuntimeCandidate{}, Blockers: []daemon.LegacyMigrationBlocker{}, Debt: []daemon.LegacyMigrationDebt{},
		}, nil
	}
	runHostMigrationStatus = func(context.Context) (daemon.MigrationStatusProjection, error) {
		statusCalls++
		return daemon.MigrationStatusProjection{State: "none", NextAction: "none"}, nil
	}
	t.Cleanup(func() { runHostMigrationInspect, runHostMigrationStatus = previousInspect, previousStatus })

	tests := []struct {
		name     string
		args     []string
		contract string
		offline  bool
	}{
		{name: "status", args: []string{"status"}, contract: "status"},
		{name: "doctor", args: []string{"doctor"}, contract: "doctor"},
		{name: "migrate-inspect", args: []string{"migrate", "inspect"}, contract: "migrate.inspect", offline: true},
		{name: "migrate-status", args: []string{"migrate", "status"}, contract: "migrate.status", offline: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run("agent-sessions", append(append([]string(nil), test.args...), "--json"), &stdout, &stderr)
			if test.offline && code != 0 {
				t.Fatalf("offline %s --json exit = %d, want 0 without a daemon; stdout=%q stderr=%q", test.name, code, stdout.String(), stderr.String())
			}
			if code != 0 && code != 3 {
				t.Fatalf("%s --json exit = %d, want success or unavailable", test.name, code)
			}
			if stderr.Len() != 0 {
				t.Errorf("%s --json wrote non-JSON stderr: %q", test.name, stderr.String())
			}
			var envelope map[string]any
			decoder := json.NewDecoder(&stdout)
			if err := decoder.Decode(&envelope); err != nil {
				t.Fatalf("%s --json did not emit one JSON object: %v; stdout=%q", test.name, err, stdout.String())
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				t.Fatalf("%s --json emitted trailing data: %v", test.name, err)
			}
			contract := clihelp.Contract()
			ok, _ := envelope["ok"].(bool)
			if ok {
				if code != 0 {
					t.Errorf("successful %s JSON used exit %d", test.name, code)
				}
				assertCLIExactStringSet(t, "success envelope fields", cliMapKeys(envelope), contract.JSONEnvelope.SuccessFields)
				result, resultOK := envelope["result"].(map[string]any)
				if !resultOK {
					t.Fatalf("%s success result = %#v", test.name, envelope["result"])
				}
				assertCLIExactStringSet(t, test.contract+" result fields", cliMapKeys(result), contract.JSONResultFields[test.contract])
				return
			}
			if code != 3 {
				t.Errorf("failed %s JSON exit = %d, want unavailable class 3", test.name, code)
			}
			assertCLIExactStringSet(t, "failure envelope fields", cliMapKeys(envelope), contract.JSONEnvelope.FailureFields)
			errorObject, errorOK := envelope["error"].(map[string]any)
			if !errorOK {
				t.Fatalf("%s failure error = %#v", test.name, envelope["error"])
			}
			assertCLIExactStringSet(t, "failure error fields", cliMapKeys(errorObject), contract.JSONEnvelope.ErrorFields)
			if errorObject["class"] != "unavailable" || errorObject["code"] != "daemon_unavailable" ||
				errorObject["retryable"] != true || strings.TrimSpace(stringValue(errorObject["next_action"])) == "" { //nolint:revive // Dynamic JSON Boolean is intentional.
				t.Errorf("%s unavailable error is not canonical: %#v", test.name, errorObject)
			}
		})
	}
	if inspectCalls != 1 || statusCalls != 1 {
		t.Fatalf("offline migration dispatch calls inspect=%d status=%d, want one each", inspectCalls, statusCalls)
	}
}

func TestMigrationAdministrativeFailuresPreserveStableSpecificExitClasses(t *testing.T) {
	command, _, err := clihelp.ResolveCommand("agent-sessions", []string{"migrate", "status"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		failure   *daemon.AdministrativeError
		exit      int
		class     string
		code      string
		retryable bool
	}{
		{
			name: "exact blocker", exit: 4, class: "refused", code: "migration_blocked",
			failure: &daemon.AdministrativeError{Operation: "migration.status", Code: "migration_blocked", Message: "one exact peer remains live", NextAction: "close peer peer-owner and retry"},
		},
		{
			name: "unsafe state root", exit: 4, class: "refused", code: "migration_state_unsafe",
			failure: &daemon.AdministrativeError{Operation: "migration.status", Code: "migration_state_unsafe", Message: "state root is not owner-only", NextAction: "restore the owner-only canonical state path"},
		},
		{
			name: "incompatible state", exit: 5, class: "incompatible", code: "migration_state_incompatible",
			failure: &daemon.AdministrativeError{Operation: "migration.status", Code: "migration_state_incompatible", Message: "selector schema is unsupported", NextAction: "install a compatible Agent Sessions release"},
		},
		{
			name: "incomplete state", exit: 6, class: "retryable", code: "migration_state_incomplete", retryable: true,
			failure: &daemon.AdministrativeError{Operation: "migration.status", Code: "migration_state_incomplete", Message: "selected record is absent", Retryable: true, NextAction: "restore the exact selected record and retry"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := renderAdministrativeFailure(command, true, test.failure, &stdout, &stderr); got != test.exit {
				t.Fatalf("exit = %d, want %d", got, test.exit)
			}
			if stderr.Len() != 0 {
				t.Fatalf("machine failure wrote stderr %q", stderr.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			errorObject, ok := envelope["error"].(map[string]any)
			if !ok || errorObject["class"] != test.class || errorObject["code"] != test.code ||
				errorObject["retryable"] != test.retryable || errorObject["next_action"] != test.failure.NextAction {
				t.Fatalf("migration failure envelope = %#v", envelope)
			}
		})
	}
}

func TestAdministrativeParserRejectsUndocumentedOptionsBeforeDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run("agent-sessions", []string{"status", "--not-a-real-option"}, &stdout, &stderr); code != 2 {
		t.Fatalf("undocumented status option exit = %d, want usage class 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run("agent-sessions", []string{"purge", "apply", "--plan"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing --plan value exit = %d, want usage class 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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
	if code := run("agent-sessions", []string{"connector", "grok", "mcp"}, &stdout, &stderr); code != 17 {
		t.Fatalf("connector exit = %d, stderr=%q", code, stderr.String())
	}
	if called != "grok" {
		t.Fatalf("connector product = %q", called)
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
	for _, name := range contract.EnvironmentNames {
		if strings.HasPrefix(name, "AGENT_SESSIONS_") {
			t.Errorf("public environment invents daemon identity selector %q", name)
		}
	}
	wantExit := []struct {
		code int
		name string
	}{{0, "success"}, {1, "internal"}, {2, "usage"}, {3, "unavailable"}, {4, "refused"}, {5, "incompatible"}, {6, "retryable"}}
	if len(contract.ExitClasses) != len(wantExit) {
		t.Fatalf("exit classes = %+v, want %d", contract.ExitClasses, len(wantExit))
	}
	for index, want := range wantExit {
		if got := contract.ExitClasses[index]; got.Code != want.code || got.Name != want.name {
			t.Errorf("exit class[%d] = %+v, want %d/%s", index, got, want.code, want.name)
		}
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
	modes, ok := result["modes"].([]any)
	if !ok || len(modes) != len(contract.Commands) {
		t.Fatalf("help modes = %T/%d, want %d canonical descriptors", result["modes"], len(modes), len(contract.Commands))
	}
	for _, command := range contract.Commands {
		parsed, err := clihelp.ParserOptionNames(command.Key)
		if err != nil {
			t.Errorf("instantiate parser %s: %v", command.Key, err)
			continue
		}
		help, err := clihelp.HelpOptionNames(command.Key)
		if err != nil {
			t.Errorf("render help %s: %v", command.Key, err)
			continue
		}
		assertCLIExactStringSet(t, command.Key+" parser/help parity", parsed, help)
		if command.JSONResultContract != "" {
			if _, exists := contract.JSONResultFields[command.JSONResultContract]; !exists {
				t.Errorf("%s references undocumented JSON result %q", command.Key, command.JSONResultContract)
			}
		}
	}
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
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want exactly %q", label, got, want)
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
