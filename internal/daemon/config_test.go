package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonConfigAcceptsOneCanonicalHubConnectionOrLocalOnly(t *testing.T) {
	paths := ProductionPaths{StateRoot: "/state/agent-sessions", RuntimeRoot: "/run/agent-sessions"}
	base := DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: "host-a", HostName: "builder",
		StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
	}
	for _, test := range []struct {
		name        string
		hubAddress  string
		remoteLanes bool
		wantError   bool
	}{
		{name: "local only"},
		{name: "hub messaging without remote lanes", hubAddress: "hub.example.test:7419"},
		{name: "hub with remote lanes", hubAddress: "[2001:db8::1]:7419", remoteLanes: true},
		{name: "remote lanes require hub", remoteLanes: true, wantError: true},
		{name: "address is canonical", hubAddress: " hub.example.test:7419", wantError: true},
		{name: "address has no scheme", hubAddress: "https://hub.example.test:7419", wantError: true},
		{name: "address has no credentials", hubAddress: "user@hub.example.test:7419", wantError: true},
		{name: "address requires port", hubAddress: "hub.example.test", wantError: true},
		{name: "address requires usable port", hubAddress: "hub.example.test:0", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := base
			configuration.HubAddress = test.hubAddress
			configuration.RemoteLanesEnabled = test.remoteLanes
			err := configuration.Validate(paths)
			if test.wantError && err == nil {
				t.Fatalf("hub configuration %+v was accepted", configuration)
			}
			if !test.wantError && err != nil {
				t.Fatalf("hub configuration %+v was rejected: %v", configuration, err)
			}
		})
	}
}

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

	t.Run("intermediate symlink", func(t *testing.T) {
		realDirectory := filepath.Join(root, "real-config")
		if err := os.Mkdir(realDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDirectory, "config.json"), []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "config-alias")
		if err := os.Symlink(realDirectory, alias); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDaemonConfig(filepath.Join(alias, "config.json"), paths); err == nil {
			t.Fatal("daemon configuration followed an intermediate directory symlink")
		}
	})
}

func TestDaemonBoundedConfigReadRemainsBoundToOpenedFileAcrossSwap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	original := []byte(`{"schema_version":1,"host_id":"original"}`)
	replacement := filepath.Join(root, "replacement.json")
	outside := []byte(`{"schema_version":1,"host_id":"outside"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, outside, 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := readDaemonBoundedRegularFileWithHook(path, maxDaemonConfigBytes, func() {
		if renameErr := os.Rename(path, path+".opened"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if symlinkErr := os.Symlink(replacement, path); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("bounded read followed swapped path: %s", body)
	}
}

func TestDaemonBoundedConfigReadRejectsWrongOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires privilege to construct a wrong-owner configuration")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := readDaemonBoundedRegularFile(path, maxDaemonConfigBytes); err == nil {
		t.Fatal("bounded daemon configuration read accepted a wrong-owner file")
	}
}
