package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const qwenInteractiveTestSessionID = "11111111-2222-4333-8444-555555555555"

func TestQwenDualOutputAdmissionRequiresExactFirstSessionStart(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events.jsonl")
	writeQwenTestEvents(t, events, qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.15", 2))
	expected := qwenAdmissionExpectation{
		SessionID: qwenInteractiveTestSessionID, Cwd: cwd, Version: "0.21.15", ProtocolVersion: 2,
		RequiredEvents: []string{"system", "user", "assistant", "stream_event", "result", "control_request", "control_response"},
	}
	cursor, start, err := admitQwenDualOutput(events, expected)
	if err != nil {
		t.Fatal(err)
	}
	if start.SessionID != expected.SessionID || start.Cwd != expected.Cwd || start.Version != expected.Version || cursor.Offset() == 0 {
		t.Fatalf("admission = cursor=%+v start=%+v", cursor, start)
	}

	for _, test := range []struct {
		name  string
		first map[string]any
		want  string
	}{
		{name: "wrong type", first: map[string]any{"type": "assistant"}, want: "first"},
		{name: "wrong subtype", first: map[string]any{"type": "system", "subtype": "init"}, want: "session_start"},
		{name: "wrong uuid", first: qwenSessionStartEvent("00000000-0000-4000-8000-000000000000", cwd, "0.21.15", 2), want: "session"},
		{name: "wrong cwd", first: qwenSessionStartEvent(qwenInteractiveTestSessionID, root, "0.21.15", 2), want: "working"},
		{name: "wrong version", first: qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.14", 2), want: "version"},
		{name: "wrong protocol", first: qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.15", 1), want: "protocol"},
		{name: "missing inventory", first: qwenSessionStartEventWithEvents(qwenInteractiveTestSessionID, cwd, "0.21.15", 2, []string{"system"}), want: "event"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+".jsonl")
			writeQwenTestEvents(t, path, test.first)
			if _, _, err := admitQwenDualOutput(path, expected); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("admission error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestQwenDualOutputCursorRejectsTruncationReplacementAndChangedType(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := qwenAdmissionExpectation{
		SessionID: qwenInteractiveTestSessionID, Cwd: cwd, Version: "0.21.15", ProtocolVersion: 2,
		RequiredEvents: qwenRequiredDualOutputEvents(),
	}
	for _, mutation := range []string{"truncate", "replace", "directory", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(root, mutation+".jsonl")
			writeQwenTestEvents(t, path, qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.15", 2))
			cursor, _, err := admitQwenDualOutput(path, expected)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "truncate":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				writeQwenTestEvents(t, path, qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.15", 2))
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(root, mutation+"-other")
				writeQwenTestEvents(t, other, qwenSessionStartEvent(qwenInteractiveTestSessionID, cwd, "0.21.15", 2))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := cursor.ReadAvailable(); err == nil {
				t.Fatalf("cursor accepted %s mutation", mutation)
			}
		})
	}
}

func TestQwenInputWriterAppendsOneCompleteSubmitAndAttestsBodyCursor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"submit","text":"existing"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := openQwenInputWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.Submit("trusted cross-session message\nsecond line"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("input lines = %q", lines)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &record); err != nil || record["type"] != "submit" || record["text"] != "trusted cross-session message\nsecond line" {
		t.Fatalf("submit record = %#v, %v", record, err)
	}

	if err := writer.Submit("second delivery"); err != nil {
		t.Fatal(err)
	}
	if writer.Offset() != int64(len(mustReadQwenTestFile(t, path))) {
		t.Fatalf("writer offset = %d", writer.Offset())
	}
}

func TestQwenInputWriterRejectsTruncationReplacementAndChangedType(t *testing.T) {
	for _, mutation := range []string{"body", "truncate", "replace", "directory", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "input.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"submit","text":"seed"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writer, err := openQwenInputWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()
			switch mutation {
			case "body":
				body := mustReadQwenTestFile(t, path)
				body[len(body)-3] ^= 1
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"type":"submit","text":"seed"}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(root, "other")
				if err := os.WriteFile(other, []byte(`{"type":"submit","text":"seed"}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Submit("must not append"); err == nil {
				t.Fatalf("writer accepted %s mutation", mutation)
			}
		})
	}
}

func qwenSessionStartEvent(sessionID, cwd, version string, protocol int) map[string]any {
	return qwenSessionStartEventWithEvents(sessionID, cwd, version, protocol, qwenRequiredDualOutputEvents())
}

func qwenSessionStartEventWithEvents(sessionID, cwd, version string, protocol int, events []string) map[string]any {
	return map[string]any{
		"type": "system", "subtype": "session_start", "session_id": sessionID,
		"data": map[string]any{
			"session_id": sessionID, "cwd": cwd, "protocol_version": protocol,
			"version": version, "supported_events": events,
		},
	}
}

func writeQwenTestEvents(t *testing.T, path string, events ...map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustReadQwenTestFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
