package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestGrokNativeActorOwnsLeaderACPAndDeliveryWithoutHostListener(t *testing.T) {
	root := t.TempDir()
	sessionID := "55555555-5555-4555-8555-555555555555"
	t.Setenv(grokFakeProcessEnv, "1")
	t.Setenv("GROK_FAKE_GENERATED_SESSION_ID", sessionID)
	t.Setenv("GROK_FAKE_SESSION_TITLE", "daemon-grok")
	actorRoot := filepath.Join(root, "actor")
	actor := &grokDaemonActor{
		coordinatorID: "coordinator-test", sessionID: sessionID, name: "daemon-grok",
		cwd: root, profile: root, permission: "default", grokBin: os.Args[0],
		leaderSocket: filepath.Join(actorRoot, "leader.sock"), actorRoot: actorRoot,
		recordPath: filepath.Join(root, "state", "actor.json"), rosterUpdates: make(chan grokRosterState, 8),
		commandOverride: func(arguments ...string) *exec.Cmd {
			argv := append([]string{"-test.run=^TestGrokFakeProcess$", "--"}, arguments...)
			return exec.Command(os.Args[0], argv...)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := actor.start(ctx); err != nil {
		t.Fatalf("start daemon Grok actor: %v", err)
	}
	t.Cleanup(actor.close)
	if _, err := os.Lstat(filepath.Join(actorRoot, "control.sock")); !os.IsNotExist(err) {
		t.Fatalf("obsolete grok-host listener exists: %v", err)
	}
	actor.mu.Lock()
	actor.ownerPID, actor.ownerProcStart = os.Getpid(), readProcStart(os.Getpid())
	state, selected, err := actor.refreshRosterLocked(ctx)
	if err == nil {
		actor.name, actor.permission = state.name, state.permissionMode
		err = actor.persist()
	}
	actor.mu.Unlock()
	if err != nil || selected != sessionID {
		t.Fatalf("refresh daemon Grok roster: selected=%q state=%+v err=%v", selected, state, err)
	}
	frame := federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "grok-daemon-message", Content: "hello from daemon",
	}
	if err := actor.interject(ctx, frame); err != nil {
		t.Fatalf("interject daemon Grok frame: %v", err)
	}
	t.Setenv("GROK_PEER_GROK_BIN", os.Args[0])
	recovered := recoverGrokDaemonActor(actor.recordPath)
	if recovered == nil {
		t.Fatal("recover exact daemon Grok actor = nil")
	}
	recovered.commandOverride = actor.commandOverride
	if session, err := recovered.inspect(ctx); err != nil || session.SessionID != sessionID || session.OwnerPID != os.Getpid() {
		t.Fatalf("reconstruct daemon Grok actor: session=%+v err=%v", session, err)
	}
	recovered.close()
	if _, err := os.Lstat(actor.recordPath); !os.IsNotExist(err) {
		t.Fatalf("daemon Grok actor record survived exact close: %v", err)
	}
	if _, err := os.Lstat(actor.leaderSocket); !os.IsNotExist(err) {
		t.Fatalf("daemon Grok leader socket survived exact close: %v", err)
	}
}
