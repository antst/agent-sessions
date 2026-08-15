package launcher

import (
	"reflect"
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
