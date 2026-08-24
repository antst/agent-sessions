package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federator"
)

func TestQwenInteractiveHostPublishesAndCleansExactPreparation(t *testing.T) {
	agentRuntime := useBridgeTestAgent(t)
	status, err := federator.ReadAgentStatus(agentRuntime)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeQwenProcess(t)
	fake.installEnvironment(t)
	canonicalCwd, err := filepath.EvalSymlinks(fake.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "11111111-2222-4333-8444-555555555555"
	lifecycleRoot := federator.PeerLifecycleRootInState(status.StateDir, "qwen", sessionID)
	if err := os.MkdirAll(lifecycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(lifecycleRoot, "input.jsonl")
	eventsPath := filepath.Join(lifecycleRoot, "events.jsonl")
	for _, path := range []string{inputPath, eventsPath} {
		qwenTestCreatePrivateFile(t, path, nil)
	}
	input, err := federator.QwenArtifactAttestationForPath(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := federator.QwenArtifactAttestationForPath(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	procStart := readProcStart(os.Getpid())
	digest := "sha256:" + strings.Repeat("a", 64)
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: sessionID, Product: "qwen", Name: "qwen-host-test",
		PID: os.Getpid(), ProcStart: procStart, LifecyclePID: os.Getpid(), LifecycleProcStart: procStart,
		LifecycleRoot: lifecycleRoot, QwenCapabilityDigest: digest,
		QwenPreparation: &federator.QwenPreparationPayload{
			Version: 1, CanonicalCwd: canonicalCwd,
			Profile:          federator.QwenProfileIdentity{Fingerprint: strings.Repeat("b", 64)},
			LaunchPreference: "native_default", InitialModeRequest: "native_default",
			Input: input, Events: events, MCPCapabilityDigest: digest,
		},
	}
	metadata := &federator.QwenSessionMetadata{
		Cwd: canonicalCwd, Profile: registration.QwenPreparation.Profile,
		LaunchPreference: "native_default", InitialModeRequest: "native_default",
	}
	request := federator.ResolvePreferencesRequest{
		SessionID: sessionID, Product: "qwen", Kind: federator.SessionKindInteractive, Qwen: metadata,
	}
	preview, err := federator.PreviewSessionPreferences(agentRuntime, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := federator.PreparePeerLaunch(agentRuntime, registration, request, preview.Preference); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- runQwenInteractiveHost(context.Background(), qwenHostArguments{
			Qwen: fake.Paths.Executable, Version: "0.21.15", RuntimeDir: filepath.Join(fake.Paths.Root, "runtime"),
			AgentRuntimeDir: agentRuntime, Registration: registration,
			Native: []string{"--session-id", sessionID, "--json-file", eventsPath, "--input-file", inputPath},
		})
	}()
	qwenTestWaitForPath(t, fake.Paths.Ready, 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resolved, lookupErr := federator.LookupManagedSession(agentRuntime, sessionID)
		if lookupErr == nil && resolved.Live {
			break
		}
		select {
		case hostErr := <-done:
			t.Fatalf("Qwen host exited before publication: %v", hostErr)
		default:
		}
		if time.Now().After(deadline) {
			current, _ := federator.ReadAgentStatus(agentRuntime)
			entries, _ := os.ReadDir(filepath.Join(status.StateDir, "peer-preparations"))
			runtimeEntries, _ := filepath.Glob(filepath.Join(fake.Paths.Root, "runtime", "**"))
			records, _ := qwenTestReadJSONL(fake.Paths.Records)
			socketInfo, socketErr := os.Lstat(filepath.Join(bridgeRuntimeRoot(filepath.Join(fake.Paths.Root, "runtime"), os.Getuid()), "session-"+sessionKey(sessionID)+".sock"))
			t.Fatalf("timed out waiting for managed Qwen publication: lookup=%v status=%+v preparations=%v runtime=%v records=%v socket=%v/%v", lookupErr, current, entries, runtimeEntries, records, socketInfo, socketErr)
		}
		time.Sleep(qwenTestPollInterval)
	}
	qwenFakeWriteMarker(fake.Paths.Stop, "stop\n")
	if err := qwenTestWaitForCommand(t, done, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{inputPath, eventsPath, lifecycleRoot, filepath.Dir(lifecycleRoot)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Qwen host residue survived at %s: %v", path, err)
		}
	}
	if _, err := federator.LookupManagedSession(agentRuntime, sessionID); err != nil {
		// Durable session metadata is intentionally retained for resume.
		t.Fatalf("Qwen resume catalog was not retained: %v", err)
	}
}
