//go:build darwin

package bridge

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

func TestDarwinClaudeWorkerRegistryUsesSharedStartToken(t *testing.T) {
	probe := probeProcessIdentity(os.Getpid())
	if probe.status != processIdentityProbeKnown {
		t.Fatalf("Darwin process identity probe = %+v", probe)
	}
	runtimeDir := t.TempDir()
	socket := filepath.Join(runtimeDir, strconv.Itoa(os.Getpid())+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	row := map[string]any{
		"pid": os.Getpid(), "sessionId": "lane-session", "entrypoint": "sdk-cli",
		"procStart": probe.start, "messagingSocketPath": socket,
	}
	resolved, ready, err := verifiedClaudeWorkerPeerSocket(row, runtimeDir, os.Getpid(), probe.start, "lane-session")
	if err != nil || !ready || resolved != socket {
		t.Fatalf("Claude worker row = socket %q ready=%v err=%v", resolved, ready, err)
	}
	if !claudeNativeWorkerRowOwned(row, claudeLaneState{
		SessionID: "lane-session", WorkerPID: os.Getpid(), WorkerProcStart: probe.start,
	}) {
		t.Fatal("Claude worker row was not owned for cleanup")
	}
}

func TestDarwinShimPublishesClaudeCompatibleProcessStart(t *testing.T) {
	root := t.TempDir()
	stateFile := filepath.Join(root, "state", "state.json")
	registryFile := filepath.Join(root, "claude", "sessions", strconv.Itoa(os.Getpid())+".json")
	for _, directory := range []string{filepath.Dir(stateFile), filepath.Dir(registryFile)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	d := daemon{
		sessionID: "00000000-0000-0000-0000-000000000001", cwd: root, name: "darwin-shim",
		nameSource: "generated", permissionMode: "default", status: "idle",
		procStart: readProcStart(os.Getpid()), stateFile: stateFile, registryFile: registryFile,
	}
	if err := d.writeRecordsLocked(); err != nil {
		t.Fatal(err)
	}
	row := readJSONMap(registryFile)
	command := exec.Command("ps", "-p", strconv.Itoa(os.Getpid()), "-o", "lstart=")
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(body))
	if got := stringValue(row["procStart"]); got != want {
		t.Fatalf("shared Claude registry procStart = %q, want %q", got, want)
	}
}
