package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestClaudeNativeCoordinatorPublishesServiceDeliversAndRemovesExactProjection(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	runtimeBase := testutil.ShortSocketRoot(t, "as-claude-native-", "agent-sessions/daemon.sock")
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	paths, err := daemonpkg.ResolveProductionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigurationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := daemonpkg.DaemonConfig{
		SchemaVersion: daemonpkg.DaemonConfigSchemaVersion, HostID: "test-host-id", HostName: "test-host",
		StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(configuration)
	if err := os.WriteFile(paths.ConfigurationFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "claude")
	coordinator := newClaudeNativeCoordinator()
	if err := coordinator.ensureSyntheticService(profile); err != nil {
		t.Fatalf("publish synthetic Claude service: %v", err)
	}
	projection := coordinator.projections[profile]
	if _, err := os.Lstat(projection.recordPath); err != nil {
		t.Fatalf("synthetic record: %v", err)
	}
	keyInfo, err := os.Lstat(projection.keyPath)
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("synthetic key metadata: %v, %v", keyInfo, err)
	}

	target := filepath.Join(runtimeBase, "claude-target.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- ""
			return
		}
		defer func() { _ = connection.Close() }()
		line, _ := bufio.NewReader(connection).ReadString('\n')
		received <- line
	}()
	if err := coordinator.DeliverFrame(context.Background(), target, federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: "message-claude-1", Content: "hello Claude",
	}); err != nil {
		t.Fatalf("deliver native Claude frame: %v", err)
	}
	line := <-received
	if !strings.Contains(line, `"msg_id":"message-claude-1"`) || !strings.Contains(line, "hello Claude") ||
		!strings.Contains(line, paths.ControlEndpoint) {
		t.Fatalf("native Claude carrier = %s", line)
	}

	coordinator.close()
	for _, path := range []string{projection.recordPath, projection.keyPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("synthetic projection survived close %s: %v", path, err)
		}
	}
}
