package launcher

import (
	"reflect"
	"testing"
)

func TestPeerLaunchContextSeparatesParentLayerFromCodexTargetArgs(t *testing.T) {
	forwarded, context, err := extractPeerLaunchContext([]string{
		"--group", "project", "--group=review", "--parent-session", "parent-id", "--inherit-groups",
		"resume", "thread-id", "--model", "gpt-test",
	}, codexOptionConsumesNext)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"resume", "thread-id", "--model", "gpt-test"}
	if !reflect.DeepEqual(forwarded, want) {
		t.Fatalf("target argv = %q, want %q", forwarded, want)
	}
	if !reflect.DeepEqual(context.groups, []string{"project", "review"}) || !context.groupsSpecified ||
		context.parentSession != "parent-id" || !context.parentSpecified ||
		!context.inheritParentGroups || !context.inheritGroupsSpecified {
		t.Fatalf("parent context = %+v", context)
	}
}

func TestPeerLaunchContextDoesNotInterpretNativeOptionValue(t *testing.T) {
	forwarded, context, err := extractPeerLaunchContext([]string{
		"--model", "--group", "--group", "actual", "--no-inherit-groups", "--no-yolo",
	}, codexOptionConsumesNext)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "--group"}
	if !reflect.DeepEqual(forwarded, want) || !reflect.DeepEqual(context.groups, []string{"actual"}) ||
		context.inheritParentGroups || !context.inheritGroupsSpecified || !context.forceNoYolo {
		t.Fatalf("forwarded=%q context=%+v", forwarded, context)
	}
}

func TestPersistentRuntimeEnvironmentDropsTransientParentIdentity(t *testing.T) {
	got := persistentRuntimeEnvironment([]string{
		"KEEP=value",
		peerSessionIDEnv + "=parent",
		peerProductEnv + "=grok",
		remoteParentEnv + "={}",
		"CODEX_THREAD_ID=thread",
		grokLaunchTokenEnv + "=secret",
		grokSessionIDEnv + "=native",
		agentRuntimeDirEnv + "=/runtime",
	})
	want := []string{"KEEP=value", agentRuntimeDirEnv + "=/runtime"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persistent environment = %q, want %q", got, want)
	}
}
