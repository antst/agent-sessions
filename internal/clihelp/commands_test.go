package clihelp

import (
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestParseDispatchesEveryMultiCallAndAliasWithoutChangingPassthrough(t *testing.T) {
	passthrough := []string{"--name", "worker", "--", "prompt", "-g", "native"}
	for _, product := range productcatalog.All() {
		for _, test := range []struct {
			argv0   string
			args    []string
			command string
		}{
			{argv0: "/installed/bin/" + product.PeerAlias, args: passthrough, command: "peer"},
			{argv0: "/installed/bin/" + product.LaneAlias, args: passthrough, command: "lane"},
			{argv0: "agent-sessions", args: append([]string{"peer", product.ID}, passthrough...), command: "peer"},
			{argv0: "agent-sessions", args: append([]string{"lane", product.ID}, passthrough...), command: "lane"},
		} {
			invocation, err := Parse(test.argv0, test.args)
			if err != nil {
				t.Fatalf("Parse(%q,%v): %v", test.argv0, test.args, err)
			}
			if invocation.Command != test.command || invocation.Product != product.ID || !reflect.DeepEqual(invocation.Arguments, passthrough) {
				t.Fatalf("Parse(%q,%v) = %+v", test.argv0, test.args, invocation)
			}
		}
	}

	for _, command := range []string{"daemon", "status", "doctor", "roster"} {
		invocation, err := Parse("agent-sessions", []string{command, "--exact", "value"})
		if err != nil || invocation.Command != command || invocation.Product != "" || !reflect.DeepEqual(invocation.Arguments, []string{"--exact", "value"}) {
			t.Fatalf("admin parse %s = %+v, %v", command, invocation, err)
		}
	}
	for _, command := range []string{"hook", "connector"} {
		invocation, err := Parse("agent-sessions", []string{command, "qwen", "session_start", "--opaque"})
		if err != nil || invocation.Command != command || invocation.Product != "qwen" || !reflect.DeepEqual(invocation.Arguments, []string{"session_start", "--opaque"}) {
			t.Fatalf("product service parse %s = %+v, %v", command, invocation, err)
		}
	}
	auto, err := Parse("agent-sessions", []string{"connector", "auto"})
	if err != nil || auto.Command != "connector" || auto.Product != "auto" || len(auto.Arguments) != 0 {
		t.Fatalf("automatic connector parse = %+v, %v", auto, err)
	}
}

func TestHelpUsesAuthoritativeProductAndAliasInventory(t *testing.T) {
	help := Usage()
	for _, literal := range []string{"agent-sessions daemon", "agent-sessions status", "agent-sessions doctor", "agent-sessions roster", "agent-sessions peer PRODUCT", "agent-sessions lane PRODUCT", "agent-sessions hook PRODUCT", "agent-sessions connector PRODUCT|auto"} {
		if !strings.Contains(help, literal) {
			t.Errorf("help omits %q", literal)
		}
	}
	for _, product := range productcatalog.All() {
		for _, literal := range []string{product.ID, product.PeerAlias, product.LaneAlias} {
			if strings.Count(help, literal) == 0 {
				t.Errorf("help omits %q", literal)
			}
		}
	}
}

func TestParseRejectsUnknownOrIncompleteCommands(t *testing.T) {
	for _, test := range []struct {
		argv0 string
		args  []string
	}{
		{argv0: "unknown-alias"},
		{argv0: "agent-sessions"},
		{argv0: "agent-sessions", args: []string{"unknown"}},
		{argv0: "agent-sessions", args: []string{"peer"}},
		{argv0: "agent-sessions", args: []string{"lane", "imaginary"}},
		{argv0: "agent-sessions", args: []string{"hook"}},
	} {
		if invocation, err := Parse(test.argv0, test.args); err == nil {
			t.Fatalf("Parse(%q,%v) = %+v, want error", test.argv0, test.args, invocation)
		}
	}
}

func TestParseHelpAndVersionRemainReadOnly(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"--version"}} {
		invocation, err := Parse("agent-sessions", args)
		if err != nil || (invocation.Command != "help" && invocation.Command != "version") {
			t.Fatalf("Parse(%v) = %+v, %v", args, invocation, err)
		}
	}
}
