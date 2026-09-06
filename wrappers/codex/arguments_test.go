package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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
	t.Setenv("CODEX_HOME", "/codex-home")
	plan, coordinated, err := InteractivePlan([]string{"--model", "-g", "-g", "team", "--peer-name", "chosen", "resume", "id"}, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !coordinated || !slices.Equal(plan.Args, []string{"--remote", "unix:///codex-home/app-server-control/app-server-control.sock", "--model", "-g", "resume", "id"}) {
		t.Fatalf("args = %v", plan.Args)
	}
	if !slices.Contains(plan.Env, host.GroupsEnv+`=["team"]`) || !slices.Contains(plan.Env, host.NameEnv+"=chosen") {
		t.Fatalf("env = %v", plan.Env)
	}
}

func TestInteractivePlanRemoteAndPassthrough(t *testing.T) {
	for _, argument := range []string{"--remote", "--remote=x", "--remote-auth-token-env"} {
		if _, _, err := InteractivePlan([]string{argument}, nil); err == nil || !strings.Contains(err.Error(), "caller-controlled --remote") {
			t.Fatalf("%s: %v", argument, err)
		}
	}
	for _, arguments := range [][]string{
		{"--help", "-g", "literal", "--peer-name", "literal"},
		{"exec", "-g", "literal"},
	} {
		environment := []string{"PATH=/bin"}
		plan, coordinated, err := InteractivePlan(arguments, environment)
		if err != nil || coordinated || !slices.Equal(plan.Args, arguments) || !slices.Equal(plan.Env, environment) {
			t.Fatalf("passthrough = %#v, %v", plan, err)
		}
	}
	for _, argument := range []string{"-i", "--image=x", "--local-provider", "--add-dir=x"} {
		if _, coordinated, err := InteractivePlan([]string{argument, "value"}, nil); err != nil || !coordinated {
			t.Fatalf("%s: %v", argument, err)
		}
	}
}

func TestInteractiveDaemonStartScrubsBusEnvironment(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_DAEMON", "1")
	evidence := t.TempDir() + "/daemon.json"
	t.Setenv("CODEX_TEST_EVIDENCE", evidence)
	t.Setenv(host.TokenEnv, "secret")
	original := peerDaemonCommand
	peerDaemonCommand = func(ctx context.Context, _ string, arguments ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestCodexDaemonProcess", "--"}, arguments...)...)
	}
	t.Cleanup(func() { peerDaemonCommand = original })
	if err := StartPeerDaemon(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	var observed struct{ Args, Env []string }
	body, err := os.ReadFile(evidence)
	if err != nil || json.Unmarshal(body, &observed) != nil || !slices.Equal(observed.Args, []string{"app-server", "daemon", "start"}) || slices.Contains(observed.Env, host.TokenEnv) {
		t.Fatalf("observed = %#v, %v", observed, err)
	}
}

func TestCodexDaemonProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_DAEMON") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	names := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		names = append(names, name)
	}
	body, _ := json.Marshal(map[string]any{"Args": os.Args[separator+1:], "Env": names})
	_ = os.WriteFile(os.Getenv("CODEX_TEST_EVIDENCE"), body, 0o600)
	os.Exit(0)
}
