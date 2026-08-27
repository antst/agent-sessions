package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDaemonConfigAcceptsOnlyCanonicalNonSecretInventory(t *testing.T) {
	paths := ProductionPaths{StateRoot: "/state/agent-sessions", RuntimeRoot: "/run/agent-sessions"}
	configuration := DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: "host-id", HostName: "builder",
		HubAddress: "hub.example:7419", RemoteLanesEnabled: true,
		ProductOverrides: map[string]ProductOverride{
			"codex": {Executable: "codex", Profile: "/profiles/codex"},
		},
		StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 3, UpdatedAt: 42,
	}
	body, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDaemonConfig(path, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != configuration.HostID || got.ProductOverrides["codex"] != configuration.ProductOverrides["codex"] {
		t.Fatalf("loaded configuration = %+v", got)
	}
}

func TestLoadDaemonConfigRejectsNamespacesUnknownFieldsAndSymlinks(t *testing.T) {
	paths := ProductionPaths{StateRoot: "/state/agent-sessions", RuntimeRoot: "/run/agent-sessions"}
	root := t.TempDir()
	valid := `{"schema_version":1,"host_id":"host","host_name":"builder","remote_lanes_enabled":false,"state_root":"/state/agent-sessions","runtime_root":"/run/agent-sessions","revision":1,"updated_at":1}`

	t.Run("unknown field", func(t *testing.T) {
		path := filepath.Join(root, "unknown.json")
		body := valid[:len(valid)-1] + `,"credential":"forbidden"}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDaemonConfig(path, paths); err == nil {
			t.Fatal("unknown credential-shaped field was accepted")
		}
	})

	t.Run("runtime namespace", func(t *testing.T) {
		path := filepath.Join(root, "namespace.json")
		body := strings.Replace(valid, `"/run/agent-sessions"`, `"/tmp/another-daemon"`, 1)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDaemonConfig(path, paths); err == nil {
			t.Fatal("alternate runtime namespace was accepted")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(root, "target.json")
		link := filepath.Join(root, "link.json")
		if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDaemonConfig(link, paths); err == nil {
			t.Fatal("symlinked daemon configuration was accepted")
		}
	})
}
