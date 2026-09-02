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

func writeNativeManifest(t *testing.T, name, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.json")
	body, err := json.Marshal(map[string]string{"name": name, "version": version})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func doctorFixture(t *testing.T, processes ACPProcessFactory) *DoctorProbe {
	t.Helper()
	commands := fakeCommandProbe{
		paths: map[string]string{"dsh": "/usr/bin/dsh", "pnpm": "/usr/bin/pnpm"},
		output: map[string]string{
			"/usr/bin/dsh":  "dsh " + PinnedVersion + "\n",
			"/usr/bin/pnpm": PinnedPNPM + "\n",
		},
	}
	doctor, err := NewDoctorProbe(DoctorConfig{
		DSHHome: "/home/test/.dsh", ACPProfile: "acp",
		ACPAppManifest: writeNativeManifest(t, ACPAppPackage, PinnedVersion),
		Environment:    []string{"HOME=" + t.TempDir(), "PATH=/usr/bin", "DEEPSEEK_API_KEY=not-forwarded"},
		Commands:       commands, Processes: processes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return doctor
}

func TestDoctorVerifiesOnlyTheNativeACPTuple(t *testing.T) {
	doctor := doctorFixture(t, nil)
	tuple, err := doctor.VerifyTuple(context.Background(), "acp")
	if err != nil {
		t.Fatal(err)
	}
	if tuple != PinnedTuple() {
		t.Fatalf("tuple = %#v, want %#v", tuple, PinnedTuple())
	}

	doctor.config.Commands = fakeCommandProbe{
		paths:  map[string]string{"dsh": "/usr/bin/dsh", "pnpm": "/usr/bin/pnpm"},
		output: map[string]string{"/usr/bin/dsh": PinnedVersion, "/usr/bin/pnpm": "10.29.0"},
	}
	if _, err := doctor.VerifyTuple(context.Background(), "acp"); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("unpinned pnpm error = %v", err)
	}
}

func TestDoctorFeatureProbeUsesProductOwnedACPProfile(t *testing.T) {
	factory := &isolatedDoctorProcessFactory{}
	doctor := doctorFixture(t, factory)
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != productruntime.ProbeReady || !report.Features["lane"] || !report.Features["acp-initialize"] || !report.Features["acp-session-new"] {
		t.Fatalf("report = %#v", report)
	}
	if report.Features["peer"] || report.Features["parent"] {
		t.Fatalf("lane-only DSH doctor credited a peer surface: %#v", report.Features)
	}
	if len(factory.roots) != 1 {
		t.Fatalf("probe roots = %v", factory.roots)
	}
	if _, err := os.Stat(factory.roots[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable DSH probe home survived: %v", err)
	}
}

func TestDoctorReportsMissingAndIncompatibleNativeFacts(t *testing.T) {
	doctor := doctorFixture(t, nil)
	doctor.config.Commands = fakeCommandProbe{}
	report, err := doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbePresence})
	if err != nil || report.State != productruntime.ProbeMissing {
		t.Fatalf("missing report = %#v, %v", report, err)
	}

	doctor = doctorFixture(t, nil)
	doctor.config.ACPAppManifest = writeNativeManifest(t, ACPAppPackage, "0.1.2-alpha.4")
	report, err = doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeVersion})
	if err != nil || report.State != productruntime.ProbeIncompatible {
		t.Fatalf("incompatible report = %#v, %v", report, err)
	}
}

func TestDoctorConstructionValidatesLaneInputsWithoutRequiringAProfileSnapshot(t *testing.T) {
	manifest := writeNativeManifest(t, ACPAppPackage, PinnedVersion)
	for _, config := range []DoctorConfig{
		{DSHHome: "/home/test/.dsh", ACPAppManifest: manifest, Timeout: -1},
		{DSHHome: "/tmp/dsh", ACPAppManifest: manifest},
		{DSHHome: "/home/test/.dsh", ACPAppManifest: "relative.json"},
	} {
		if _, err := NewDoctorProbe(config); err == nil {
			t.Fatalf("invalid doctor config accepted: %#v", config)
		}
	}
	if _, err := NewDoctorProbe(DoctorConfig{DSHHome: "/home/test/.dsh", ACPAppManifest: manifest}); err != nil {
		t.Fatalf("native ACP doctor requires a pre-existing profile snapshot: %v", err)
	}
}

func TestManifestVersionReadsOnlyExactProductPackage(t *testing.T) {
	path := writeNativeManifest(t, ACPAppPackage, PinnedVersion)
	if got := manifestVersion(path, ACPAppPackage); got != PinnedVersion {
		t.Fatalf("manifest version = %q", got)
	}
	if got := manifestVersion(path, "@other/package"); got != "" {
		t.Fatalf("foreign manifest version = %q", got)
	}
}

func TestSafeDoctorEnvironmentOmitsCredentials(t *testing.T) {
	got := strings.Join(safeDoctorEnv([]string{
		"HOME=/home/test", "PATH=/usr/bin", "DSH_HOME=/home/test/.dsh",
		"DEEPSEEK_API_KEY=secret", "OTHER_TOKEN=secret",
	}), "\n")
	if strings.Contains(got, "secret") || strings.Contains(got, "DEEPSEEK_API_KEY") || !strings.Contains(got, "HOME=/home/test") {
		t.Fatalf("safe doctor environment = %q", got)
	}
}
