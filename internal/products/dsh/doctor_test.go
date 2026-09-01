package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

type fakeCommandProbe struct {
	paths  map[string]string
	output map[string]string
}

type isolatedDoctorProcessFactory struct {
	roots []string
}

func (factory *isolatedDoctorProcessFactory) StartACPProcess(_ context.Context, command productruntime.NativeCommand) (ACPProcess, error) {
	root := envValue(command.Env, "DSH_HOME")
	if root == "" {
		return nil, errors.New("doctor omitted isolated DSH_HOME")
	}
	if err := os.WriteFile(filepath.Join(root, "materialized-session"), []byte("native-state"), 0o600); err != nil {
		return nil, err
	}
	factory.roots = append(factory.roots, root)
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "doctor-native"}, nil)
		case "session/close":
			process.respond(frame["id"], map[string]any{}, nil)
		}
	}
	return process, nil
}

func (probe fakeCommandProbe) LookPath(name string) (string, error) {
	if path := probe.paths[name]; path != "" {
		return path, nil
	}
	return "", errors.New("missing")
}
func (probe fakeCommandProbe) Output(_ context.Context, path string, _ []string, _ []string) ([]byte, error) {
	if output, ok := probe.output[path]; ok {
		return []byte(output), nil
	}
	return nil, errors.New("unexpected command")
}

func writeManifest(t *testing.T, root, name, version string) string {
	t.Helper()
	filename := strings.NewReplacer("/", "-", "@", "").Replace(name) + ".json"
	if name == ProfilePackage {
		filename = "package.json"
	}
	path := filepath.Join(root, filename)
	manifest := map[string]any{"name": name, "version": version}
	if name == PluginPackage {
		manifest["packageManager"] = RequiredPNPM + "@" + PinnedPNPM
		manifest["dependencies"] = map[string]string{
			"@deepseek-ai/dsh-llm": PinnedVersion, "@deepseek-ai/dsh-tools": PinnedVersion,
			"@deepseek-ai/dsh-sandbox-policy": PinnedVersion, "@deepseek-ai/dsh-user-approval": PinnedVersion,
		}
		manifest["peerDependencies"] = map[string]string{CLIPackage: PinnedVersion, ACPAppPackage: PinnedVersion}
	}
	if name == ProfilePackage {
		manifest["packageManager"] = RequiredPNPM + "@" + PinnedPNPM
		manifest["dependencies"] = map[string]string{ACPAppPackage: PinnedVersion, PluginPackage: PinnedVersion}
	}
	body, _ := json.Marshal(manifest)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func managedTestDSHHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".local", "state", "agent-sessions")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "dsh-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles", "acp"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func writeManagedProfileManifest(t *testing.T, dshHome, profile, version string) string {
	t.Helper()
	root := filepath.Join(dshHome, "profiles", profile)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return writeManifest(t, root, ProfilePackage, version)
}

func TestDoctorFailsClosedOnTupleMismatchBeforeFeatureProbe(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	process := newScriptedACPProcess()
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": "10.28.1"}},
		ACPAppManifest:  writeManifest(t, root, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, root, PluginPackage, "0.1.2-alpha.4"),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		Processes:       oneProcessFactory{process: process}, ACPProfile: "acp",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeFeature})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != productruntime.ProbeIncompatible || report.TupleOK == nil || *report.TupleOK {
		t.Fatalf("report = %+v", report)
	}
	select {
	case frame := <-process.writes:
		t.Fatalf("feature probe ran after tuple mismatch: %+v", frame)
	default:
	}
}

func TestDoctorUsesKeylessInitializeAndSessionNew(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		id := frame["id"]
		switch frame["method"] {
		case "initialize":
			process.respond(id, map[string]any{"protocolVersion": 1, "authMethods": []any{}}, nil)
		case "session/new":
			process.respond(id, map[string]any{"sessionId": "doctor-native"}, nil)
		case "session/close":
			process.respond(id, map[string]any{}, nil)
		}
	}
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": "dsh " + PinnedVersion, "/bin/pnpm": "10.28.1"}},
		ACPAppManifest:  writeManifest(t, root, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, root, PluginPackage, PinnedVersion),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		Processes:       oneProcessFactory{process: process}, ACPProfile: "acp", ProbeCwd: root,
		CheckIntegration: func(context.Context) (bool, string, error) { return true, "", nil },
		Environment:      []string{"HOME=" + root, "DEEPSEEK_API_KEY=must-not-pass", "DSH_SECRET=must-not-pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != productruntime.ProbeReady || report.TupleOK == nil || !*report.TupleOK || !report.Features["acp-session-new"] {
		t.Fatalf("report = %+v", report)
	}
	if got := envValue(process.command.Env, "DEEPSEEK_API_KEY"); got != "" {
		t.Fatalf("keyless doctor leaked DEEPSEEK_API_KEY=%q", got)
	}
	if got := envValue(process.command.Env, "DSH_SECRET"); got != "" {
		t.Fatalf("keyless doctor leaked DSH_SECRET=%q", got)
	}
	if got := envValue(process.command.Env, "DSH_PERMISSION_MODE"); got != string(SandboxWorkspaceWrite) {
		t.Fatalf("DSH_PERMISSION_MODE = %q, want workspace-write", got)
	}
}

func TestRepeatedDoctorUsesDisposableHomeWithoutNativeStateGrowth(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	profileRoot := filepath.Join(dshHome, "profiles", "acp")
	userHome := filepath.Join(root, "user-dsh")
	if err := os.Mkdir(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, "sentinel"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	factory := &isolatedDoctorProcessFactory{}
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": PinnedPNPM}},
		ACPAppManifest:  writeManifest(t, profileRoot, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, profileRoot, PluginPackage, PinnedVersion),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		Processes:       factory, ACPProfile: "acp", ProbeCwd: root,
		Environment: []string{"HOME=" + root, "DSH_HOME=" + userHome},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		report, probeErr := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeFeature})
		if probeErr != nil || report.State != productruntime.ProbeReady {
			t.Fatalf("repeated probe = %+v, %v", report, probeErr)
		}
	}
	if len(factory.roots) != 2 || factory.roots[0] == factory.roots[1] {
		t.Fatalf("isolated doctor roots = %v", factory.roots)
	}
	for _, probeRoot := range factory.roots {
		if _, err := os.Stat(probeRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disposable doctor root %q remains: %v", probeRoot, err)
		}
	}
	entries, err := os.ReadDir(userHome)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("configured user DSH_HOME changed: entries=%v err=%v", entries, err)
	}
}

func TestDoctorRejectsManifestPackageIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": "10.28.1"}},
		ACPAppManifest:  writeManifest(t, root, "@other/acp-app", PinnedVersion),
		PluginManifest:  writeManifest(t, root, PluginPackage, PinnedVersion),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doctor.VerifyTuple(context.Background(), ""); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("VerifyTuple error = %v, want ErrIncompatible", err)
	}
}

func TestDoctorRejectsPNPMVersionMismatch(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": "10.27.0"}},
		ACPAppManifest:  writeManifest(t, root, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, root, PluginPackage, PinnedVersion),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doctor.VerifyTuple(context.Background(), ""); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("VerifyTuple error = %v, want ErrIncompatible", err)
	}
}

func TestDoctorRejectsPinnedPluginPolicyDependencyDrift(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	pluginManifest := writeManifest(t, root, PluginPackage, PinnedVersion)
	body, err := os.ReadFile(pluginManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["dependencies"].(map[string]any)["@deepseek-ai/dsh-sandbox-policy"] = "0.1.2-alpha.4"
	body, _ = json.Marshal(manifest)
	if err := os.WriteFile(pluginManifest, body, 0o600); err != nil {
		t.Fatal(err)
	}
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": PinnedPNPM}},
		ACPAppManifest:  writeManifest(t, root, ACPAppPackage, PinnedVersion),
		PluginManifest:  pluginManifest,
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doctor.VerifyTuple(context.Background(), ""); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("VerifyTuple dependency drift error = %v, want ErrIncompatible", err)
	}
}

func TestDoctorIntegrationDoesNotCreditPeerWhenCentralAuthorityIsUnready(t *testing.T) {
	root := t.TempDir()
	dshHome := managedTestDSHHome(t)
	process := newScriptedACPProcess()
	process.writeHook = func(frame map[string]any) {
		switch frame["method"] {
		case "initialize":
			process.respond(frame["id"], map[string]any{"protocolVersion": 1}, nil)
		case "session/new":
			process.respond(frame["id"], map[string]any{"sessionId": "doctor-native"}, nil)
		case "session/close":
			process.respond(frame["id"], map[string]any{}, nil)
		}
	}
	doctor, err := NewDoctorProbe(DoctorConfig{
		Commands:        fakeCommandProbe{paths: map[string]string{"dsh": "/bin/dsh", "pnpm": "/bin/pnpm"}, output: map[string]string{"/bin/dsh": PinnedVersion, "/bin/pnpm": PinnedPNPM}},
		ACPAppManifest:  writeManifest(t, root, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, root, PluginPackage, PinnedVersion),
		DSHHome:         dshHome,
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
		Processes:       oneProcessFactory{process: process}, ProbeCwd: root,
		CheckIntegration: func(context.Context) (bool, string, error) {
			return false, "component generation rollover is unready", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != productruntime.ProbeUnconfigured || report.Features["peer"] || report.Features["parent"] || !report.Features["lane"] {
		t.Fatalf("report = %+v", report)
	}
}

func TestManifestVersionRejectsOversizedFileBeforeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-package.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((1 << 20) + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := manifestVersion(path, ProfilePackage); got != "" {
		t.Fatalf("oversized manifest version = %q, want fail-closed empty", got)
	}
}

func TestDoctorPresenceRejectsRenamedExecutableOverride(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	manifestRoot := t.TempDir()
	renamed := filepath.Join(t.TempDir(), "not-dsh")
	if err := os.WriteFile(renamed, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	doctor, err := NewDoctorProbe(DoctorConfig{
		DSHHome: dshHome, ACPProfile: "acp",
		ACPAppManifest:  writeManifest(t, manifestRoot, ACPAppPackage, PinnedVersion),
		PluginManifest:  writeManifest(t, manifestRoot, PluginPackage, PinnedVersion),
		ProfileManifest: writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{
		ProductID: ProductID, Depth: productruntime.ProbePresence, ExecutablePath: renamed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != productruntime.ProbeIncompatible || report.Features["native-cli"] {
		t.Fatalf("renamed executable presence report = %+v", report)
	}
}
