package launcher

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
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
		{product: "pi", wantArg: []string{"--extension", filepath.Join(root, "integrations", "pi", "agent-sessions.mjs"), "--model", "native-model", "--approve"}},
		{product: "omp", wantArg: []string{"--extension=" + filepath.Join(root, "integrations", "omp", "agent-sessions.mjs"), "--model", "native-model"}},
		{product: "codebuddy", wantArg: []string{
			"--model", "native-model", "--session-id", "00000000-0000-4000-8000-000000000123",
			"--strict-mcp-config", "--mcp-config", filepath.Join(root, "integrations", "codebuddy", "mcp.json"),
		}},
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

func TestManagedLaunchPolicyComesOnlyFromTheProductDescriptor(t *testing.T) {
	descriptor, _ := productcatalog.ByID("pi")
	for _, test := range []struct {
		name string
		args []string
		no   bool
		want []string
	}{
		{name: "grant", args: []string{"--model", "native"}, want: []string{"--model", "native", "--approve"}},
		{name: "yolo", args: []string{"--yolo", "--model", "native"}, want: []string{"--approve", "--model", "native"}},
		{name: "native grant deduplicated", args: []string{"--approve"}, want: []string{"--approve"}},
		{name: "prompt boundary", args: []string{"--", "--yolo"}, want: []string{"--approve", "--", "--yolo"}},
		{name: "no yolo keeps tool grant", args: []string{"--model", "native"}, no: true, want: []string{"--model", "native", "--approve"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := projectNativeLaunchPolicy(descriptor, test.args, test.no)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("projected args = %#v, want %#v", got, test.want)
			}
		})
	}
	if _, err := projectNativeLaunchPolicy(descriptor, []string{"--yolo"}, true); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting wrapper policy error = %v", err)
	}
	opencode, _ := productcatalog.ByID("opencode")
	if _, err := projectNativeLaunchPolicy(opencode, []string{"--yolo"}, false); err == nil || !strings.Contains(err.Error(), "not mapped") {
		t.Fatalf("unprobed product yolo error = %v", err)
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
		{product: "omp", args: []string{"--resume", "native-exact"}},
		{product: "codebuddy", args: []string{"--session-id", "native-exact"}},
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
		})
	}
}

func TestManagedPeerRejectsLaneOnlyProduct(t *testing.T) {
	if _, err := buildManagedPeerPlan("dsh", nil, nil, "", "/native/dsh", nil); err == nil || !strings.Contains(err.Error(), "unsupported managed peer") {
		t.Fatalf("DSH peer plan error = %v", err)
	}
}

func TestOMPProjectsOnlyItsRealProbedYoloMapping(t *testing.T) {
	descriptor, ok := productcatalog.ByID("omp")
	if !ok {
		t.Fatal("OMP descriptor missing")
	}
	projected, err := projectNativeLaunchPolicy(descriptor, []string{"--model", "deepseek", "--yolo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "deepseek", "--approval-mode", "yolo"}
	if !reflect.DeepEqual(projected, want) {
		t.Fatalf("projected = %#v, want %#v", projected, want)
	}
}

func TestManagedGlobalPluginPeersResolveNamesFromTheirProductSessionList(t *testing.T) {
	sessions := []productSession{
		{ID: "ses_one", Title: "review", Directory: "/one", Updated: 10},
		{ID: "ses_two", Title: "other", Directory: "/two", Updated: 20},
	}
	for _, product := range []string{"opencode", "kilo"} {
		t.Run(product, func(t *testing.T) {
			listCalls := 0
			list := func(executable, cwd string) ([]productSession, error) {
				listCalls++
				if executable != "/native/"+product {
					t.Fatalf("executable = %q", executable)
				}
				if cwd != "" {
					t.Fatalf("cwd = %q", cwd)
				}
				return sessions, nil
			}
			resolved, err := resolveProductResume(product, "/native/"+product, "", []string{"--session", "review", "--model", "native"}, "--session", isOpenCodeSessionID, list)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolved, []string{"--session", "ses_one", "--model", "native"}) || listCalls != 1 {
				t.Fatalf("resolved = %#v, list calls = %d", resolved, listCalls)
			}
			resolved, err = resolveProductResume(product, "/native/"+product, "", []string{"--session=review"}, "--session", isOpenCodeSessionID, list)
			if err != nil || !reflect.DeepEqual(resolved, []string{"--session=ses_one"}) {
				t.Fatalf("equals-form resolved = %#v, err = %v", resolved, err)
			}
			resolved, err = resolveProductResume(product, "/native/"+product, "", []string{"--session", "ses_exact"}, "--session", isOpenCodeSessionID, list)
			if err != nil || !reflect.DeepEqual(resolved, []string{"--session", "ses_exact"}) || listCalls != 2 {
				t.Fatalf("exact-id resolved = %#v, list calls = %d, err = %v", resolved, listCalls, err)
			}
		})
	}
}

func TestResolveProductResumeRejectsMissingAndAmbiguousProductNames(t *testing.T) {
	list := func(string, string) ([]productSession, error) {
		return []productSession{
			{ID: "ses_one", Title: "duplicate", Directory: "/one", Updated: 10},
			{ID: "ses_two", Title: "duplicate", Directory: "/two", Updated: 20},
		}, nil
	}
	if _, err := resolveProductResume("opencode", "opencode", "/work", []string{"--session", "missing"}, "--session", isOpenCodeSessionID, list); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing-name error = %v", err)
	}
	if _, err := resolveProductResume("opencode", "opencode", "/work", []string{"--session", "duplicate"}, "--session", isOpenCodeSessionID, list); err == nil ||
		!strings.Contains(err.Error(), "ses_one (directory=/one updated=10)") ||
		!strings.Contains(err.Error(), "ses_two (directory=/two updated=20)") {
		t.Fatalf("ambiguous-name error = %v", err)
	}
}

func TestPiResumeNamesUseTheProductsPublicSessionList(t *testing.T) {
	sessions := []productSession{
		{ID: "01a06232-23d2-75b4-ba75-c23bbae61751", Title: "e2e-pi", Directory: "/work", Modified: "2026-09-02T13:00:04.763Z"},
	}
	listCalls := 0
	list := func(executable, cwd string) ([]productSession, error) {
		listCalls++
		if executable != "/native/pi" || cwd != "/work" {
			t.Fatalf("list request = %q %q", executable, cwd)
		}
		return sessions, nil
	}
	resolved, err := resolveProductResume("pi", "/native/pi", "/work", []string{"--session", "e2e-pi"}, "--session", isPiNativeSessionSelector, list)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, []string{"--session", sessions[0].ID}) || listCalls != 1 {
		t.Fatalf("resolved = %#v, calls = %d", resolved, listCalls)
	}
	for _, selector := range []string{sessions[0].ID, "/tmp/pi-session.jsonl", "relative/session.jsonl"} {
		resolved, err = resolveProductResume("pi", "/native/pi", "/work", []string{"--session", selector}, "--session", isPiNativeSessionSelector, list)
		if err != nil || !reflect.DeepEqual(resolved, []string{"--session", selector}) {
			t.Fatalf("native selector %q resolved to %#v, err %v", selector, resolved, err)
		}
	}
	if listCalls != 1 {
		t.Fatalf("native selectors queried the product list: %d calls", listCalls)
	}
}

func TestPiAmbiguousNameListsOnlyProductProvidedIdentity(t *testing.T) {
	list := func(string, string) ([]productSession, error) {
		return []productSession{
			{ID: "00000000-0000-4000-8000-000000000001", Title: "duplicate", Directory: "/one", Modified: "first"},
			{ID: "00000000-0000-4000-8000-000000000002", Title: "duplicate", Directory: "/two", Modified: "second"},
		}, nil
	}
	_, err := resolveProductResume("pi", "pi", "/work", []string{"--session", "duplicate"}, "--session", isPiNativeSessionSelector, list)
	if err == nil || !strings.Contains(err.Error(), "00000000-0000-4000-8000-000000000001 (directory=/one updated=first)") ||
		!strings.Contains(err.Error(), "00000000-0000-4000-8000-000000000002 (directory=/two updated=second)") {
		t.Fatalf("ambiguous Pi name error = %v", err)
	}
}

func TestOMPResumeNamesUseTheProductsPublicSessionList(t *testing.T) {
	sessions := []productSession{
		{ID: "01a0624c-5de8-70dd-9247-c51adbcb56e4", Title: "e2e-omp2", Directory: "/work", Modified: "2026-09-02T13:30:03.476Z"},
	}
	listCalls := 0
	list := func(executable, cwd string) ([]productSession, error) {
		listCalls++
		if executable != "/native/omp" || cwd != "/work" {
			t.Fatalf("list request = %q %q", executable, cwd)
		}
		return sessions, nil
	}
	resolved, err := resolveProductResume("omp", "/native/omp", "/work", []string{"--resume", "e2e-omp2"}, "--resume", isPiNativeSessionSelector, list)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, []string{"--resume", sessions[0].ID}) || listCalls != 1 {
		t.Fatalf("resolved = %#v, calls = %d", resolved, listCalls)
	}
	for _, selector := range []string{sessions[0].ID, "/tmp/omp-session.jsonl", "relative/session.jsonl"} {
		resolved, err = resolveProductResume("omp", "/native/omp", "/work", []string{"--resume", selector}, "--resume", isPiNativeSessionSelector, list)
		if err != nil || !reflect.DeepEqual(resolved, []string{"--resume", selector}) {
			t.Fatalf("native selector %q resolved to %#v, err %v", selector, resolved, err)
		}
	}
	if listCalls != 1 {
		t.Fatalf("native selectors queried the product list: %d calls", listCalls)
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
