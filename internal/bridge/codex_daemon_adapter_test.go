package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func TestCodexAppServerCoordinatorStartsMissingVendorDaemonOnFirstUse(t *testing.T) {
	profile := t.TempDir()
	want := &appServerClient{done: make(chan struct{})}
	dialCalls, startCalls := 0, 0
	coordinator := &codexAppServerCoordinator{
		clients: make(map[string]*appServerClient),
		dial: func(_ context.Context, socket string) (*appServerClient, error) {
			dialCalls++
			if socket != filepath.Join(profile, "app-server-control", "app-server-control.sock") {
				t.Fatalf("App Server socket = %q", socket)
			}
			if dialCalls == 1 {
				return nil, os.ErrNotExist
			}
			return want, nil
		},
		start: func(_ context.Context, executable, selectedProfile string) error {
			startCalls++
			if executable != "/test/codex" || selectedProfile != profile {
				t.Fatalf("start App Server = %q profile %q", executable, selectedProfile)
			}
			return nil
		},
		executable: func() (string, error) { return "/test/codex", nil },
	}
	first, err := coordinator.client(context.Background(), profile)
	if err != nil || first != want {
		t.Fatalf("first App Server client = %p, %v", first, err)
	}
	second, err := coordinator.client(context.Background(), profile)
	if err != nil || second != want {
		t.Fatalf("reused App Server client = %p, %v", second, err)
	}
	if dialCalls != 2 || startCalls != 1 {
		t.Fatalf("App Server calls = dial %d start %d, want 2/1", dialCalls, startCalls)
	}
}

func TestCodexAppServerCoordinatorDoesNotStartForUntrustedSocketFailure(t *testing.T) {
	startCalls := 0
	coordinator := &codexAppServerCoordinator{
		clients: make(map[string]*appServerClient),
		dial: func(context.Context, string) (*appServerClient, error) {
			return nil, os.ErrPermission
		},
		start: func(context.Context, string, string) error {
			startCalls++
			return nil
		},
		executable: func() (string, error) { return "/test/codex", nil },
	}
	if _, err := coordinator.client(context.Background(), t.TempDir()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("untrusted socket error = %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("untrusted socket failure started vendor daemon %d time(s)", startCalls)
	}
}

func TestCodexAppServerEnvironmentSelectsExactProfile(t *testing.T) {
	got := codexAppServerEnvironment([]string{"PATH=/bin", "CODEX_HOME=/old", "OTHER=value"}, "/new")
	want := []string{"PATH=/bin", "OTHER=value", "CODEX_HOME=/new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex App Server environment = %#v, want %#v", got, want)
	}
}

func TestCodexDaemonAdapterCorroboratesExactAppServerThread(t *testing.T) {
	client := &codexDaemonTestClient{thread: codexDaemonThread{
		ID: "thread-1", Cwd: "/work", Profile: "profile-1", PID: 4101,
		ProcStart: "start-4101", HistoryReady: true,
	}}
	adapter := newCodexDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		AttachmentID: "attachment-1", Product: "codex", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-1"},
		NativeActor:     map[string]any{"thread_id": "thread-1", "pid": 4101, "proc_start": "start-4101"},
	}
	actor, err := adapter.Corroborate(context.Background(), record, map[string]any{"thread_id": "thread-1"})
	if err != nil {
		t.Fatalf("corroborate Codex attachment: %v", err)
	}
	if actor["thread_id"] != "thread-1" || actor["history_ready"] != (true) || client.inspectCalls != 1 {
		t.Fatalf("corroborated Codex actor = %#v, inspect calls=%d", actor, client.inspectCalls)
	}

	client.thread.Cwd = "/other"
	if _, err := adapter.Corroborate(context.Background(), record, map[string]any{"thread_id": "thread-1"}); !errors.Is(err, daemonpkg.ErrAttachmentEvidenceChanged) {
		t.Fatalf("changed Codex cwd error = %v, want exact-evidence rejection", err)
	}
}

func TestCodexDaemonAdapterReportsMissingHistoryProjection(t *testing.T) {
	client := &codexDaemonTestClient{thread: codexDaemonThread{
		ID: "thread-large", Cwd: "/work", Profile: "profile-1", PID: 4201,
		ProcStart: "start-4201", HistoryReady: false,
	}}
	adapter := newCodexDaemonAdapter(client)
	_, err := adapter.Reconnect(context.Background(), daemonpkg.AttachmentRecord{
		SessionID: "thread-large", Product: "codex", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-1"},
		NativeActor:     map[string]any{"thread_id": "thread-large", "pid": 4201, "proc_start": "start-4201"},
	})
	if !errors.Is(err, ErrCodexHistoryProjectionUnavailable) || !strings.Contains(err.Error(), "codex migrate-rollouts --apply") {
		t.Fatalf("missing history error = %v, want native remediation", err)
	}
}

func TestCodexDaemonAdapterDeliversThroughAppServerAndReconnectsWithoutLaunch(t *testing.T) {
	client := &codexDaemonTestClient{thread: codexDaemonThread{
		ID: "thread-2", Cwd: "/work", Profile: "profile-1", PID: 4301,
		ProcStart: "start-4301", HistoryReady: true,
	}}
	adapter := newCodexDaemonAdapter(client)
	record := daemonpkg.AttachmentRecord{
		AttachmentID: "attachment-2", SessionID: "thread-2", Product: "codex", Cwd: "/work",
		ProfileIdentity: map[string]any{"profile": "profile-1"},
		NativeActor:     map[string]any{"thread_id": "thread-2", "pid": 4301, "proc_start": "start-4301"},
	}
	frame := federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "send", MessageID: "message-1", Content: "hello"}
	if err := adapter.Deliver(context.Background(), record, frame); err != nil {
		t.Fatalf("deliver Codex frame: %v", err)
	}
	if len(client.frames) != 1 || !reflect.DeepEqual(client.frames[0], frame) || client.deliverThread != "thread-2" {
		t.Fatalf("Codex App Server delivery = thread %q frames %#v", client.deliverThread, client.frames)
	}
	actor, err := adapter.Reconnect(context.Background(), record)
	if err != nil {
		t.Fatalf("reconnect Codex attachment: %v", err)
	}
	if actor["thread_id"] != "thread-2" || client.launchCalls != 0 {
		t.Fatalf("Codex reconnect actor = %#v, launch calls=%d", actor, client.launchCalls)
	}
}

type codexDaemonTestClient struct {
	thread        codexDaemonThread
	inspectCalls  int
	launchCalls   int
	deliverThread string
	frames        []federation.AgentFrame
}

func (client *codexDaemonTestClient) PrepareInteractive(_ context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	return daemonpkg.NativeLaunchPlan{Executable: "codex", Arguments: request.Intent.NativeArguments, Cwd: request.Cwd}, nil
}

func (client *codexDaemonTestClient) InspectThread(_ context.Context, _, _ string) (codexDaemonThread, error) {
	client.inspectCalls++
	return client.thread, nil
}

func (client *codexDaemonTestClient) DeliverFrame(_ context.Context, _, threadID string, frame federation.AgentFrame) error {
	client.deliverThread = threadID
	client.frames = append(client.frames, frame)
	return nil
}
