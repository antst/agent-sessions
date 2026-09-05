package livepresence

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

var daemonDeletionFiles = []string{
	"cmd/agent-sessions/codex_host.go",
	"cmd/agent-sessions/codex_host_test.go",
	"cmd/agent-sessions/control_retry_test.go",
	"cmd/agent-sessions/dsh_lane.go",
	"cmd/agent-sessions/federation.go",
	"cmd/agent-sessions/federation_test.go",
	"cmd/agent-sessions/lane.go",
	"cmd/agent-sessions/lane_names.go",
	"cmd/agent-sessions/lane_notice.go",
	"cmd/agent-sessions/lane_test.go",
	"cmd/agent-sessions/messaging.go",
	"cmd/agent-sessions/messaging_test.go",
	"cmd/agent-sessions/presence.go",
	"cmd/agent-sessions/presence_test.go",
	"cmd/agent-sessions/preparation.go",
	"cmd/agent-sessions/socket_test.go",
	"internal/daemon/admin.go",
	"internal/daemon/admin_test.go",
	"internal/daemon/adapter_authorization_test.go",
	"internal/daemon/adapter_claude.go",
	"internal/daemon/adapter_claude_test.go",
	"internal/daemon/adapter_codex.go",
	"internal/daemon/adapter_codex_test.go",
	"internal/daemon/adapter_grok.go",
	"internal/daemon/adapter_grok_test.go",
	"internal/daemon/adapter_qwen.go",
	"internal/daemon/adapter_qwen_test.go",
	"internal/daemon/attachment.go",
	"internal/daemon/attachment_test.go",
	"internal/daemon/control.go",
	"internal/daemon/control_test.go",
	"internal/daemon/control_unix.go",
	"internal/daemon/control_unix_test.go",
	"internal/daemon/lane.go",
	"internal/daemon/lane_test.go",
	"internal/daemon/socket_test_helper_test.go",
	"internal/productruntime/architecture_test.go",
	"internal/productruntime/drivers.go",
	"internal/productruntime/environment.go",
	"internal/productruntime/environment_test.go",
	"internal/productruntime/errors.go",
	"internal/productruntime/fakes_test.go",
	"internal/productruntime/lane_registry.go",
	"internal/productruntime/lane_registry_test.go",
	"internal/productruntime/registry.go",
	"internal/productruntime/registry_test.go",
	"internal/productruntime/types.go",
}

var dshDeletionFiles = []string{
	"internal/products/dsh/doctor.go",
	"internal/products/dsh/doctor_test.go",
	"internal/products/dsh/drivers.go",
	"internal/products/dsh/inspect.go",
	"internal/products/dsh/inspect_test.go",
	"internal/products/dsh/lane.go",
	"internal/products/dsh/lane_test.go",
	"internal/products/dsh/permission.go",
	"internal/products/dsh/tuple.go",
	"internal/products/dsh/tuple_permission_test.go",
	"integrations/dsh/lane/README.md",
	"integrations/dsh/lane/cordis.patch.yml",
	"integrations/dsh/lane/package.json",
	"integrations/dsh/lane/plugin.cjs",
	"integrations/dsh/lane/plugin.test.cjs",
	"integrations/dsh/comms/prepack.cjs",
}

var wrapperDeletionFiles = []string{
	"internal/launcher/bootstrap.go",
	"internal/launcher/bootstrap_test.go",
	"internal/launcher/exec.go",
	"internal/launcher/lane.go",
	"internal/launcher/lane_grok_test.go",
	"internal/launcher/lane_test.go",
	"internal/launcher/managed_peer.go",
	"internal/launcher/managed_peer_test.go",
	"internal/launcher/options.go",
	"internal/launcher/options_test.go",
	"internal/launcher/peer_context.go",
	"internal/launcher/peer_context_test.go",
	"internal/launcher/product.go",
	"internal/launcher/product_executable_test.go",
	"internal/launcher/product_test.go",
	"internal/launcher/resume_architecture_test.go",
	"cmd/agent-sessions/connector_refresh.go",
	"cmd/agent-sessions/connector_refresh_test.go",
	"internal/products/claude/lane.go",
	"internal/products/claude/lane_test.go",
	"internal/products/claude/process.go",
	"internal/launcher/claude_peer.go",
	"internal/launcher/claude_peer_test.go",
	"internal/products/codex/lane.go",
	"internal/products/codex/lane_test.go",
	"internal/launcher/codex_peer.go",
	"internal/launcher/codex_peer_test.go",
	"internal/products/grok/lane.go",
	"internal/products/grok/lane_test.go",
	"internal/launcher/grok_peer.go",
	"internal/launcher/grok_peer_test.go",
	"cmd/agent-sessions/grok_peer.go",
	"internal/products/qwen/client.go",
	"internal/products/qwen/lane.go",
	"internal/products/qwen/lane_test.go",
	"internal/products/qwen/process.go",
	"internal/launcher/qwen_peer.go",
	"internal/launcher/qwen_peer_test.go",
	"internal/launcher/qwen_test_helpers_test.go",
	"cmd/agent-sessions/qwen_peer.go",
	"cmd/agent-sessions/qwen_peer_test.go",
	"qwen/scripts/native-entry",
	"internal/products/opencodefamily/client.go",
	"internal/products/opencodefamily/client_test.go",
	"internal/products/opencodefamily/doctor.go",
	"internal/products/opencodefamily/doctor_test.go",
	"internal/products/opencodefamily/lane.go",
	"internal/products/opencodefamily/lane_test.go",
	"internal/products/opencodefamily/lane_transaction_test.go",
	"internal/products/opencodefamily/permission.go",
	"internal/products/opencodefamily/server.go",
	"internal/products/opencodefamily/server_test.go",
	"internal/products/opencode/opencode.go",
	"internal/products/opencode/permission_test.go",
	"internal/products/kilocode/kilocode.go",
	"internal/products/kilocode/permission_test.go",
	"internal/products/pifamily/doctor.go",
	"internal/products/pifamily/doctor_test.go",
	"internal/products/pifamily/lane.go",
	"internal/products/pifamily/permission.go",
	"internal/products/pifamily/quirks.go",
	"internal/products/pifamily/quirks_test.go",
	"internal/products/pifamily/rpc.go",
	"internal/products/pifamily/rpc_lane_test.go",
	"internal/products/pi/permission.go",
	"internal/products/pi/permission_test.go",
	"internal/products/pi/pi.go",
	"internal/products/omp/omp.go",
	"internal/products/omp/permission.go",
	"internal/products/omp/permission_test.go",
}

func TestUniversalMigrationDeletionLedger(t *testing.T) {
	stages := []struct {
		name      string
		files     []string
		wantFiles int
		wantLines int
	}{
		{"daemon", daemonDeletionFiles, 47, 12263},
		{"daemon+dsh", append(append([]string{}, daemonDeletionFiles...), dshDeletionFiles...), 63, 13767},
		{"all", append(append(append([]string{}, daemonDeletionFiles...), dshDeletionFiles...), wrapperDeletionFiles...), 133, 28882},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			seen, lines := make(map[string]bool, len(stage.files)), 0
			for _, name := range stage.files {
				if seen[name] {
					t.Fatalf("duplicate ledger path %s", name)
				}
				seen[name] = true
				content, err := os.ReadFile(filepath.Join("..", "..", name))
				if err != nil {
					t.Fatal(err)
				}
				lines += bytes.Count(content, []byte{'\n'})
			}
			if len(stage.files) != stage.wantFiles || lines != stage.wantLines {
				t.Fatalf("ledger = %d files / %d lines, want %d / %d", len(stage.files), lines, stage.wantFiles, stage.wantLines)
			}
		})
	}
}
