package federator

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCurrentProtocolPreservesQwenCapabilityAndTypedParentContext(t *testing.T) {
	parent := ParentContext{
		HostID: "source", SessionID: "qwen-parent", Product: "qwen", InstanceID: "instance-qwen",
		Groups: []string{"project", "session:source/qwen-parent"}, AlwaysApprove: true,
		AgentRuntimeDir: "/tmp/source runtime", AdapterPID: 41, AdapterProcStart: "adapter-start",
		AdapterStrongStart: "adapter-strong", AdapterSocket: "/tmp/qwen-parent.sock",
		PID: 40, ProcStart: "lifecycle-start", StrongStart: "lifecycle-strong",
		PermissionMode: "yolo", QwenCapabilityDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	want := Message{
		Type: "lane_exec", Version: ProtocolVersion, SourceID: "source/qwen-parent",
		TargetHostID: "destination", Product: "qwen", Args: []string{"start", "--name", "worker", "-"},
		Capabilities: []string{CapabilityQwenLane}, ParentContext: &parent,
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Qwen protocol round trip = %#v, want %#v", got, want)
	}
	if normalized := normalizeCapabilities([]string{CapabilityQwenLane, CapabilityQwenLane, "unknown"}); !reflect.DeepEqual(normalized, []string{CapabilityQwenLane}) {
		t.Fatalf("Qwen capability normalization = %#v", normalized)
	}
}

func TestCurrentProtocolRejectsMixedVersionHello(t *testing.T) {
	for _, version := range []int{ProtocolVersion - 1, ProtocolVersion + 1} {
		if err := validateHello(Message{
			Type: "hello", Version: version, HostID: "qwen-host", HostName: "qwen-host",
			Capabilities: []string{CapabilityQwenLane},
		}); err == nil {
			t.Fatalf("mixed protocol version %d was accepted", version)
		}
	}
}
