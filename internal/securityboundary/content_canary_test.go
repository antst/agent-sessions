package securityboundary_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/securityboundary"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestCredentialAndProfileObservationIsMetadataOnly(t *testing.T) {
	canaries := securityboundary.FixtureCanaries()
	root := t.TempDir()
	for _, class := range []securityboundary.ContentClass{
		securityboundary.CredentialContent,
		securityboundary.TranscriptContent,
		securityboundary.LogContent,
	} {
		path := filepath.Join(root, string(class)+".json")
		if err := os.WriteFile(path, canaries[class], 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, err := securityboundary.ObserveFileMetadata(path)
		if err != nil || !metadata.Exists || metadata.Size != int64(len(canaries[class])) || metadata.Mode.Perm() != 0o600 {
			t.Fatalf("metadata %s = %+v, %v", class, metadata, err)
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		assertNoContentCanary(t, "metadata", encoded, canaries)
	}
}

func TestHookConnectorAndDiagnosticFailuresNeverEchoContentIntoEvidence(t *testing.T) {
	canaries := securityboundary.FixtureCanaries()
	root := testutil.ShortSocketRoot(t, "sb-", filepath.Join("run", "daemon.sock"))
	seen := map[securityboundary.ContentClass]bool{}
	server, err := daemon.StartControlServer(context.Background(), root, 3, func(_ context.Context, request daemon.ControlRequest) (json.RawMessage, error) {
		for class, canary := range canaries {
			if jsonContains(request.Payload, canary) {
				seen[class] = true
			}
		}
		return nil, errors.New(string(canaries[securityboundary.LogContent]))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	hookPayload, _ := json.Marshal(map[string]string{
		"credential": string(canaries[securityboundary.CredentialContent]),
		"prompt":     string(canaries[securityboundary.PromptContent]),
		"transcript": string(canaries[securityboundary.TranscriptContent]),
	})
	connectorPayload, _ := json.Marshal(map[string]string{
		"result": string(canaries[securityboundary.ResultContent]),
		"log":    string(canaries[securityboundary.LogContent]),
	})
	requests := []daemon.ControlRequest{
		{ID: "hook", Role: daemon.RoleHook, Operation: "hook.event", Generation: 3, IdempotencyKey: "hook-key", Payload: hookPayload},
		{ID: "connector", Role: daemon.RoleConnector, Operation: "connector.call", Generation: 3, IdempotencyKey: "connector-key", AttachmentID: "attachment", Capability: "capability", Payload: connectorPayload},
	}
	for _, request := range requests {
		response, err := daemon.CallControl(context.Background(), server.Endpoint(), request)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		assertNoContentCanary(t, request.ID+" response", encoded, canaries)
	}
	for class := range canaries {
		if !seen[class] {
			t.Fatalf("%s canary did not reach its requested operation input", class)
		}
	}

	evidence, err := json.Marshal(map[string]any{
		"cell": "content-boundary", "verdict": "RED", "diagnostic_classification": "operation_failed",
		"preserved_state_evidence": map[string]any{"credential_exists": true, "transcript_exists": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoContentCanary(t, "test evidence", evidence, canaries)
}

func TestContentCanaryDetectorRejectsEveryRawAndJSONEncodedClass(t *testing.T) {
	canaries := securityboundary.FixtureCanaries()
	for class, canary := range canaries {
		if got := securityboundary.Detect(canary, canaries); len(got) != 1 || got[0] != class {
			t.Fatalf("raw %s detection = %v", class, got)
		}
		encoded, err := json.Marshal(map[string]string{"value": string(canary)})
		if err != nil {
			t.Fatal(err)
		}
		if got := securityboundary.Detect(encoded, canaries); len(got) != 1 || got[0] != class {
			t.Fatalf("encoded %s detection = %v", class, got)
		}
	}
}

func assertNoContentCanary(t *testing.T, surface string, data []byte, canaries map[securityboundary.ContentClass][]byte) {
	t.Helper()
	if found := securityboundary.Detect(data, canaries); len(found) != 0 {
		t.Fatalf("%s leaked content canaries %v", surface, found)
	}
}

func jsonContains(payload json.RawMessage, canary []byte) bool {
	var decoded map[string]string
	if json.Unmarshal(payload, &decoded) != nil {
		return false
	}
	for _, value := range decoded {
		if value == string(canary) {
			return true
		}
	}
	return false
}
