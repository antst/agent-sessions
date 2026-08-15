package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgyPeerPreservesNativeArgumentsAndExtractsOnlyPeerName(t *testing.T) {
	input := []string{"--model", "gemini-3.7-pro", "-n", "agy-review", "--conversation", "conversation-id", "--dangerously-skip-permissions", "--", "literal"}
	plan, err := parseAgyPeerArgs(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "gemini-3.7-pro", "--conversation", "conversation-id", "--dangerously-skip-permissions", "--", "literal"}
	if !reflect.DeepEqual(plan.forwarded, want) {
		t.Fatalf("forwarded args = %#v, want %#v", plan.forwarded, want)
	}
	if plan.peerName != "agy-review" || plan.permissionMode != "bypassPermissions" || plan.passthroughOnly {
		t.Fatalf("unexpected plan: %#v", plan)
	}
}

func TestAgyPeerRecognizesNativeBypassFlagSpellings(t *testing.T) {
	for _, argument := range []string{
		"--dangerously-skip-permissions",
		"-dangerously-skip-permissions",
		"--dangerously-skip-permissions=true",
		"-dangerously-skip-permissions=true",
	} {
		plan, err := parseAgyPeerArgs([]string{argument})
		if err != nil || plan.permissionMode != "bypassPermissions" {
			t.Fatalf("bypass spelling %q = %#v, %v", argument, plan, err)
		}
	}
	for _, argument := range []string{"--dangerously-skip-permissions=false", "-dangerously-skip-permissions=false"} {
		plan, err := parseAgyPeerArgs([]string{argument})
		if err != nil || plan.permissionMode != "default" {
			t.Fatalf("disabled bypass spelling %q = %#v, %v", argument, plan, err)
		}
	}
}

func TestAgyPeerPassesNativeSubcommandsThrough(t *testing.T) {
	for _, args := range [][]string{{"models", "list"}, {"plugin", "list"}, {"--help"}, {"--model", "x", "agents"}} {
		plan, err := parseAgyPeerArgs(args)
		if err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		if !plan.passthroughOnly {
			t.Fatalf("%q was not classified as passthrough", args)
		}
	}
}

func TestAgyPeerDoesNotMistakePromptTextForSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"--print", "models"}, {"--prompt", "plugin"}, {"--prompt-interactive", "agents"},
		{"--print=models"}, {"-pmodels"}, {"-iagents"},
	} {
		plan, err := parseAgyPeerArgs(args)
		if err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		if plan.passthroughOnly {
			t.Fatalf("prompt invocation %q was classified as passthrough", args)
		}
	}
}

func TestAgyPeerRejectsNameOnPassthrough(t *testing.T) {
	if _, err := parseAgyPeerArgs([]string{"-n", "wrong", "models"}); err == nil {
		t.Fatal("named passthrough was accepted")
	}
}

func TestReplaceEnvironmentRemovesInheritedLaunchToken(t *testing.T) {
	got := replaceEnvironment([]string{"A=1", agyLaunchTokenEnv + "=old", "B=2"}, agyLaunchTokenEnv, "new")
	want := []string{"A=1", "B=2", agyLaunchTokenEnv + "=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func TestPrependEnvironmentPathMakesNativeRuntimeDiscoverable(t *testing.T) {
	runtimeDirectory := filepath.Join(t.TempDir(), "runtime")
	originalPath := strings.Join([]string{"/usr/local/bin", "/usr/bin"}, string(os.PathListSeparator))
	got := prependEnvironmentPath([]string{"A=1", "PATH=" + originalPath}, runtimeDirectory)
	want := []string{"A=1", "PATH=" + runtimeDirectory + string(os.PathListSeparator) + originalPath}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
	if duplicate := prependEnvironmentPath(got, runtimeDirectory); !reflect.DeepEqual(duplicate, want) {
		t.Fatalf("duplicate prepend = %#v, want unchanged %#v", duplicate, want)
	}
}

func TestAgyExecutableSkipsGUIShimBeforeHeadlessCLI(t *testing.T) {
	home := t.TempDir()
	guiDirectory := filepath.Join(t.TempDir(), ".antigravity", "antigravity", "bin")
	cliDirectory := t.TempDir()
	guiMarker := filepath.Join(t.TempDir(), "gui-invoked")
	writeAgyFixture(t, guiDirectory, `: >"$GUI_MARKER"; printf '%s\n' 'Antigravity 1.107.0' 'Usage: antigravity [options]'`)
	cli := writeAgyFixture(t, cliDirectory, agyHelpFixture)
	t.Setenv("AGY_PEER_AGY_BIN", "")
	t.Setenv("HOME", home)
	t.Setenv("GUI_MARKER", guiMarker)
	t.Setenv("PATH", guiDirectory+string(os.PathListSeparator)+cliDirectory)

	got, err := agyExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if got != cli {
		t.Fatalf("agy executable = %q, want headless CLI %q", got, cli)
	}
	if _, err := os.Stat(guiMarker); !os.IsNotExist(err) {
		t.Fatalf("GUI helper was executed during CLI resolution: %v", err)
	}
}

func TestAgyExecutableRejectsGUIOverride(t *testing.T) {
	guiDirectory := filepath.Join(t.TempDir(), ".antigravity", "antigravity", "bin")
	guiMarker := filepath.Join(t.TempDir(), "gui-invoked")
	gui := writeAgyFixture(t, guiDirectory, `: >"$GUI_MARKER"; printf '%s\n' 'Antigravity 1.107.0' 'Usage: antigravity [options]'`)
	t.Setenv("AGY_PEER_AGY_BIN", gui)
	t.Setenv("GUI_MARKER", guiMarker)

	_, err := agyExecutable()
	if err == nil {
		t.Fatal("GUI override was accepted as the headless Antigravity CLI")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 127 || !strings.Contains(err.Error(), "not the headless Antigravity CLI") {
		t.Fatalf("GUI override error = %v, want exit 127 with CLI diagnostic", err)
	}
	if _, statErr := os.Stat(guiMarker); !os.IsNotExist(statErr) {
		t.Fatalf("GUI override was executed during validation: %v", statErr)
	}
}

func TestAgyExecutableRejectsUserLocalSymlinkToGUI(t *testing.T) {
	home := t.TempDir()
	guiDirectory := filepath.Join(t.TempDir(), "Antigravity.app", "Contents", "MacOS")
	guiMarker := filepath.Join(t.TempDir(), "gui-invoked")
	gui := writeAgyFixture(t, guiDirectory, `: >"$GUI_MARKER"; printf '%s\n' 'Antigravity GUI'`)
	userDirectory := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	userCLI := filepath.Join(userDirectory, "agy")
	if err := os.Symlink(gui, userCLI); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_PEER_AGY_BIN", "")
	t.Setenv("HOME", home)
	t.Setenv("GUI_MARKER", guiMarker)
	t.Setenv("PATH", userDirectory)

	_, err := agyExecutable()
	if err == nil || !strings.Contains(err.Error(), "no headless Antigravity CLI") {
		t.Fatalf("GUI-backed user-local agy error = %v, want fail-closed CLI diagnostic", err)
	}
	if _, statErr := os.Stat(guiMarker); !os.IsNotExist(statErr) {
		t.Fatalf("GUI-backed user-local agy was executed during validation: %v", statErr)
	}
}

func TestAgyExecutableRejectsUserLocalSymlinkToLinuxGUIHelper(t *testing.T) {
	home := t.TempDir()
	guiDirectory := filepath.Join(home, ".antigravity", "antigravity", "bin")
	guiMarker := filepath.Join(t.TempDir(), "gui-invoked")
	gui := writeAgyFixture(t, guiDirectory, `: >"$GUI_MARKER"; printf '%s\n' 'Antigravity GUI'`)
	userDirectory := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	userCLI := filepath.Join(userDirectory, "agy")
	if err := os.Symlink(gui, userCLI); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_PEER_AGY_BIN", "")
	t.Setenv("HOME", home)
	t.Setenv("GUI_MARKER", guiMarker)
	t.Setenv("PATH", userDirectory)

	_, err := agyExecutable()
	if err == nil || !strings.Contains(err.Error(), "no headless Antigravity CLI") {
		t.Fatalf("Linux GUI-backed user-local agy error = %v, want fail-closed CLI diagnostic", err)
	}
	if _, statErr := os.Stat(guiMarker); !os.IsNotExist(statErr) {
		t.Fatalf("Linux GUI-backed user-local agy was executed during validation: %v", statErr)
	}
}

func TestAgyExecutableUsesPathBeforeUserLocalFallback(t *testing.T) {
	home := t.TempDir()
	userCLI := writeAgyFixture(t, filepath.Join(home, ".local", "bin"), agyHelpFixture)
	pathCLI := writeAgyFixture(t, t.TempDir(), agyHelpFixture)
	t.Setenv("AGY_PEER_AGY_BIN", "")
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(pathCLI))

	got, err := agyExecutable()
	if err != nil || got != pathCLI || got == userCLI {
		t.Fatalf("agy executable = %q, %v; want PATH CLI %q", got, err, pathCLI)
	}
}

const agyHelpFixture = `printf '%s\n' 'Usage of agy:' '  --conversation' '  --prompt-interactive' '  --output-format' 'Available subcommands:'`

func writeAgyFixture(t *testing.T, directory, body string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
