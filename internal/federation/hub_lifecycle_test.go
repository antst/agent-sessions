package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

func TestHubRoleDescriptorUsesDistinctSelectedBinaryAndSharedServiceEngine(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "prefix")
	definition := filepath.Join(t.TempDir(), "agent-sessions-hub.plist")
	descriptor, err := HubServiceRole(prefix, definition, "127.0.0.1:7443")
	if err != nil {
		t.Fatalf("resolve hub service role: %v", err)
	}
	wantProgram := filepath.Join(prefix, "libexec", "agent-sessions", "hub", "current", "bin", "agent-sessions-hub")
	if descriptor.Role != "hub" || descriptor.ServiceName != "agent-sessions-hub.service" ||
		descriptor.Label != "net.antst.agent-sessions-hub" || descriptor.DefinitionPath != definition ||
		descriptor.Program != wantProgram || !reflect.DeepEqual(descriptor.ProgramArguments, []string{"--listen", "127.0.0.1:7443"}) {
		t.Fatalf("hub role descriptor = %+v", descriptor)
	}
	if strings.Contains(descriptor.Program, string(filepath.Separator)+"host"+string(filepath.Separator)) ||
		filepath.Base(descriptor.Program) == "agent-sessions" {
		t.Fatalf("hub descriptor aliases the host image: %+v", descriptor)
	}

	runner := &hubServiceCommandRecorder{}
	controller := servicecontrol.NewController(runner)
	operations := []struct {
		name   string
		invoke func(context.Context, *servicecontrol.Controller, servicecontrol.RoleDescriptor) error
	}{
		{name: "enable", invoke: func(ctx context.Context, c *servicecontrol.Controller, d servicecontrol.RoleDescriptor) error {
			return c.Enable(ctx, d)
		}},
		{name: "start", invoke: func(ctx context.Context, c *servicecontrol.Controller, d servicecontrol.RoleDescriptor) error {
			return c.Start(ctx, d)
		}},
		{name: "restart", invoke: func(ctx context.Context, c *servicecontrol.Controller, d servicecontrol.RoleDescriptor) error {
			return c.Restart(ctx, d)
		}},
		{name: "stop", invoke: func(ctx context.Context, c *servicecontrol.Controller, d servicecontrol.RoleDescriptor) error {
			return c.Stop(ctx, d)
		}},
		{name: "disable", invoke: func(ctx context.Context, c *servicecontrol.Controller, d servicecontrol.RoleDescriptor) error {
			return c.Disable(ctx, d)
		}},
	}
	for _, operation := range operations {
		if err := operation.invoke(context.Background(), controller, descriptor); err != nil {
			t.Fatalf("shared %s hub service: %v", operation.name, err)
		}
	}
	if len(runner.commands) != len(operations) {
		t.Fatalf("shared service commands = %#v", runner.commands)
	}
	for index, command := range runner.commands {
		switch runtime.GOOS {
		case "linux":
			if command.executable != "systemctl" || len(command.arguments) != 3 ||
				command.arguments[0] != "--user" || command.arguments[2] != descriptor.ServiceName {
				t.Fatalf("systemd command %d = %#v", index, command)
			}
		case "darwin":
			if command.executable != "launchctl" || !hubCommandContainsIdentity(command.arguments, descriptor) {
				t.Fatalf("launchd command %d = %#v", index, command)
			}
		}
	}
}

func TestCoLocatedHostAndHubUseIndependentReleaseSelectionUpgradeRollbackAndRemoval(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { makeHubTestTreeWritable(root) })
	hostLayout, err := releaseinstall.ResolveRoleLayout(root, releaseinstall.RoleHost)
	if err != nil {
		t.Fatal(err)
	}
	hubLayout, err := releaseinstall.ResolveRoleLayout(root, releaseinstall.RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	if hostLayout.ReleaseRoot == hubLayout.ReleaseRoot || hostLayout.CurrentSelection == hubLayout.CurrentSelection ||
		hostLayout.TransactionRoot == hubLayout.TransactionRoot || hostLayout.InstallLock == hubLayout.InstallLock {
		t.Fatalf("co-located role ownership aliases: host=%+v hub=%+v", hostLayout, hubLayout)
	}
	hostService, hubService := &hubTestRoleService{}, &hubTestRoleService{}
	hostHooks := &hubTestRoleHooks{role: releaseinstall.RoleHost}
	hubHooks := &hubTestRoleHooks{role: releaseinstall.RoleHub}
	hostEngine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{
		Layout: hostLayout, Service: hostService, Hooks: hostHooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	hubEngine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{
		Layout: hubLayout, Service: hubService, Hooks: hubHooks,
	})
	if err != nil {
		t.Fatal(err)
	}

	hostRelease, err := hostEngine.Install(context.Background(), hubTestRelease(t, "host-2024.01", "1", "agent-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	hubPrior, err := hubEngine.Install(context.Background(), hubTestRelease(t, "hub-2030.09", "2", "agent-sessions-hub"))
	if err != nil {
		t.Fatal(err)
	}
	hostSelection := hubReadSelection(t, hostLayout)
	hostCalls := append([]string(nil), hostService.calls...)
	if hostRelease.ReleaseID == hubPrior.ReleaseID || hostSelection == hubReadSelection(t, hubLayout) {
		t.Fatalf("different-role releases alias: host=%+v hub=%+v", hostRelease, hubPrior)
	}

	hubSuccessor, err := hubEngine.Install(context.Background(), hubTestRelease(t, "hub-2031.01", "3", "agent-sessions-hub"))
	if err != nil {
		t.Fatal(err)
	}
	if hubReadSelection(t, hubLayout) != filepath.Join("releases", hubSuccessor.ReleaseID) {
		t.Fatalf("hub upgrade did not select successor %+v", hubSuccessor)
	}
	assertHostLifecycleUnchanged(t, hostLayout, hostSelection, hostService.calls, hostCalls)

	hubHooks.readyErr = errors.New("hub listener identity did not become ready")
	if _, err := hubEngine.Install(context.Background(), hubTestRelease(t, "hub-2032.01", "4", "agent-sessions-hub")); err == nil {
		t.Fatal("hub readiness failure committed its candidate")
	}
	if hubReadSelection(t, hubLayout) != filepath.Join("releases", hubSuccessor.ReleaseID) {
		t.Fatal("hub readiness rollback did not restore its prior exact selection")
	}
	assertHostLifecycleUnchanged(t, hostLayout, hostSelection, hostService.calls, hostCalls)

	hostConfiguration := hubWritePreservedState(t, hostLayout, "host-config.json", "host-preserved")
	hubConfiguration := hubWritePreservedState(t, hubLayout, "hub-config.json", "hub-preserved")
	if err := hubEngine.Remove(context.Background()); err != nil {
		t.Fatalf("remove hub: %v", err)
	}
	if _, err := os.Lstat(hubLayout.CurrentSelection); !os.IsNotExist(err) {
		t.Fatalf("hub removal retained its release selection: %v", err)
	}
	assertHostLifecycleUnchanged(t, hostLayout, hostSelection, hostService.calls, hostCalls)
	if body, err := os.ReadFile(hubConfiguration); err != nil || string(body) != "hub-preserved" {
		t.Fatalf("normal hub removal changed preserved configuration: %q, %v", body, err)
	}
	if body, err := os.ReadFile(hostConfiguration); err != nil || string(body) != "host-preserved" {
		t.Fatalf("hub removal changed host state: %q, %v", body, err)
	}

	plan, err := hubEngine.PlanPurge(context.Background())
	if err != nil || plan.Role != releaseinstall.RoleHub || len(plan.Targets) != 1 || plan.Targets[0] != hubLayout.PreservedStateRoot {
		t.Fatalf("hub purge plan = %+v, %v", plan, err)
	}
	if err := hubEngine.ApplyPurge(context.Background(), plan); err != nil {
		t.Fatalf("purge exact hub state: %v", err)
	}
	if _, err := os.Lstat(hubLayout.PreservedStateRoot); !os.IsNotExist(err) {
		t.Fatalf("hub purge retained planned state: %v", err)
	}
	if body, err := os.ReadFile(hostConfiguration); err != nil || string(body) != "host-preserved" {
		t.Fatalf("hub purge changed host state: %q, %v", body, err)
	}
}

type hubRecordedServiceCommand struct {
	executable string
	arguments  []string
}

type hubServiceCommandRecorder struct{ commands []hubRecordedServiceCommand }

func (recorder *hubServiceCommandRecorder) Run(_ context.Context, executable string, arguments ...string) error {
	recorder.commands = append(recorder.commands, hubRecordedServiceCommand{
		executable: executable,
		arguments:  append([]string(nil), arguments...),
	})
	return nil
}

func hubCommandContainsIdentity(arguments []string, descriptor servicecontrol.RoleDescriptor) bool {
	joined := strings.Join(arguments, " ")
	return strings.Contains(joined, descriptor.Label) || strings.Contains(joined, descriptor.DefinitionPath)
}

type hubTestRoleService struct{ calls []string }

func (*hubTestRoleService) Observe(context.Context) (releaseinstall.RoleServiceState, error) {
	return releaseinstall.RoleServiceState{}, nil
}
func (*hubTestRoleService) Reload(context.Context) error { return nil }
func (*hubTestRoleService) Enable(context.Context) error { return nil }
func (service *hubTestRoleService) Restart(context.Context) error {
	service.calls = append(service.calls, "restart")
	return nil
}
func (service *hubTestRoleService) Stop(context.Context) error {
	service.calls = append(service.calls, "stop")
	return nil
}
func (service *hubTestRoleService) Disable(context.Context) error {
	service.calls = append(service.calls, "disable")
	return nil
}
func (service *hubTestRoleService) Start(context.Context) error {
	service.calls = append(service.calls, "start")
	return nil
}
func (service *hubTestRoleService) Verify(context.Context) error {
	service.calls = append(service.calls, "verify")
	return nil
}
func (*hubTestRoleService) VerifyCandidate(context.Context, releaseinstall.InstalledRelease) error {
	return errors.New("candidate is not already verified")
}

type hubTestRoleHooks struct {
	role     releaseinstall.Role
	readyErr error
	calls    []string
}

func (hooks *hubTestRoleHooks) Prepare(context.Context, releaseinstall.InstallRequest) error {
	hooks.calls = append(hooks.calls, "prepare")
	return nil
}
func (hooks *hubTestRoleHooks) Ready(_ context.Context, release releaseinstall.InstalledRelease) error {
	hooks.calls = append(hooks.calls, "ready")
	wantExecutable := map[releaseinstall.Role]string{
		releaseinstall.RoleHost: "agent-sessions", releaseinstall.RoleHub: "agent-sessions-hub",
	}[hooks.role]
	if release.Role != hooks.role || filepath.Base(release.Executable) != wantExecutable {
		return errors.New("release readiness received another role's executable")
	}
	return hooks.readyErr
}
func (hooks *hubTestRoleHooks) Commit(context.Context) error {
	hooks.calls = append(hooks.calls, "commit")
	return nil
}
func (hooks *hubTestRoleHooks) Rollback(context.Context) error {
	hooks.calls = append(hooks.calls, "rollback")
	return nil
}
func (hooks *hubTestRoleHooks) Remove(context.Context) error {
	hooks.calls = append(hooks.calls, "remove")
	return nil
}

func hubTestRelease(t *testing.T, version, identitySeed, executable string) releaseinstall.InstallRequest {
	t.Helper()
	root := filepath.Join(t.TempDir(), version)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", executable), []byte(version+" "+identitySeed), 0o700); err != nil {
		t.Fatal(err)
	}
	servicePaths := []string{
		"deploy/agent-sessions-hub/systemd/user/agent-sessions-hub.service",
		"deploy/agent-sessions-hub/systemd/user/hub.env.example",
		"deploy/agent-sessions-hub/launchd/net.antst.agent-sessions-hub.plist",
	}
	manifestInventory := `"connector_payloads":[],"service_assets":{"host":[],"hub":["` + strings.Join(servicePaths, `","`) + `"]}`
	role := "hub"
	if executable == "agent-sessions" {
		role = "host"
		servicePaths = []string{
			"deploy/agent-sessions/systemd/user/agent-sessions.service",
			"deploy/agent-sessions/launchd/net.antst.agent-sessions.plist",
		}
		connectorPaths := []string{
			".agents", ".codex-plugin", ".mcp.json", "hooks", "scripts", "skills",
			".claude-plugin", "claude", "grok", "qwen",
		}
		for _, path := range connectorPaths {
			if path == ".mcp.json" {
				if err := os.WriteFile(filepath.Join(root, path), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		manifestInventory = `"connector_payloads":[{"product":"codex","plugin_id":"agent-sessions","archive_paths":[".agents",".codex-plugin",".mcp.json","hooks","scripts","skills"]},{"product":"claude","plugin_id":"agent-sessions","archive_paths":[".claude-plugin","claude"]},{"product":"grok","plugin_id":"agent-sessions","archive_paths":["grok"]},{"product":"qwen","plugin_id":"agent-sessions","archive_paths":["qwen"]}],"service_assets":{"host":["` + strings.Join(servicePaths, `","`) + `"],"hub":[]}`
	}
	serviceBody := []byte("hub service " + identitySeed)
	for _, servicePath := range servicePaths {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, servicePath)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, servicePath), serviceBody, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []byte(`{"schema_version":1,"release_version":"` + version + `","hub_protocol_version":3,"platform":"linux-x64","checksums":"SHA256SUMS","executables":[{"name":"` + executable + `","role":"` + role + `","path":"bin/` + executable + `"}],` + manifestInventory + `}`)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := func(body []byte) string {
		hash := sha256.Sum256(body)
		return hex.EncodeToString(hash[:])
	}
	checksums := digest([]byte(version+" "+identitySeed)) + "  bin/" + executable + "\n"
	if role == "host" {
		checksums += digest([]byte("{}")) + "  .mcp.json\n"
	}
	for _, servicePath := range servicePaths {
		checksums += digest(serviceBody) + "  " + servicePath + "\n"
	}
	checksums += digest(manifest) + "  manifest.json\n"
	if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(checksums), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := releaseinstall.ContentIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return releaseinstall.InstallRequest{
		Version: version, ContentIdentity: identity, SourceRoot: root, Executable: executable,
	}
}

func hubReadSelection(t *testing.T, layout releaseinstall.RoleLayout) string {
	t.Helper()
	selection, err := os.Readlink(layout.CurrentSelection)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func assertHostLifecycleUnchanged(
	t *testing.T,
	layout releaseinstall.RoleLayout,
	wantSelection string,
	gotCalls, wantCalls []string,
) {
	t.Helper()
	if selection := hubReadSelection(t, layout); selection != wantSelection {
		t.Fatalf("hub lifecycle changed host selection: got %q want %q", selection, wantSelection)
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("hub lifecycle invoked host service: got %q want %q", gotCalls, wantCalls)
	}
}

func hubWritePreservedState(t *testing.T, layout releaseinstall.RoleLayout, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(layout.PreservedStateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.PreservedStateRoot, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeHubTestTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
