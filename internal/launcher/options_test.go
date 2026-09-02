package launcher

import (
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestWrapperOptionScannerPreservesBytesOrderDelimiterAndRepeatedSemanticsAcrossProducts(t *testing.T) {
	for _, product := range productcatalog.All() {
		if !product.Has(productcatalog.CapabilityInteractive) {
			continue
		}
		product := product
		t.Run(product.ID, func(t *testing.T) {
			nativeValue := string([]byte{'-', 'g', 0xff, 'x'})
			args := []string{
				"--model", nativeValue,
				"-g", "first", "--group=second",
				"--no-inherit-groups", "--inherit-groups", "--no-yolo",
				"--", "prompt", "-g", "after-delimiter",
			}
			forwarded, context, err := scanPeerWrapperOptions(product.ID, args)
			if err != nil {
				t.Fatal(err)
			}
			wantForwarded := []string{"--model", nativeValue, "--", "prompt", "-g", "after-delimiter"}
			if !reflect.DeepEqual(forwarded, wantForwarded) {
				t.Fatalf("forwarded argv bytes/order = %q, want %q", forwarded, wantForwarded)
			}
			if !reflect.DeepEqual(context.groups, []string{"first", "second"}) || !context.groupsSpecified {
				t.Fatalf("repeated groups = %#v, specified=%v", context.groups, context.groupsSpecified)
			}
			if !context.inheritGroupsSpecified || !context.inheritParentGroups {
				t.Fatalf("last repeated inheritance option did not win: %#v", context)
			}
			if !context.forceNoYolo {
				t.Fatalf("--no-yolo was not extracted: %#v", context)
			}
			if !productOptionConsumesNext(product.ID, "--model") || productOptionConsumesNext(product.ID, "--model=value") {
				t.Fatal("product option arity table does not distinguish split and attached values")
			}
		})
	}
}

func TestWrapperOptionScannerRejectsInternalOrMalformedOptionsWithoutPartialForwarding(t *testing.T) {
	for _, product := range productcatalog.All() {
		if !product.Has(productcatalog.CapabilityInteractive) {
			continue
		}
		for _, args := range [][]string{
			{"-g"},
			{"--group="},
			{"--parent-session", "forged"},
			{"--parent-session=forged"},
		} {
			if forwarded, context, err := scanPeerWrapperOptions(product.ID, args); err == nil || forwarded != nil || !reflect.DeepEqual(context, peerLaunchContext{}) {
				t.Fatalf("%s accepted %q or returned partial state: %q %#v %v", product.ID, args, forwarded, context, err)
			}
		}
	}
	if _, _, err := scanPeerWrapperOptions("unknown", []string{"--model", "x"}); err == nil {
		t.Fatal("unknown product option table was accepted")
	}
}

func TestWrapperOptionTablesRetainEveryBaselineArityEdge(t *testing.T) {
	tests := []struct {
		product string
		split   []string
		joined  []string
	}{
		{product: "codex", split: []string{"--config", "--image", "--ask-for-approval", "-c", "-i", "-a"}, joined: []string{"--config=x", "--image=x", "-cx", "-ix", "-ax"}},
		{product: "claude", split: []string{"--allowedTools", "--permission-mode", "--resume", "-r", "--settings"}, joined: []string{"--allowedTools=x", "--permission-mode=x", "--resume=x"}},
		{product: "grok", split: []string{"--rules", "--prompt-json", "--permission-mode", "--peer-name", "-n", "-m"}, joined: []string{"--rules=x", "--prompt-json=x", "--peer-name=x", "-mx"}},
		{product: "qwen", split: []string{"--model", "--resume", "--approval-mode", "--qwen-home", "-m", "-r"}, joined: []string{"--model=x", "--resume=x", "--approval-mode=x", "-mx", "-rx"}},
	}
	for _, test := range tests {
		for _, argument := range test.split {
			if !productOptionConsumesNext(test.product, argument) {
				t.Errorf("%s split value option %q was not retained", test.product, argument)
			}
		}
		for _, argument := range test.joined {
			if productOptionConsumesNext(test.product, argument) {
				t.Errorf("%s attached value option %q consumed the following argument", test.product, argument)
			}
		}
	}
}
