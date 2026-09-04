//go:build darwin

package bridge

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestDarwinProcessIdentityMatchesClaudeRegistryContract(t *testing.T) {
	probe := probeProcessIdentity(os.Getpid())
	if probe.status != processIdentityProbeKnown {
		t.Fatalf("Darwin process identity probe = %+v", probe)
	}
	command := exec.Command("ps", "-p", strconv.Itoa(os.Getpid()), "-o", "lstart=")
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	claudeToken := strings.TrimSpace(string(body))
	if probe.start != claudeToken {
		t.Fatalf("bridge procStart %q differs from Claude ps token %q", probe.start, claudeToken)
	}
	if observation := classifyProcessIdentityProbe(probe, claudeToken); observation.Status != processIdentityMatches {
		t.Fatalf("Claude registry token did not match Darwin process: %+v", observation)
	}
}
