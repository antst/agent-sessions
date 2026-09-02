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

func TestGlobalPluginPeersHaveNoLaunchTimePayloadDependency(t *testing.T) {
	for _, product := range []string{"opencode", "kilo"} {
		t.Run(product, func(t *testing.T) {
			plan, err := buildManagedPeerPlan(product, []string{"--model", "native-model"}, nil, "", "/native/"+product, nil)
			if err != nil {
				t.Fatal(err)
			}
			if plan.path != "/native/"+product || !reflect.DeepEqual(plan.args, []string{"--model", "native-model"}) {
				t.Fatalf("plan = path %q args %#v", plan.path, plan.args)
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
			if (test.product == "opencode" || test.product == "kilo") && environmentValue(plan.environment, peerSessionIDEnv) != "ses_exact" {
				t.Fatalf("%s exact resume id was not shared with its native plugin", test.product)
			}
			if test.product == "dsh" && strings.Count(joined, "--profile") != 1 {
				t.Fatalf("DSH exact profile selector was duplicated: %#v", plan.args)
			}
		})
	}
}

func TestResolveOpenCodeResumeUsesProductOwnedSessionList(t *testing.T) {
	sessions := []openCodeSession{
		{ID: "ses_one", Title: "review", Directory: "/one", Updated: 10},
		{ID: "ses_two", Title: "other", Directory: "/two", Updated: 20},
	}
	listCalls := 0
	list := func(executable string) ([]openCodeSession, error) {
		listCalls++
		if executable != "/native/opencode" {
			t.Fatalf("executable = %q", executable)
		}
		return sessions, nil
	}
	resolved, err := resolveOpenCodeResume("/native/opencode", []string{"--session", "review", "--model", "native"}, list)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, []string{"--session", "ses_one", "--model", "native"}) || listCalls != 1 {
		t.Fatalf("resolved = %#v, list calls = %d", resolved, listCalls)
	}
	resolved, err = resolveOpenCodeResume("/native/opencode", []string{"--session=review"}, list)
	if err != nil || !reflect.DeepEqual(resolved, []string{"--session=ses_one"}) {
		t.Fatalf("equals-form resolved = %#v, err = %v", resolved, err)
	}
	resolved, err = resolveOpenCodeResume("/native/opencode", []string{"--session", "ses_exact"}, list)
	if err != nil || !reflect.DeepEqual(resolved, []string{"--session", "ses_exact"}) || listCalls != 2 {
		t.Fatalf("exact-id resolved = %#v, list calls = %d, err = %v", resolved, listCalls, err)
	}
}

func TestResolveOpenCodeResumeRejectsMissingAndAmbiguousProductNames(t *testing.T) {
	list := func(string) ([]openCodeSession, error) {
		return []openCodeSession{
			{ID: "ses_one", Title: "duplicate", Directory: "/one", Updated: 10},
			{ID: "ses_two", Title: "duplicate", Directory: "/two", Updated: 20},
		}, nil
	}
	if _, err := resolveOpenCodeResume("opencode", []string{"--session", "missing"}, list); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing-name error = %v", err)
	}
	if _, err := resolveOpenCodeResume("opencode", []string{"--session", "duplicate"}, list); err == nil ||
		!strings.Contains(err.Error(), "ses_one (directory=/one updated=10)") ||
		!strings.Contains(err.Error(), "ses_two (directory=/two updated=20)") {
		t.Fatalf("ambiguous-name error = %v", err)
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
