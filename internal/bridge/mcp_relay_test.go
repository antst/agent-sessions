package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func TestDaemonMCPRelayPreservesFramingAndUsesInheritedConnectorIdentity(t *testing.T) {
	t.Setenv(daemonpkg.InternalProductEnvironment, "qwen")
	t.Setenv(daemonpkg.InternalAttachmentIDEnvironment, "attachment-1")
	t.Setenv(daemonpkg.InternalSessionIDEnvironment, "session-1")
	t.Setenv(daemonpkg.InternalCapabilityEnvironment, "capability-1")
	previous := forwardMCPToDaemon
	t.Cleanup(func() { forwardMCPToDaemon = previous })
	forwardMCPToDaemon = func(_ context.Context, identity daemonpkg.LocalControlIdentity, method string, params json.RawMessage) (daemonpkg.MCPForwardResult, error) {
		if identity.Product != "qwen" || identity.AttachmentID != "attachment-1" || identity.SessionID != "session-1" || identity.Capability != "capability-1" {
			t.Fatalf("identity = %#v", identity)
		}
		if method != "tools/list" || len(params) == 0 {
			t.Fatalf("forwarded method=%q params=%s", method, params)
		}
		return daemonpkg.MCPForwardResult{Result: json.RawMessage(`{"tools":[{"name":"identity"}]}`)}, nil
	}
	input := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}` + "\n")
	var output, diagnostics bytes.Buffer
	if exit := runDaemonMCPRelay("codex", input, &output, &diagnostics); exit != 0 {
		t.Fatalf("exit=%d diagnostics=%s", exit, diagnostics.String())
	}
	if got := output.String(); !strings.Contains(got, `"id":7`) || !strings.Contains(got, `"name":"identity"`) {
		t.Fatalf("response = %s", got)
	}
}

func TestDaemonMCPRelayReportsUnavailableToolAsInactiveWithoutFallback(t *testing.T) {
	previous := forwardMCPToDaemon
	t.Cleanup(func() { forwardMCPToDaemon = previous })
	forwardMCPToDaemon = func(context.Context, daemonpkg.LocalControlIdentity, string, json.RawMessage) (daemonpkg.MCPForwardResult, error) {
		return daemonpkg.MCPForwardResult{}, errors.New("daemon absent")
	}
	input := strings.NewReader(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"identity","arguments":{}}}` + "\n")
	var output, diagnostics bytes.Buffer
	if exit := runDaemonMCPRelay("grok", input, &output, &diagnostics); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if got := output.String(); !strings.Contains(got, "inactive outside an attested peer session") || !strings.Contains(got, `"isError":true`) {
		t.Fatalf("response = %s", got)
	}
}
