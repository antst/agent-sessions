package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/wrappers/host"
)

func TestProcessArguments(t *testing.T) {
	for _, test := range []struct {
		name        string
		input, want []string
		failure     string
	}{
		{"ordered", []string{"--enable", "alpha", "-c", "feature=true", "--search", "--disable=beta"}, []string{"--enable", "alpha", "-c", "feature=true", "--search", "--disable=beta"}, ""},
		{"model conflict", []string{"--config", `model="gpt"`}, nil, "typed field model"},
		{"nested MCP conflict", []string{"-c=mcp_servers.agent_sessions.command=other"}, nil, "typed field mcp"},
		{"quoted MCP conflict", []string{"-c", `mcp_servers . "agent_sessions" . command=other`}, nil, "typed field mcp"},
		{"permission conflict", []string{"-c", "sandbox_mode=workspace-write"}, nil, "typed field permission_mode"},
		{"position", []string{"prompt"}, nil, "unsupported argument prompt"},
		{"subcommand", []string{"exec"}, nil, "unsupported argument exec"},
		{"separator", []string{"--"}, nil, "unsupported argument --"},
		{"unknown", []string{"--profile", "work"}, nil, "unsupported argument --profile"},
		{"missing value", []string{"--enable"}, nil, "requires a value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := processArguments(test.input)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("arguments = %v, %v", got, err)
			}
		})
	}
}

func TestPermissionAndName(t *testing.T) {
	for _, test := range []struct{ input, approval, sandbox, failure string }{
		{"", "never", "", ""}, {"default", "never", "", ""},
		{"bypassPermissions", "never", "danger-full-access", ""},
		{"ask", "", "", "unsupported value permission_mode=ask"},
	} {
		approval, sandbox, err := permission(test.input)
		if approval != test.approval || sandbox != test.sandbox || (err != nil && err.Error() != test.failure) {
			t.Fatalf("permission(%q) = %q, %q, %v", test.input, approval, sandbox, err)
		}
	}
	if got, err := namePart("parent/leaf@host"); err != nil || got != "parent/leaf" {
		t.Fatalf("name = %q, %v", got, err)
	}
}

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan([]string{"--model", "-g", "-g", "team", "--peer-name", "chosen", "resume", "id"}, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Args, []string{"--model", "-g", "resume", "id"}) {
		t.Fatalf("args = %v", plan.Args)
	}
	if !slices.Contains(plan.Env, host.GroupsEnv+`=["team"]`) || !slices.Contains(plan.Env, host.NameEnv+"=chosen") {
		t.Fatalf("env = %v", plan.Env)
	}
}

func TestInteractivePlanRemoteAndPassthrough(t *testing.T) {
	for _, argument := range []string{"--remote", "--remote=x", "--remote-auth-token-env"} {
		if _, err := InteractivePlan([]string{argument}, nil); err == nil || !strings.Contains(err.Error(), "caller-controlled --remote") {
			t.Fatalf("%s: %v", argument, err)
		}
	}
	for _, arguments := range [][]string{
		{"--help", "-g", "literal", "--peer-name", "literal"},
		{"exec", "-g", "literal"},
	} {
		environment := []string{"PATH=/bin"}
		plan, err := InteractivePlan(arguments, environment)
		if err != nil || !slices.Equal(plan.Args, arguments) || !slices.Equal(plan.Env, environment) {
			t.Fatalf("passthrough = %#v, %v", plan, err)
		}
	}
	for _, argument := range []string{"-i", "--image=x", "--local-provider", "--add-dir=x"} {
		if _, err := InteractivePlan([]string{argument, "value"}, nil); err != nil {
			t.Fatalf("%s: %v", argument, err)
		}
	}
}
