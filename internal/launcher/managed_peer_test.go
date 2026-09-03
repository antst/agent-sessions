package launcher

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestManagedPeerPlansUseProductNativeLaunchSurfaces(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		product string
		wantArg []string
	}{
		{product: "opencode", wantArg: []string{"--model", "native-model"}},
		{product: "kilo", wantArg: []string{"--model", "native-model"}},
		{product: "pi", wantArg: []string{"--extension", filepath.Join(root, "integrations", "pi", "agent-sessions.mjs"), "--model", "native-model", "--approve"}},
		{product: "omp", wantArg: []string{"--extension=" + filepath.Join(root, "integrations", "omp", "agent-sessions.mjs"), "--model", "native-model"}},
	}
	for _, test := range tests {
		t.Run(test.product, func(t *testing.T) {
			asset := ""
			if test.product == "pi" || test.product == "omp" {
				asset = filepath.Join(root, "integrations", test.product, "agent-sessions.mjs")
			}
			plan, err := buildManagedPeerPlan(test.product, []string{
				"--group", "project", "--peer-name", "reviewer", "--model", "native-model",
			}, []string{"PATH=/bin"}, asset, "/native/"+test.product)
			if err != nil {
				t.Fatal(err)
			}
			if plan.path != "/native/"+test.product || !reflect.DeepEqual(plan.args, test.wantArg) {
				t.Fatalf("plan = path %q args %#v, want path %q args %#v", plan.path, plan.args, "/native/"+test.product, test.wantArg)
			}
			if got := environmentValue(plan.environment, peerProductEnv); got != test.product {
				t.Fatalf("AGENT_SESSIONS_PRODUCT = %q", got)
			}
			if got := environmentValue(plan.environment, "AGENT_SESSIONS_PRODUCT_ID"); got != "" {
				t.Fatalf("obsolete AGENT_SESSIONS_PRODUCT_ID = %q", got)
			}
			if got := environmentValue(plan.environment, peerSessionNameEnv); got != "reviewer" {
				t.Fatalf("AGENT_SESSIONS_SESSION_NAME = %q", got)
			}
			if _, present := envutil.Lookup(plan.environment)(peerSessionIDEnv); present {
				t.Fatal("fresh product-generated session exported AGENT_SESSIONS_SESSION_ID")
			}
			var groups []string
			if err := json.Unmarshal([]byte(environmentValue(plan.environment, peerGroupsEnv)), &groups); err != nil || !reflect.DeepEqual(groups, []string{"project"}) {
				t.Fatalf("AGENT_SESSIONS_GROUPS = %#v, %v", groups, err)
			}
		})
	}
}

func TestManagedIntegrationAssetIsThePeerPlanPayload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_SESSIONS_PLUGIN_ROOT", root)
	asset, err := ManagedIntegrationAsset("pi", "agent-sessions.mjs")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "integrations", "pi", "agent-sessions.mjs")
	if asset != want {
		t.Fatalf("managed asset = %q, want %q", asset, want)
	}
	plan, err := buildManagedPeerPlan("pi", nil, nil, asset, "/native/pi")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.args[:2], []string{"--extension", asset}) {
		t.Fatalf("peer extension args = %#v", plan.args)
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
	unmapped := descriptor
	unmapped.ID = "unprobed"
	unmapped.NativeYoloArgs = nil
	if _, err := projectNativeLaunchPolicy(unmapped, []string{"--yolo"}, false); err == nil || !strings.Contains(err.Error(), "not mapped") {
		t.Fatalf("unprobed product yolo error = %v", err)
	}
}

func TestGlobalPluginPeersHaveNoLaunchTimePayloadDependency(t *testing.T) {
	for _, product := range []string{"opencode", "kilo"} {
		t.Run(product, func(t *testing.T) {
			plan, err := buildManagedPeerPlan(product, []string{"--yolo", "--model", "native-model"}, nil, "", "/native/"+product)
			if err != nil {
				t.Fatal(err)
			}
			if plan.path != "/native/"+product || !reflect.DeepEqual(plan.args, []string{"--yolo", "--model", "native-model"}) {
				t.Fatalf("plan = path %q args %#v", plan.path, plan.args)
			}
		})
	}
}

func TestManagedPeerPlanPreservesProductOwnedResumeSelectors(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		product string
		args    []string
	}{
		{product: "opencode", args: []string{"--session", "ses_exact"}},
		{product: "kilo", args: []string{"--session", "ses_exact"}},
		{product: "pi", args: []string{"--session", "native-exact"}},
		{product: "omp", args: []string{"--resume", "native-exact"}},
	} {
		t.Run(test.product, func(t *testing.T) {
			asset := ""
			if test.product == "pi" || test.product == "omp" {
				asset = filepath.Join(root, "integrations", test.product, "agent-sessions.mjs")
			}
			plan, err := buildManagedPeerPlan(test.product, test.args, nil, asset, "/native/"+test.product)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan.args, "\x00")
			for _, value := range test.args {
				if !strings.Contains(joined, value) {
					t.Fatalf("native selector %q was not preserved in %#v", value, plan.args)
				}
			}
			if (test.product == "opencode" || test.product == "kilo") && environmentValue(plan.environment, peerSessionIDEnv) != "ses_exact" {
				t.Fatalf("%s exact resume id was not shared with its native plugin", test.product)
			}
		})
	}
}

func TestManagedPeerRejectsLaneOnlyProduct(t *testing.T) {
	if _, err := buildManagedPeerPlan("dsh", nil, nil, "", "/native/dsh"); err == nil || !strings.Contains(err.Error(), "unsupported managed peer") {
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
	want := []string{"--model", "deepseek", "--yolo"}
	if !reflect.DeepEqual(projected, want) {
		t.Fatalf("projected = %#v, want %#v", projected, want)
	}
}

func TestTranslateNativeOptionPreservesFormAndDoubleDashBoundary(t *testing.T) {
	for _, test := range []struct {
		name        string
		arguments   []string
		replacement []string
		want        []string
	}{
		{name: "split", arguments: []string{"--resume", "review", "--model", "native"}, replacement: []string{"--session"}, want: []string{"--session", "review", "--model", "native"}},
		{name: "equals", arguments: []string{"--resume=review"}, replacement: []string{"--session"}, want: []string{"--session", "review"}},
		{name: "subcommand", arguments: []string{"--resume", "review"}, replacement: []string{"resume"}, want: []string{"resume", "review"}},
		{name: "boundary", arguments: []string{"--", "--resume", "prompt-data"}, replacement: []string{"--session"}, want: []string{"--", "--resume", "prompt-data"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := translateNativeOption(test.arguments, "--resume", test.replacement, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("translation = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNativeSelectionRulesProjectExactPeerAndLaneSurfaces(t *testing.T) {
	peerCases := []struct {
		product string
		args    []string
		want    []string
	}{
		{product: "codex", args: []string{"--effort", "low"}, want: []string{"-c", "model_reasoning_effort=low"}},
		{product: "claude", args: []string{"--agent=Explore", "--reasoning-effort", "high"}, want: []string{"--agent=Explore", "--effort", "high"}},
		{product: "grok", args: []string{"--agent", "plan", "--effort=low"}, want: []string{"--agent", "plan", "--reasoning-effort", "low"}},
		{product: "opencode", args: []string{"--agent", "octto"}, want: []string{"--agent", "octto"}},
		{product: "kilo", args: []string{"--agent=plan"}, want: []string{"--agent=plan"}},
		{product: "pi", args: []string{"--reasoning-effort", "low"}, want: []string{"--thinking", "low"}},
		{product: "omp", args: []string{"--effort=low"}, want: []string{"--thinking", "low"}},
	}
	for _, test := range peerCases {
		t.Run("peer-"+test.product, func(t *testing.T) {
			descriptor, _ := productcatalog.ByID(test.product)
			got, err := projectNativeArgumentTranslations(descriptor, productcatalog.NativeArgumentPeer, test.args)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("projection = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
	laneCases := []struct {
		product string
		args    []string
		want    []string
	}{
		{product: "codex", args: []string{"run", "--reasoning-effort", "low"}, want: []string{"run", "--effort", "low"}},
		{product: "claude", args: []string{"run", "--agent", "Explore", "--effort", "high"}, want: []string{"run", "--agent", "Explore", "--effort", "high"}},
		{product: "grok", args: []string{"run", "--agent=plan", "--reasoning-effort=low"}, want: []string{"run", "--agent=plan", "--effort", "low"}},
		{product: "opencode", args: []string{"run", "--agent", "octto", "--effort", "high"}, want: []string{"run", "--agent", "octto", "--effort", "high"}},
		{product: "kilo", args: []string{"run", "--agent", "plan", "--reasoning-effort", "minimal"}, want: []string{"run", "--agent", "plan", "--effort", "minimal"}},
		{product: "pi", args: []string{"run", "--effort", "low"}, want: []string{"run", "--thinking", "low"}},
		{product: "omp", args: []string{"run", "--reasoning-effort=low"}, want: []string{"run", "--thinking", "low"}},
		{product: "dsh", args: []string{"run", "--model", "deepseek/deepseek-v4-flash", "--effort", "high"}, want: []string{"run", "--model", "deepseek/deepseek-v4-flash", "--effort", "high"}},
	}
	for _, test := range laneCases {
		t.Run("lane-"+test.product, func(t *testing.T) {
			got, err := ProjectNativeLaneArguments(test.product, test.args)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("projection = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func TestNativeSelectionRulesRejectUnsupportedBeforeLaunch(t *testing.T) {
	tests := []struct {
		product string
		surface productcatalog.NativeArgumentSurface
		args    []string
		want    string
	}{
		{product: "codex", surface: productcatalog.NativeArgumentPeer, args: []string{"--agent", "plan"}, want: "codex has no native agent selector"},
		{product: "codex", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--agent", "plan"}, want: "codex has no native agent selector"},
		{product: "qwen", surface: productcatalog.NativeArgumentPeer, args: []string{"--agent", "plan"}, want: "qwen has no native agent selector"},
		{product: "qwen", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--agent", "plan"}, want: "qwen has no native agent selector"},
		{product: "qwen", surface: productcatalog.NativeArgumentPeer, args: []string{"--effort", "low"}, want: "qwen has no native effort selector"},
		{product: "qwen", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--effort", "low"}, want: "qwen has no native effort selector"},
		{product: "opencode", surface: productcatalog.NativeArgumentPeer, args: []string{"--reasoning-effort=high"}, want: "opencode has no native effort selector"},
		{product: "kilo", surface: productcatalog.NativeArgumentPeer, args: []string{"--effort", "minimal"}, want: "kilo has no native effort selector"},
		{product: "pi", surface: productcatalog.NativeArgumentPeer, args: []string{"--agent", "plan"}, want: "pi has no native agent selector"},
		{product: "pi", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--agent=plan"}, want: "pi has no native agent selector"},
		{product: "omp", surface: productcatalog.NativeArgumentPeer, args: []string{"--agent", "plan"}, want: "omp has no native agent selector"},
		{product: "omp", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--agent", "plan"}, want: "omp has no native agent selector"},
		{product: "dsh", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--agent", "plan"}, want: "dsh has no native agent selector"},
		{product: "dsh", surface: productcatalog.NativeArgumentLane, args: []string{"run", "--effort", "high"}, want: "dsh effort requires --model in the same invocation"},
	}
	for _, test := range tests {
		t.Run(test.product+"-"+string(test.surface), func(t *testing.T) {
			var err error
			if test.surface == productcatalog.NativeArgumentPeer {
				descriptor, _ := productcatalog.ByID(test.product)
				_, err = projectNativeArgumentTranslations(descriptor, test.surface, test.args)
			} else {
				_, err = ProjectNativeLaneArguments(test.product, test.args)
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	descriptor, _ := productcatalog.ByID("qwen")
	got, err := projectNativeArgumentTranslations(descriptor, productcatalog.NativeArgumentPeer, []string{"--", "--agent", "plan"})
	if err != nil || !reflect.DeepEqual(got, []string{"--", "--agent", "plan"}) {
		t.Fatalf("double-dash projection = %#v, %v", got, err)
	}
}

func TestNativeSelectionOmissionLeavesEverySurfaceByteIdentical(t *testing.T) {
	for _, descriptor := range productcatalog.RuntimeInventory() {
		for _, surface := range []productcatalog.NativeArgumentSurface{productcatalog.NativeArgumentPeer, productcatalog.NativeArgumentLane} {
			if surface == productcatalog.NativeArgumentPeer && !descriptor.Has(productcatalog.CapabilityInteractive) {
				continue
			}
			args := []string{"run", "--model", "native-model", "--product-only=value"}
			var got []string
			var err error
			if surface == productcatalog.NativeArgumentLane {
				got, err = ProjectNativeLaneArguments(descriptor.ID, args)
			} else {
				got, err = projectNativeArgumentTranslations(descriptor, surface, args)
			}
			if err != nil || !reflect.DeepEqual(got, args) {
				t.Fatalf("%s %s omission projection = %#v, %v; want %#v", descriptor.ID, surface, got, err, args)
			}
		}
	}
	got, err := ProjectNativeLaneArguments("dsh", []string{"run"})
	if err != nil || !reflect.DeepEqual(got, []string{"run"}) {
		t.Fatalf("DSH omission projection = %#v, %v", got, err)
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
