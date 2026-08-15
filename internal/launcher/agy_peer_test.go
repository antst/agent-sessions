package launcher

import (
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

func TestAgyExecutablePrefersHeadlessUserCLIOverPath(t *testing.T) {
	home := t.TempDir()
	userCLI := filepath.Join(home, ".local", "bin", "agy")
	writeAgyExecutable(t, userCLI)
	pathCLI := filepath.Join(t.TempDir(), "agy")
	writeAgyExecutable(t, pathCLI)
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(pathCLI))
	t.Setenv("AGY_PEER_AGY_BIN", "")

	got, err := agyExecutable()
	if err != nil || got != userCLI {
		t.Fatalf("agy executable = %q, %v; want %q", got, err, userCLI)
	}
}

func TestAgyExecutableRejectsAntigravityGUIHelper(t *testing.T) {
	home := t.TempDir()
	gui := filepath.Join(home, ".antigravity", "antigravity", "bin", "agy")
	writeAgyExecutable(t, gui)
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(gui))
	t.Setenv("AGY_PEER_AGY_BIN", "")

	got, err := agyExecutable()
	if err == nil || got != "" || !strings.Contains(err.Error(), "GUI helper") || !strings.Contains(err.Error(), "AGY_PEER_AGY_BIN") {
		t.Fatalf("GUI agy resolution = %q, %v", got, err)
	}
}

func writeAgyExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}
