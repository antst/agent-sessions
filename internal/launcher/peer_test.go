package launcher

import (
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestGenericResumeInvocationUsesCatalogProduct(t *testing.T) {
	tests := map[string]struct {
		launcher string
		args     []string
	}{
		"codex":  {launcher: "codex-peer", args: []string{"resume", "session", "--group", "project"}},
		"claude": {launcher: "claude-peer", args: []string{"--resume", "session", "--group", "project"}},
		"grok":   {launcher: "grok-peer", args: []string{"--resume", "session", "--group", "project"}},
	}
	for product, want := range tests {
		t.Run(product, func(t *testing.T) {
			launcher, args, err := genericResumeInvocation(product, federator.SessionKindInteractive, "session", []string{"--group", "project"})
			if err != nil {
				t.Fatal(err)
			}
			if launcher != want.launcher || !reflect.DeepEqual(args, want.args) {
				t.Fatalf("invocation = %s %v; want %s %v", launcher, args, want.launcher, want.args)
			}
		})
	}
	for product, launcherName := range map[string]string{
		"codex": "codex-peer-lane", "claude": "claude-peer-lane", "grok": "grok-peer-lane",
	} {
		launcher, args, err := genericResumeInvocation(product, federator.SessionKindLane, "session", nil)
		if err != nil || launcher != launcherName || !reflect.DeepEqual(args, []string{"resume", "session"}) {
			t.Fatalf("%s lane resume = %s %v, %v", product, launcher, args, err)
		}
	}
	if _, _, err := genericResumeInvocation("grok", "", "session", nil); err == nil {
		t.Fatal("unknown session kind was routed to an interactive shim")
	}
}
