package main

import (
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestAutomaticConnectorProductUsesManagedEnvironmentAndCodexFallback(t *testing.T) {
	for _, test := range []struct {
		name, product, grokSession, want string
	}{
		{name: "codex fallback", want: "codex"},
		{name: "codex explicit environment", product: "codex", want: "codex"},
		{name: "claude", product: "claude", want: "claude"},
		{name: "qwen", product: "qwen", want: "qwen"},
		{name: "grok product", product: "grok", want: "grok"},
		{name: "grok private launch", grokSession: "session", want: "grok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "AGENT_SESSIONS_PRODUCT":
					return test.product
				case "AGENT_SESSIONS_GROK_SESSION_ID":
					return test.grokSession
				default:
					return ""
				}
			}
			got, err := resolveConnectorProduct("auto", getenv)
			if err != nil || got != test.want {
				t.Fatalf("resolve automatic connector = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := resolveConnectorProduct("auto", func(string) string { return "imaginary" }); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid automatic connector error = %v", err)
	}
}

func TestGrokConnectorAcceptsExactTUIOrPrivateLeaderAncestry(t *testing.T) {
	owner := procinfo.Identity{PID: 101, Start: "owner"}
	leader := procinfo.Identity{PID: 102, Start: "leader"}
	caller := procinfo.Identity{PID: 103, Start: "connector"}
	attachment := daemonpkg.ManagedAttachment{
		Product:  "grok",
		Evidence: daemonpkg.NativeEvidence{Process: owner, Ancestry: []procinfo.Identity{leader}},
	}
	for _, ancestry := range [][]procinfo.Identity{{owner}, {leader}, {caller, owner}, {caller, leader}} {
		if !connectorAncestryMatches(attachment, caller, ancestry) {
			t.Fatalf("Grok connector ancestry %+v was rejected", ancestry)
		}
	}
	if connectorAncestryMatches(attachment, caller, []procinfo.Identity{{PID: 104, Start: "unrelated"}}) {
		t.Fatal("unrelated Grok connector ancestry was accepted")
	}
}
