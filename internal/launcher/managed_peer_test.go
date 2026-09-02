package launcher

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManagedPeerPlansUseProductNativeLaunchSurfaces(t *testing.T) {
	root := t.TempDir()
	idSource := func() (string, error) { return "00000000-0000-4000-8000-000000000123", nil }
	tests := []struct {
		product string
		wantArg []string
	}{
		{product: "opencode", wantArg: []string{"--model", "native-model"}},
		{product: "kilo", wantArg: []string{"--model", "native-model"}},
		{product: "pi", wantArg: []string{"--extension", filepath.Join(root, "integrations", "pi", "agent-sessions.mjs"), "--model", "native-model"}},
		{product: "omp", wantArg: []string{"--extension=" + filepath.Join(root, "integrations", "omp", "agent-sessions.mjs"), "--model", "native-model"}},
		{product: "codebuddy", wantArg: []string{
			"--model", "native-model", "--session-id", "00000000-0000-4000-8000-000000000123",
			"--strict-mcp-config", "--mcp-config", filepath.Join(root, "integrations", "codebuddy", "mcp.json"),
		}},
		{product: "dsh", wantArg: []string{"--profile", "agent-sessions", "--model", "native-model"}},
	}
	for _, test := range tests {
		t.Run(test.product, func(t *testing.T) {
			plan, err := buildManagedPeerPlan(test.product, []string{
				"--group", "project", "--peer-name", "reviewer", "--model", "native-model",
			}, []string{"PATH=/bin"}, root, "/native/"+test.product, idSource)
			if err != nil {
				t.Fatal(err)
			}
			if plan.path != "/native/"+test.product || !reflect.DeepEqual(plan.args, test.wantArg) {
				t.Fatalf("plan = path %q args %#v, want path %q args %#v", plan.path, plan.args, "/native/"+test.product, test.wantArg)
			}
			if got := environmentValue(plan.environment, peerProductEnv); got != test.product {
				t.Fatalf("AGENT_SESSIONS_PRODUCT = %q", got)
			}
			if got := environmentValue(plan.environment, "AGENT_SESSIONS_PRODUCT_ID"); got != test.product {
				t.Fatalf("AGENT_SESSIONS_PRODUCT_ID = %q", got)
			}
			if got := environmentValue(plan.environment, peerSessionNameEnv); got != "reviewer" {
				t.Fatalf("AGENT_SESSIONS_SESSION_NAME = %q", got)
			}
			var groups []string
			if err := json.Unmarshal([]byte(environmentValue(plan.environment, peerGroupsEnv)), &groups); err != nil || !reflect.DeepEqual(groups, []string{"project"}) {
				t.Fatalf("AGENT_SESSIONS_GROUPS = %#v, %v", groups, err)
			}
			if test.product == "codebuddy" && environmentValue(plan.environment, peerSessionIDEnv) != "00000000-0000-4000-8000-000000000123" {
				t.Fatalf("CodeBuddy session id was not shared with its MCP connector")
			}
		})
	}
}

func TestManagedPeerPlanPreservesProductOwnedResumeSelectors(t *testing.T) {
	root := t.TempDir()
	idSource := func() (string, error) { return "new-id", nil }
	for _, test := range []struct {
		product string
		args    []string
	}{
		{product: "opencode", args: []string{"--session", "ses_exact"}},
		{product: "kilo", args: []string{"--session", "ses_exact"}},
		{product: "pi", args: []string{"--session", "native-exact"}},
		{product: "omp", args: []string{"--session", "native-exact"}},
		{product: "codebuddy", args: []string{"--session-id", "native-exact"}},
		{product: "dsh", args: []string{"--profile", "custom"}},
	} {
		t.Run(test.product, func(t *testing.T) {
			plan, err := buildManagedPeerPlan(test.product, test.args, nil, root, "/native/"+test.product, idSource)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan.args, "\x00")
			for _, value := range test.args {
				if !strings.Contains(joined, value) {
					t.Fatalf("native selector %q was not preserved in %#v", value, plan.args)
				}
			}
			if test.product == "codebuddy" && strings.Count(joined, "--session-id") != 1 {
				t.Fatalf("CodeBuddy exact selector was duplicated: %#v", plan.args)
			}
			if test.product == "dsh" && strings.Count(joined, "--profile") != 1 {
				t.Fatalf("DSH exact profile selector was duplicated: %#v", plan.args)
			}
		})
	}
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}
