package launcher

import (
	"reflect"
	"testing"
)

func TestPeerLaunchContextSeparatesGroupLayerFromCodexTargetArgs(t *testing.T) {
	forwarded, context, err := scanPeerWrapperOptions("codex", []string{
		"-g", "project", "-g=review", "--group", "release", "--group=docs", "--inherit-groups",
		"resume", "thread-id", "--model", "gpt-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"resume", "thread-id", "--model", "gpt-test"}
	if !reflect.DeepEqual(forwarded, want) {
		t.Fatalf("target argv = %q, want %q", forwarded, want)
	}
	if !reflect.DeepEqual(context.groups, []string{"project", "review", "release", "docs"}) || !context.groupsSpecified ||
		context.parentSession != "" || context.parentSpecified ||
		!context.inheritParentGroups || !context.inheritGroupsSpecified {
		t.Fatalf("parent context = %+v", context)
	}
	if _, _, err := scanPeerWrapperOptions("codex", []string{"--parent-session", "someone-else"}); err == nil {
		t.Fatal("public peer launch accepted a caller-selected parent")
	}
}

func TestPeerLaunchContextDoesNotInterpretNativeOptionValue(t *testing.T) {
	forwarded, context, err := scanPeerWrapperOptions("codex", []string{
		"--model", "-g", "-g", "actual", "--no-inherit-groups", "--no-yolo",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "-g"}
	if !reflect.DeepEqual(forwarded, want) || !reflect.DeepEqual(context.groups, []string{"actual"}) ||
		context.inheritParentGroups || !context.inheritGroupsSpecified || !context.forceNoYolo {
		t.Fatalf("forwarded=%q context=%+v", forwarded, context)
	}
}

func TestPeerLaunchContextShortGroupAliasAppliesToEveryProduct(t *testing.T) {
	products := []string{"codex", "claude", "grok", "qwen"}
	for _, product := range products {
		t.Run(product, func(t *testing.T) {
			forwarded, context, err := scanPeerWrapperOptions(product, []string{
				"-g", "project", "-g=review", "--", "prompt", "-g", "not-a-group",
			})
			if err != nil {
				t.Fatal(err)
			}
			wantForwarded := []string{"--", "prompt", "-g", "not-a-group"}
			if !reflect.DeepEqual(forwarded, wantForwarded) ||
				!reflect.DeepEqual(context.groups, []string{"project", "review"}) || !context.groupsSpecified {
				t.Fatalf("forwarded=%q context=%+v", forwarded, context)
			}
		})
	}
}

func TestPersistentRuntimeEnvironmentDropsTransientParentIdentity(t *testing.T) {
	got := persistentRuntimeEnvironment([]string{
		"KEEP=value",
		peerSessionIDEnv + "=parent",
		peerProductEnv + "=grok",
		peerSessionNameEnv + "=worker",
		peerGroupsEnv + `=["team"]`,
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

func TestLiveReportEnvironmentCarriesNameAndGroupsOnlyInChildEnvironment(t *testing.T) {
	got := liveReportEnvironment([]string{"KEEP=value"}, "worker", []string{"team", "review"})
	want := []string{"KEEP=value", peerSessionNameEnv + "=worker", peerGroupsEnv + `=["team","review"]`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("live report environment = %q, want %q", got, want)
	}
}
