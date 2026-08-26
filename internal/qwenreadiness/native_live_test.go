package qwenreadiness

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/qwenprofile"
)

func TestNativeSourceAgainstInstalledQwen(t *testing.T) {
	if os.Getenv("AGENT_SESSIONS_LIVE_QWEN") != "1" {
		t.Skip("set AGENT_SESSIONS_LIVE_QWEN=1 for the real session-free Qwen readiness probe")
	}
	executable, err := exec.LookPath("qwen")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := qwenprofile.Current()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	source := NewNativeSource(os.Environ())
	report, err := Check(ctx, Request{
		Executable: executable, Workspace: workspace, Profile: profile,
		ExpectedIntegrationVersion: IntegrationVersion, Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Ready {
		t.Logf("credential state = %s (native status %q)", report.CredentialConfigurationState, source.serve.authStatus)
		t.Fatalf("installed Qwen readiness = %#v", report)
	}
}
