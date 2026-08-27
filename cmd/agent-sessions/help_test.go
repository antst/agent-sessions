package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/clihelp"
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
	for _, command := range []string{"status", "doctor"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run("agent-sessions", []string{command, "--json"}, &stdout, &stderr)
			if code != 0 && code != 3 {
				t.Fatalf("%s --json exit = %d, want success or unavailable", command, code)
			}
			if stderr.Len() != 0 {
				t.Errorf("%s --json wrote non-JSON stderr: %q", command, stderr.String())
			}
			var envelope map[string]any
			decoder := json.NewDecoder(&stdout)
			if err := decoder.Decode(&envelope); err != nil {
				t.Fatalf("%s --json did not emit one JSON object: %v; stdout=%q", command, err, stdout.String())
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				t.Fatalf("%s --json emitted trailing data: %v", command, err)
			}
			contract := clihelp.Contract()
			ok, _ := envelope["ok"].(bool)
			if ok {
				if code != 0 {
					t.Errorf("successful %s JSON used exit %d", command, code)
				}
				assertCLIExactStringSet(t, "success envelope fields", cliMapKeys(envelope), contract.JSONEnvelope.SuccessFields)
				result, resultOK := envelope["result"].(map[string]any)
				if !resultOK {
					t.Fatalf("%s success result = %#v", command, envelope["result"])
				}
				assertCLIExactStringSet(t, command+" result fields", cliMapKeys(result), contract.JSONResultFields[command])
				return
			}
			if code != 3 {
				t.Errorf("failed %s JSON exit = %d, want unavailable class 3", command, code)
			}
			assertCLIExactStringSet(t, "failure envelope fields", cliMapKeys(envelope), contract.JSONEnvelope.FailureFields)
			errorObject, errorOK := envelope["error"].(map[string]any)
			if !errorOK {
				t.Fatalf("%s failure error = %#v", command, envelope["error"])
			}
			assertCLIExactStringSet(t, "failure error fields", cliMapKeys(errorObject), contract.JSONEnvelope.ErrorFields)
			if errorObject["class"] != "unavailable" || errorObject["code"] != "daemon_unavailable" ||
				errorObject["retryable"] != true || strings.TrimSpace(stringValue(errorObject["next_action"])) == "" { //nolint:revive // Dynamic JSON Boolean is intentional.
				t.Errorf("%s unavailable error is not canonical: %#v", command, errorObject)
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
