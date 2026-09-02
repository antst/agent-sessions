package pifamily

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

type doctorRunnerFixture struct {
	path       string
	lookErr    error
	version    string
	versionErr error
	help       string
	helpErr    error
	info       os.FileInfo
	statErr    error
	asset      string
	readErr    error
}

type fileInfoFixture struct{}

func (fileInfoFixture) Name() string       { return "extension.mjs" }
func (fileInfoFixture) Size() int64        { return 1 }
func (fileInfoFixture) Mode() os.FileMode  { return 0444 }
func (fileInfoFixture) ModTime() time.Time { return time.Time{} }
func (fileInfoFixture) IsDir() bool        { return false }
func (fileInfoFixture) Sys() any           { return nil }

func (runner doctorRunnerFixture) LookPath(string) (string, error) {
	return runner.path, runner.lookErr
}
func (runner doctorRunnerFixture) Run(_ context.Context, _ string, arguments ...string) (string, error) {
	if len(arguments) == 1 && arguments[0] == "--version" {
		return runner.version, runner.versionErr
	}
	return runner.help, runner.helpErr
}
func (runner doctorRunnerFixture) Stat(string) (os.FileInfo, error) {
	return runner.info, runner.statErr
}
func (runner doctorRunnerFixture) ReadFile(string, int) ([]byte, error) {
	return []byte(runner.asset), runner.readErr
}

func TestDoctorPinsVersionFeaturesAndManagedExtension(t *testing.T) {
	for _, productID := range []string{PiProductID, OMPProductID} {
		t.Run(productID, func(t *testing.T) {
			quirks, _ := QuirksFor(productID)
			help := "--mode --extension --session "
			if productID == PiProductID {
				help += "--tools"
			} else {
				help += "--approval-mode"
			}
			runner := doctorRunnerFixture{
				path: "/usr/bin/" + quirks.Executable, version: quirks.Executable + " v" + quirks.TestedVersion + "\n",
				help: help, info: fileInfoFixture{}, asset: `const CONTRACT_REVISION = "agent-sessions.component.v1-r1";`,
			}
			probe, err := NewDoctorProbe(DoctorConfig{
				Quirks: quirks, ExtensionPath: "/managed/extension.mjs", Runner: runner,
				CheckIntegration: func(context.Context) (bool, string, error) { return true, "", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			report, err := probe.Probe(context.Background(), productruntime.ProbeRequest{ProductID: productID, Depth: productruntime.ProbeIntegration})
			if err != nil {
				t.Fatal(err)
			}
			if report.State != productruntime.ProbeReady || report.NativeVersion != quirks.TestedVersion ||
				!report.Features["native-cli"] || !report.Features["rpc"] || !report.Features["peer"] ||
				!report.Features["lane"] || !report.Features["parent"] || report.TupleOK == nil || !*report.TupleOK {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestDoctorAcceptsReadableLiveSessionExtension(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	runner := doctorRunnerFixture{
		path: "/usr/bin/pi", version: "pi " + PiTestedVersion,
		help: "--mode --extension --session --tools", info: fileInfoFixture{},
	}
	probe, err := NewDoctorProbe(DoctorConfig{
		Quirks: quirks, ExtensionPath: "/managed/pi/agent-sessions.mjs", Runner: runner,
		CheckIntegration: func(context.Context) (bool, string, error) { return true, "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := probe.Probe(context.Background(), productruntime.ProbeRequest{ProductID: PiProductID, Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeReady {
		t.Fatalf("live-session report = %+v, %v", report, err)
	}
}

func TestDoctorClassifiesMissingIncompatibleAndUnconfigured(t *testing.T) {
	quirks, _ := QuirksFor(PiProductID)
	cases := []struct {
		name   string
		runner doctorRunnerFixture
		state  productruntime.ProbeState
		depth  productruntime.ProbeDepth
	}{
		{name: "missing", runner: doctorRunnerFixture{lookErr: errors.New("missing")}, state: productruntime.ProbeMissing, depth: productruntime.ProbePresence},
		{name: "wrong version", runner: doctorRunnerFixture{path: "/usr/bin/pi", version: "pi 99.0.0"}, state: productruntime.ProbeIncompatible, depth: productruntime.ProbeVersion},
		{name: "missing extension", runner: doctorRunnerFixture{path: "/usr/bin/pi", version: PiTestedVersion, help: "--mode --extension --session --tools", statErr: errors.New("missing")}, state: productruntime.ProbeUnconfigured, depth: productruntime.ProbeIntegration},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			probe, err := NewDoctorProbe(DoctorConfig{Quirks: quirks, ExtensionPath: "/managed/extension.mjs", Runner: testCase.runner})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			report, err := probe.Probe(ctx, productruntime.ProbeRequest{ProductID: PiProductID, Depth: testCase.depth})
			if err != nil || report.State != testCase.state {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}
