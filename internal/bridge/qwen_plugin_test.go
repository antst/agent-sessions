package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const (
	qwenTestPluginSchema       = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	qwenTestMCPSchema          = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	qwenTestPluginName         = "agent-sessions"
	qwenTestMCPName            = "agent_sessions"
	qwenTestMCPCommand         = "./scripts/native-entry"
	qwenTestPluginVersion      = qwenreadiness.IntegrationVersion
	qwenPluginProcessHelperEnv = "AGENT_SESSIONS_QWEN_PLUGIN_PROCESS_HELPER"
)

var qwenTestPluginSkills = []string{
	"agent-sessions",
	"claude-lane",
	"codex-lane",
	"grok-lane",
	"qwen-lane",
}

func TestQwenPluginPayloadMatchesAgentPluginsV1Contract(t *testing.T) {
	assertQwenPluginPayload(t, filepath.Join("..", "..", "qwen"))
}

func TestQwenPluginInstallCommandUsesOnlySelectedProfile(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "plugin")
	qwenExecutable := filepath.Join(root, "bin", "qwen")
	ambientHome := filepath.Join(root, "ambient-qwen")
	ambientRuntime := filepath.Join(root, "ambient-runtime")
	baseEnvironment := []string{
		"HOME=" + root,
		"QWEN_HOME=" + ambientHome,
		"QWEN_RUNTIME_DIR=" + ambientRuntime,
		"PRESERVE_QWEN_INSTALL_TEST=present",
	}
	wantArgs := []string{
		qwenExecutable, "extensions", "install", pluginRoot, "--scope", "user", "--consent",
	}

	defaultProfile := qwenTestProfileIdentity(t, map[string]string{"HOME": root})
	explicitHome := filepath.Join(root, "selected-qwen")
	explicitRuntime := filepath.Join(root, "selected-runtime")
	explicitProfile := qwenTestProfileIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": explicitHome, "QWEN_RUNTIME_DIR": explicitRuntime,
	})

	for _, test := range []struct {
		name        string
		profile     qwenprofile.Identity
		wantHome    string
		wantRuntime string
	}{
		{name: "native default remains unset", profile: defaultProfile},
		{name: "explicit profile is exact", profile: explicitProfile, wantHome: explicitProfile.QwenHome, wantRuntime: explicitProfile.QwenRuntimeDir},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, err := qwenPluginInstallCommand(qwenExecutable, pluginRoot, test.profile, baseEnvironment)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(command.Args, wantArgs) {
				t.Fatalf("Qwen install argv = %q, want %q", command.Args, wantArgs)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "QWEN_HOME"); present != test.profile.QwenHomeSet || got != test.wantHome {
				t.Fatalf("Qwen install QWEN_HOME = %q, present=%v", got, present)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "QWEN_RUNTIME_DIR"); present != test.profile.QwenRuntimeSet || got != test.wantRuntime {
				t.Fatalf("Qwen install QWEN_RUNTIME_DIR = %q, present=%v", got, present)
			}
			if got, present := qwenTestEnvironmentValue(command.Env, "PRESERVE_QWEN_INSTALL_TEST"); !present || got != "present" {
				t.Fatalf("unrelated install environment was not preserved: %q, present=%v", got, present)
			}
		})
	}
}

func TestVerifyQwenPluginInstallationRequiresExactEnabledInventory(t *testing.T) {
	if err := verifyQwenPluginInstallation(qwenTestPluginFixture(t, nil), qwenTestPluginVersion, true); err != nil {
		t.Fatalf("valid Qwen plugin installation rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(string)
		enabled bool
		want    string
	}{
		{name: "disabled", enabled: false, want: "enabled"},
		{name: "wrong version", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "plugin.json"), func(value map[string]any) { value["version"] = "0.2.0" })
		}, want: "version"},
		{name: "wrong plugin schema", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "plugin.json"), func(value map[string]any) { value["$schema"] = "https://example.invalid/plugin.schema.json" })
		}, want: "schema"},
		{name: "wrong MCP name", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "mcp.json"), func(value map[string]any) {
				servers := value["mcpServers"].(map[string]any)
				servers["wrong_name"] = servers[qwenTestMCPName]
				delete(servers, qwenTestMCPName)
			})
		}, want: qwenTestMCPName},
		{name: "ambient PATH MCP command", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "mcp.json"), func(value map[string]any) {
				servers := value["mcpServers"].(map[string]any)
				servers[qwenTestMCPName].(map[string]any)["command"] = "agent-session-runtime"
			})
		}, want: "exact"},
		{name: "native Qwen extension variable syntax", enabled: true, mutate: func(root string) {
			qwenTestRewriteJSONObject(t, filepath.Join(root, "mcp.json"), func(value map[string]any) {
				servers := value["mcpServers"].(map[string]any)
				servers[qwenTestMCPName].(map[string]any)["command"] = "${extensionPath}${/}scripts${/}native-entry"
			})
		}, want: "exact"},
		{name: "missing native entry", enabled: true, mutate: func(root string) {
			if err := os.Remove(filepath.Join(root, "scripts", "native-entry")); err != nil {
				t.Fatal(err)
			}
		}, want: "native entry"},
		{name: "missing skill", enabled: true, mutate: func(root string) {
			if err := os.Remove(filepath.Join(root, "skills", "qwen-lane", "SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}, want: "skills"},
		{name: "extra skill", enabled: true, mutate: func(root string) {
			qwenTestWriteSkill(t, root, "unexpected")
		}, want: "skills"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := qwenTestPluginFixture(t, test.mutate)
			err := verifyQwenPluginInstallation(root, qwenTestPluginVersion, test.enabled)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("verification error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestQwenPluginPolicyRejectsDuplicateAndMalformedState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	for _, test := range []struct {
		name  string
		state map[string]any
		want  string
	}{
		{name: "unsupported version", state: map[string]any{"version": 1, "extensions": map[string]any{}}, want: "version"},
		{name: "missing inventory", state: map[string]any{"version": 2}, want: "inventory"},
		{name: "duplicate policy", state: map[string]any{"version": 2, "extensions": map[string]any{
			"one": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
			"two": map[string]any{"name": qwenPluginName, "defaultActivation": "disabled"},
		}}, want: "2 agent-sessions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			qwenTestWriteJSON(t, statePath, test.state)
			if _, _, err := qwenPluginPolicy(statePath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("policy error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestQwenPluginMutationRefusesExactLiveProfileAndAllowsDifferentProfile(t *testing.T) {
	root := t.TempDir()
	selected := qwenTestProfileIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": filepath.Join(root, "selected"), "QWEN_RUNTIME_DIR": filepath.Join(root, "runtime"),
	})
	other := qwenTestProfileIdentity(t, map[string]string{
		"HOME": root, "QWEN_HOME": filepath.Join(root, "other"), "QWEN_RUNTIME_DIR": filepath.Join(root, "other-runtime"),
	})
	start := func(profile qwenprofile.Identity) *exec.Cmd {
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(testBinary, "-test.run=^TestQwenPluginManagedProcessHelper$", "--", "qwen-host")
		command.Env = qwenprofile.ApplyEnvironment(os.Environ(), profile)
		command.Env = append(command.Env, "AGENT_SESSIONS_PRODUCT=qwen", qwenPluginProcessHelperEnv+"=1")
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
		deadline := time.Now().Add(time.Second)
		for {
			arguments, argsErr := procinfo.Args(command.Process.Pid)
			if argsErr == nil && looksLikeManagedQwenRuntime(arguments) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("managed Qwen process %d did not exec fixture: args=%v err=%v", command.Process.Pid, arguments, argsErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return command
	}
	otherProcess := start(other)
	_, otherEnvironmentErr := procinfo.Environment(otherProcess.Process.Pid)
	if err := refuseLiveQwenPluginMutation(selected); otherEnvironmentErr == nil && err != nil {
		t.Fatalf("observable different Qwen profile blocked selected-profile mutation: %v (pid %d)", err, otherProcess.Process.Pid)
	} else if otherEnvironmentErr != nil && (err == nil || !strings.Contains(err.Error(), "process "+strconv.Itoa(otherProcess.Process.Pid))) {
		t.Fatalf("unobservable managed Qwen profile did not fail closed: environment=%v refusal=%v", otherEnvironmentErr, err)
	}
	_ = otherProcess.Process.Kill()
	_ = otherProcess.Wait()
	selectedProcess := start(selected)
	deadline := time.Now().Add(time.Second)
	for {
		err := refuseLiveQwenPluginMutation(selected)
		if err != nil {
			if !strings.Contains(err.Error(), "process "+strconv.Itoa(selectedProcess.Process.Pid)) {
				t.Fatalf("live-profile refusal = %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exact live Qwen profile did not block plugin mutation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQwenPluginManagedProcessHelper(_ *testing.T) {
	if os.Getenv(qwenPluginProcessHelperEnv) != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestQwenPluginRemoveIsProfileScopedIdempotentAndPreservesOwnerFiles(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen-home")
	runtimeDir := filepath.Join(root, "qwen-runtime")
	pluginRoot := filepath.Join(qwenHome, "extensions", qwenPluginName)
	statePath := filepath.Join(qwenHome, "extension-store", "state.json")
	ownerFile := filepath.Join(root, "owner-settings.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerFile, []byte("owner-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	qwenTestPluginFixtureAt(t, pluginRoot)
	qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
		"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
	}})
	fakeQwen := filepath.Join(root, "qwen")
	script := `#!/bin/sh
set -eu
[ "$1" = extensions ] && [ "$2" = uninstall ] && [ "$3" = agent-sessions ]
/usr/bin/find "$QWEN_HOME/extensions/agent-sessions" -depth -delete
printf '%s\n' '{"version":2,"extensions":{}}' >"$QWEN_HOME/extension-store/state.json"
`
	if err := os.WriteFile(fakeQwen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_HOME", qwenHome)
	t.Setenv("QWEN_RUNTIME_DIR", runtimeDir)
	if exit := runQwenPluginRemove([]string{"--qwen", fakeQwen}); exit != 0 {
		t.Fatalf("Qwen plugin remove exit = %d", exit)
	}
	if exit := runQwenPluginRemove([]string{"--qwen", fakeQwen}); exit != 0 {
		t.Fatalf("idempotent Qwen plugin remove exit = %d", exit)
	}
	if body, err := os.ReadFile(ownerFile); err != nil || string(body) != "owner-state\n" {
		t.Fatalf("owner file changed: body=%q err=%v", body, err)
	}
}

func TestQwenPluginUpgradeUsesNativeUpdateAndPreservesProfileState(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen-home")
	qwenRuntime := filepath.Join(root, "qwen-runtime")
	source := filepath.Join(root, "source")
	installed := filepath.Join(qwenHome, "extensions", qwenPluginName)
	statePath := filepath.Join(qwenHome, "extension-store", "state.json")
	settingsPath := filepath.Join(qwenHome, "settings.json")
	qwenTestPluginFixtureAt(t, source)
	qwenTestPluginFixtureAt(t, installed)
	qwenTestRewriteJSONObject(t, filepath.Join(installed, "plugin.json"), func(value map[string]any) {
		value["version"] = "0.2.0"
	})
	qwenTestWriteJSON(t, filepath.Join(installed, ".qwen-extension-install.json"), map[string]any{
		"type": "local", "source": source,
	})
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
		"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
	}})
	if err := os.WriteFile(settingsPath, []byte("{\"theme\":\"owner\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "qwen.log")
	fakeQwen := filepath.Join(root, "qwen")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$QWEN_TEST_LOG"
[ "$1" = extensions ] && [ "$2" = update ] && [ "$3" = agent-sessions ]
/usr/bin/find "$QWEN_HOME/extensions/agent-sessions" -depth -delete
/bin/mkdir -p "$QWEN_HOME/extensions/agent-sessions"
/bin/cp -R "$QWEN_PLUGIN_SOURCE/." "$QWEN_HOME/extensions/agent-sessions/"
`
	if err := os.WriteFile(fakeQwen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_HOME", qwenHome)
	t.Setenv("QWEN_RUNTIME_DIR", qwenRuntime)
	t.Setenv("QWEN_PLUGIN_SOURCE", source)
	t.Setenv("QWEN_TEST_LOG", logPath)
	if exit := runQwenPluginInstall([]string{
		"--qwen", fakeQwen, "--plugin-root", source, "--version", qwenTestPluginVersion,
	}); exit != 0 {
		t.Fatalf("Qwen plugin upgrade exit = %d", exit)
	}
	if body, err := os.ReadFile(logPath); err != nil || string(body) != "extensions update agent-sessions\n" {
		t.Fatalf("Qwen upgrade command = %q, %v", body, err)
	}
	if err := verifyQwenPluginInstallation(installed, qwenTestPluginVersion, true); err != nil {
		t.Fatalf("upgraded Qwen plugin = %v", err)
	}
	if body, err := os.ReadFile(settingsPath); err != nil || string(body) != "{\"theme\":\"owner\"}\n" {
		t.Fatalf("Qwen owner settings changed: %q, %v", body, err)
	}
}

func TestQwenPluginSameVersionDriftUsesNativeReplacement(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen-home")
	qwenRuntime := filepath.Join(root, "qwen-runtime")
	source := filepath.Join(root, "source")
	installed := filepath.Join(qwenHome, "extensions", qwenPluginName)
	statePath := filepath.Join(qwenHome, "extension-store", "state.json")
	settingsPath := filepath.Join(qwenHome, "settings.json")
	qwenTestPluginFixtureAt(t, source)
	qwenTestPluginFixtureAt(t, installed)
	qwenTestRewriteJSONObject(t, filepath.Join(installed, "mcp.json"), func(value map[string]any) {
		servers := value["mcpServers"].(map[string]any)
		servers[qwenTestMCPName].(map[string]any)["command"] = "agent-session-runtime"
	})
	if err := os.Remove(filepath.Join(installed, "scripts", "native-entry")); err != nil {
		t.Fatal(err)
	}
	qwenTestWriteJSON(t, filepath.Join(installed, ".qwen-extension-install.json"), map[string]any{
		"type": "local", "source": source,
	})
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
		"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
	}})
	if err := os.WriteFile(settingsPath, []byte("{\"theme\":\"owner\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "qwen.log")
	fakeQwen := filepath.Join(root, "qwen")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$QWEN_TEST_LOG"
case "$2" in
  uninstall)
    /usr/bin/find "$QWEN_HOME/extensions/agent-sessions" -depth -delete
    ;;
  install)
    plugin_source=$3
    /bin/mkdir -p "$QWEN_HOME/extensions/agent-sessions"
    /bin/cp -R "$plugin_source/." "$QWEN_HOME/extensions/agent-sessions/"
    printf '{"type":"local","source":"%s"}\n' "$plugin_source" >"$QWEN_HOME/extensions/agent-sessions/.qwen-extension-install.json"
    ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(fakeQwen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_HOME", qwenHome)
	t.Setenv("QWEN_RUNTIME_DIR", qwenRuntime)
	t.Setenv("QWEN_TEST_LOG", logPath)
	if exit := runQwenPluginInstall([]string{
		"--qwen", fakeQwen, "--plugin-root", source, "--version", qwenTestPluginVersion,
	}); exit != 0 {
		t.Fatalf("same-version Qwen drift reconciliation exit = %d", exit)
	}
	wantLog := "extensions uninstall agent-sessions\n" +
		"extensions install " + source + " --scope user --consent\n"
	if body, err := os.ReadFile(logPath); err != nil || string(body) != wantLog {
		t.Fatalf("same-version Qwen drift reconciliation commands = %q, %v", body, err)
	}
	if err := verifyQwenPluginInstallation(installed, qwenTestPluginVersion, true); err != nil {
		t.Fatalf("reconciled Qwen plugin = %v", err)
	}
	if body, err := os.ReadFile(settingsPath); err != nil || string(body) != "{\"theme\":\"owner\"}\n" {
		t.Fatalf("Qwen owner settings changed: %q, %v", body, err)
	}
}

func TestQwenPluginInstallReconcilesSameVersionDeveloperSource(t *testing.T) {
	root := t.TempDir()
	qwenHome := filepath.Join(root, "qwen-home")
	qwenRuntime := filepath.Join(root, "qwen-runtime")
	developerSource := filepath.Join(root, "checkout", "qwen")
	installedSource := filepath.Join(root, "stable", "qwen")
	installed := filepath.Join(qwenHome, "extensions", qwenPluginName)
	statePath := filepath.Join(qwenHome, "extension-store", "state.json")
	qwenTestPluginFixtureAt(t, developerSource)
	qwenTestPluginFixtureAt(t, installedSource)
	qwenTestPluginFixtureAt(t, installed)
	qwenTestWriteJSON(t, filepath.Join(installed, ".qwen-extension-install.json"), map[string]any{
		"type": "local", "source": developerSource,
	})
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
		"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
	}})
	logPath := filepath.Join(root, "qwen.log")
	fakeQwen := filepath.Join(root, "qwen")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$QWEN_TEST_LOG"
case "$2" in
  uninstall)
    /usr/bin/find "$QWEN_HOME/extensions/agent-sessions" -depth -delete
    ;;
  install)
    source=$3
    /bin/mkdir -p "$QWEN_HOME/extensions/agent-sessions"
    /bin/cp -R "$source/." "$QWEN_HOME/extensions/agent-sessions/"
    printf '{"type":"local","source":"%s"}\n' "$source" >"$QWEN_HOME/extensions/agent-sessions/.qwen-extension-install.json"
    ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(fakeQwen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_HOME", qwenHome)
	t.Setenv("QWEN_RUNTIME_DIR", qwenRuntime)
	t.Setenv("QWEN_TEST_LOG", logPath)
	if exit := runQwenPluginInstall([]string{
		"--qwen", fakeQwen, "--plugin-root", installedSource, "--version", qwenTestPluginVersion,
	}); exit != 0 {
		t.Fatalf("same-version Qwen source reconciliation exit = %d", exit)
	}
	wantLog := "extensions uninstall agent-sessions\n" +
		"extensions install " + installedSource + " --scope user --consent\n"
	if body, err := os.ReadFile(logPath); err != nil || string(body) != wantLog {
		t.Fatalf("same-version source reconciliation commands = %q, %v", body, err)
	}
	source, err := installedQwenPluginSource(installed)
	if err != nil || !sameQwenPluginSource(source, installedSource) {
		t.Fatalf("reconciled Qwen source = %q, %v", source, err)
	}
}

func TestQwenPluginReplacementRestoresPriorPluginAfterFailure(t *testing.T) {
	for _, failureMode := range []string{"native", "verification"} {
		t.Run(failureMode, func(t *testing.T) {
			root := t.TempDir()
			qwenHome := filepath.Join(root, "qwen-home")
			qwenRuntime := filepath.Join(root, "qwen-runtime")
			priorSource := filepath.Join(root, "prior-source")
			newSource := filepath.Join(root, "new-source")
			installed := filepath.Join(qwenHome, "extensions", qwenPluginName)
			statePath := filepath.Join(qwenHome, "extension-store", "state.json")
			settingsPath := filepath.Join(qwenHome, "settings.json")
			qwenTestPluginFixtureAt(t, priorSource)
			qwenTestPluginFixtureAt(t, newSource)
			qwenTestPluginFixtureAt(t, installed)
			qwenTestWriteJSON(t, filepath.Join(installed, ".qwen-extension-install.json"), map[string]any{
				"type": "local", "source": priorSource,
			})
			if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
				t.Fatal(err)
			}
			qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
				"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
			}})
			if err := os.WriteFile(settingsPath, []byte("{\"theme\":\"owner\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(root, "qwen.log")
			fakeQwen := filepath.Join(root, "qwen")
			script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$QWEN_TEST_LOG"
case "$2" in
  uninstall)
    if [ -e "$QWEN_HOME/extensions/agent-sessions" ]; then
      /usr/bin/find "$QWEN_HOME/extensions/agent-sessions" -depth -delete
    fi
    ;;
  install)
    source=$3
    if [ "$source" = "$QWEN_NEW_SOURCE" ]; then
      if [ "$QWEN_FAILURE_MODE" = native ]; then exit 45; fi
      /bin/mkdir -p "$QWEN_HOME/extensions/agent-sessions"
      /bin/cp "$source/plugin.json" "$QWEN_HOME/extensions/agent-sessions/plugin.json"
      printf '{"type":"local","source":"%s"}\n' "$source" >"$QWEN_HOME/extensions/agent-sessions/.qwen-extension-install.json"
      exit 0
    fi
    /bin/mkdir -p "$QWEN_HOME/extensions/agent-sessions"
    /bin/cp -R "$source/." "$QWEN_HOME/extensions/agent-sessions/"
    printf '{"type":"local","source":"%s"}\n' "$source" >"$QWEN_HOME/extensions/agent-sessions/.qwen-extension-install.json"
    ;;
  *) exit 64 ;;
esac
`
			if err := os.WriteFile(fakeQwen, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("QWEN_HOME", qwenHome)
			t.Setenv("QWEN_RUNTIME_DIR", qwenRuntime)
			t.Setenv("QWEN_TEST_LOG", logPath)
			t.Setenv("QWEN_NEW_SOURCE", newSource)
			t.Setenv("QWEN_FAILURE_MODE", failureMode)
			if exit := runQwenPluginInstall([]string{
				"--qwen", fakeQwen, "--plugin-root", newSource, "--version", qwenTestPluginVersion,
			}); exit != 1 {
				t.Fatalf("failed replacement exit = %d, want 1", exit)
			}
			if err := verifyQwenPluginInstallation(installed, qwenTestPluginVersion, true); err != nil {
				t.Fatalf("prior Qwen plugin was not restored: %v", err)
			}
			source, err := installedQwenPluginSource(installed)
			if err != nil || !sameQwenPluginSource(source, priorSource) {
				t.Fatalf("restored Qwen source = %q, %v", source, err)
			}
			if body, err := os.ReadFile(settingsPath); err != nil || string(body) != "{\"theme\":\"owner\"}\n" {
				t.Fatalf("Qwen owner settings changed during rollback: %q, %v", body, err)
			}
			body, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			want := []string{
				"extensions uninstall agent-sessions",
				"extensions install " + newSource + " --scope user --consent",
			}
			if failureMode == "verification" {
				want = append(want, "extensions uninstall agent-sessions")
			}
			want = append(want, "extensions install "+source+" --scope user --consent")
			if !reflect.DeepEqual(lines, want) {
				t.Fatalf("replacement rollback commands = %#v, want %#v", lines, want)
			}
		})
	}
}

func TestSameQwenPluginSourceUsesExistingPathIdentity(t *testing.T) {
	realSource := filepath.Join(t.TempDir(), "real-source")
	if err := os.Mkdir(realSource, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(realSource, alias); err != nil {
		t.Fatal(err)
	}
	if !sameQwenPluginSource(alias, realSource) {
		t.Fatalf("existing source alias %q does not match real source %q", alias, realSource)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if sameQwenPluginSource(missing, missing) {
		t.Fatalf("missing source %q was treated as a usable source identity", missing)
	}
}

func TestQwenPluginRollbackSourcePreservesRecordedSpelling(t *testing.T) {
	realSource := filepath.Join(t.TempDir(), "real-source")
	qwenTestPluginFixtureAt(t, realSource)
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(realSource, alias); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(t.TempDir(), "installed")
	qwenTestPluginFixtureAt(t, installed)
	got, err := qwenPluginRollbackSource(installed, alias, qwenTestPluginVersion)
	if err != nil {
		t.Fatal(err)
	}
	if got != alias {
		t.Fatalf("rollback source = %q, want recorded spelling %q", got, alias)
	}
}

func TestQwenPluginReplacementRefusesUnusableRollbackSourceBeforeMutation(t *testing.T) {
	for _, sourceState := range []string{"missing", "invalid-payload"} {
		t.Run(sourceState, func(t *testing.T) {
			root := t.TempDir()
			qwenHome := filepath.Join(root, "qwen-home")
			installed := filepath.Join(qwenHome, "extensions", qwenPluginName)
			statePath := filepath.Join(qwenHome, "extension-store", "state.json")
			priorSource := filepath.Join(root, "prior-source")
			newSource := filepath.Join(root, "new-source")
			qwenTestPluginFixtureAt(t, installed)
			qwenTestPluginFixtureAt(t, newSource)
			if sourceState == "invalid-payload" {
				qwenTestPluginFixtureAt(t, priorSource)
				if err := os.Remove(filepath.Join(priorSource, "scripts", "native-entry")); err != nil {
					t.Fatal(err)
				}
			}
			qwenTestWriteJSON(t, filepath.Join(installed, ".qwen-extension-install.json"), map[string]any{
				"type": "local", "source": priorSource,
			})
			if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
				t.Fatal(err)
			}
			qwenTestWriteJSON(t, statePath, map[string]any{"version": 2, "extensions": map[string]any{
				"agent-sessions": map[string]any{"name": qwenPluginName, "defaultActivation": "enabled"},
			}})
			marker := filepath.Join(root, "native-called")
			fakeQwen := filepath.Join(root, "qwen")
			if err := os.WriteFile(fakeQwen, []byte("#!/bin/sh\nprintf called >\"$QWEN_NATIVE_MARKER\"\nexit 99\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("QWEN_HOME", qwenHome)
			t.Setenv("QWEN_NATIVE_MARKER", marker)
			if exit := runQwenPluginInstall([]string{
				"--qwen", fakeQwen, "--plugin-root", newSource, "--version", qwenTestPluginVersion,
			}); exit != 1 {
				t.Fatalf("non-rollbackable replacement exit = %d, want 1", exit)
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Fatalf("non-rollbackable replacement invoked native Qwen: %v", err)
			}
			if err := verifyQwenPluginInstallation(installed, qwenTestPluginVersion, true); err != nil {
				t.Fatalf("installed Qwen plugin changed after refused replacement: %v", err)
			}
		})
	}
}

func TestMakefileAggregatesQwenInstallAndUpgradeTargets(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"install-qwen: validate-qwen", "upgrade-qwen: install-qwen", "remove-qwen: build",
		"install-all: build", "\"$(HOST_INSTALLER)\" lifecycle install", "--role host", "--qwen \"$(QWEN)\"",
		"host-package-paths", "dev-install-all: install-all",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("Makefile omits Qwen install aggregation %q", required)
		}
	}
}

func TestInstallAllSkipsOnlyUnavailableNativeProducts(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	commandRoot := filepath.Join(root, "aggregate;make")
	if err := os.Mkdir(commandRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "make.log")
	writeScript := func(name, body string) string {
		t.Helper()
		path := filepath.Join(commandRoot, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fakeInstaller := writeScript("agent-sessions", `printf '%s\n' "$*" >>"$AGGREGATE_MAKE_LOG"`)
	availableCodex := writeScript("codex", "exit 0")
	availableQwen := writeScript("qwen", "exit 0")
	missingGrok := filepath.Join(commandRoot, "missing-grok")

	run := func(name string, arguments ...string) []string {
		t.Helper()
		if err := os.WriteFile(logPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		base := make([]string, 0, 12+len(arguments))
		base = append(base, "-s", "-C", repository, "install-all",
			"BINARY_NAMES=", "BIN_DIR="+commandRoot, "HOST_INSTALLER="+fakeInstaller,
			"INSTALL_ROOT="+filepath.Join(root, "install"), "PREFIX="+filepath.Join(root, "prefix"),
			"GROK="+missingGrok)
		base = append(base, arguments...)
		command := exec.Command("make", base...)
		command.Env = append(os.Environ(), "AGGREGATE_MAKE_LOG="+logPath)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s install-all failed: %v\n%s", name, err, output)
		}
		body, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.FieldsFunc(strings.TrimSpace(string(body)), func(r rune) bool { return r == '\n' })
		return lines
	}

	calls := run("partial", "CODEX="+availableCodex, "CLAUDE=missing-claude-client", "QWEN="+availableQwen)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "lifecycle install --role host ") {
		t.Fatalf("partial install calls = %#v, want one canonical host transaction", calls)
	}
	for _, wanted := range []string{"--codex " + availableCodex, "--claude missing-claude-client", "--grok " + missingGrok, "--qwen " + availableQwen} {
		if !strings.Contains(calls[0], wanted) {
			t.Errorf("partial host transaction omitted %q: %q", wanted, calls[0])
		}
	}

	calls = run("runtime only", "CODEX=missing-codex-client", "CLAUDE=missing-claude-client", "QWEN=missing-qwen-client")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "lifecycle install --role host ") {
		t.Fatalf("runtime-only install calls = %#v, want one optional-product host transaction", calls)
	}
}

func assertQwenPluginPayload(t *testing.T, root string) {
	t.Helper()
	manifest := qwenTestReadJSONObject(t, filepath.Join(root, "plugin.json"))
	if manifest["$schema"] != qwenTestPluginSchema || manifest["name"] != qwenTestPluginName || manifest["version"] != qwenTestPluginVersion {
		t.Fatalf("Qwen plugin manifest identity = %#v", manifest)
	}
	allowedManifestKeys := map[string]bool{
		"$schema": true, "name": true, "version": true, "description": true, "author": true,
		"homepage": true, "repository": true, "license": true, "keywords": true, "extensions": true,
	}
	for key := range manifest {
		if !allowedManifestKeys[key] {
			t.Errorf("plugin.json contains Agent Plugins v1-unsupported key %q", key)
		}
	}

	mcp := qwenTestReadJSONObject(t, filepath.Join(root, "mcp.json"))
	servers, ok := mcp["mcpServers"].(map[string]any)
	if mcp["$schema"] != qwenTestMCPSchema || !ok || len(servers) != 1 {
		t.Fatalf("Qwen MCP manifest = %#v", mcp)
	}
	server, ok := servers[qwenTestMCPName].(map[string]any)
	arguments, argumentsOK := server["args"].([]any)
	if !ok || server["type"] != "stdio" || server["command"] != qwenTestMCPCommand ||
		!argumentsOK || !reflect.DeepEqual(arguments, []any{"connector", "qwen", "mcp"}) {
		t.Fatalf("Qwen %s MCP definition = %#v", qwenTestMCPName, servers[qwenTestMCPName])
	}
	entryInfo, err := os.Lstat(filepath.Join(root, "scripts", "native-entry"))
	if err != nil || !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 || entryInfo.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Qwen MCP native entry = %v, %v", entryInfo, err)
	}

	skillsRoot := filepath.Join(root, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	gotSkills := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Errorf("Qwen skills root contains non-directory %q", entry.Name())
			continue
		}
		info, err := os.Lstat(filepath.Join(skillsRoot, entry.Name(), "SKILL.md"))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("Qwen skill %q has no regular direct-child SKILL.md", entry.Name())
			continue
		}
		gotSkills = append(gotSkills, entry.Name())
	}
	sort.Strings(gotSkills)
	if !reflect.DeepEqual(gotSkills, qwenTestPluginSkills) {
		t.Errorf("Qwen plugin skills = %q, want %q", gotSkills, qwenTestPluginSkills)
	}
}

func qwenTestPluginFixture(t *testing.T, mutate func(string)) string {
	t.Helper()
	root := t.TempDir()
	qwenTestPluginFixtureAt(t, root)
	if mutate != nil {
		mutate(root)
	}
	return root
}

func qwenTestPluginFixtureAt(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	qwenTestWriteJSON(t, filepath.Join(root, "plugin.json"), map[string]any{
		"$schema": qwenTestPluginSchema, "name": qwenTestPluginName, "version": qwenTestPluginVersion,
	})
	qwenTestWriteJSON(t, filepath.Join(root, "mcp.json"), map[string]any{
		"$schema": qwenTestMCPSchema,
		"mcpServers": map[string]any{
			qwenTestMCPName: map[string]any{"type": "stdio", "command": qwenTestMCPCommand, "args": []string{"connector", "qwen", "mcp"}},
		},
	})
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "native-entry"), []byte("#!/bin/sh\nexec /exact/agent-session-runtime \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, skill := range qwenTestPluginSkills {
		qwenTestWriteSkill(t, root, skill)
	}
}

func qwenTestWriteSkill(t *testing.T, root, name string) {
	t.Helper()
	directory := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: Qwen plugin contract fixture.\n---\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func qwenTestWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func qwenTestReadJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

func qwenTestRewriteJSONObject(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	value := qwenTestReadJSONObject(t, path)
	mutate(value)
	qwenTestWriteJSON(t, path, value)
}

func qwenTestProfileIdentity(t *testing.T, values map[string]string) qwenprofile.Identity {
	t.Helper()
	identity, err := qwenprofile.ResolveEnvironment(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func qwenTestEnvironmentValue(environment []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}
