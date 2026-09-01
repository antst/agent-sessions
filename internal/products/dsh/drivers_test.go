package dsh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type profileRecordingTupleVerifier struct {
	mu       sync.Mutex
	profiles []string
	tuple    Tuple
}

func (verifier *profileRecordingTupleVerifier) VerifyTuple(_ context.Context, profile string) (Tuple, error) {
	verifier.mu.Lock()
	verifier.profiles = append(verifier.profiles, profile)
	verifier.mu.Unlock()
	return verifier.tuple, verifier.tuple.Validate()
}

func TestDriversBindDistinctPeerAndACPProfilesToOneManagedHome(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	laneManifest := writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion)
	manifestRoot := t.TempDir()
	config := DriverConfig{
		ComponentSender: &recordingComponentSender{frames: make(chan component.Frame, 1)},
		Processes:       descendantInspector{},
		Peer: PeerConfig{
			Executable: "dsh", DSHHome: dshHome,
			ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
			TupleVerifier:   StaticTupleVerifier(PinnedTuple()),
		},
		Lane: LaneConfig{
			Executable: "dsh", ACPProfile: "acp", DSHHome: dshHome, ProfileManifest: laneManifest, Generation: 1,
			TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: newScriptedACPProcess()},
			Receipts: memoryReceiptReader{body: []byte("hello")},
		},
		Doctor: DoctorConfig{
			Executable: "dsh", ACPProfile: "acp", DSHHome: dshHome,
			ACPAppManifest: writeManifest(t, manifestRoot, ACPAppPackage, PinnedVersion),
			PluginManifest: writeManifest(t, manifestRoot, PluginPackage, PinnedVersion), ProfileManifest: laneManifest,
			Commands: fakeCommandProbe{},
		},
	}
	if _, err := NewDrivers(config); err != nil {
		t.Fatalf("distinct exact peer/ACP profiles: %v", err)
	}

	otherHome := managedTestDSHHome(t)
	config.Peer.DSHHome = otherHome
	if _, err := NewDrivers(config); err == nil {
		t.Fatal("drivers accepted peer and ACP profiles from different managed DSH homes")
	}
}

func TestDriversRequireParentAncestryInspectorAtComposition(t *testing.T) {
	if _, err := NewDrivers(DriverConfig{}); err == nil {
		t.Fatal("driver composition accepted a missing parent ancestry inspector")
	}
}

func TestAbsentDSHInstallConstructsEveryDriverAndDoctorReportsMissing(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dshHome := filepath.Join(home, ".local", "state", "agent-sessions", "dsh-absent-constructor-test")
	profileManifest := filepath.Join(dshHome, "profiles", "acp", "package.json")
	config := DriverConfig{
		ComponentSender: &recordingComponentSender{frames: make(chan component.Frame, 1)},
		Processes:       descendantInspector{},
		Peer: PeerConfig{
			Executable: "dsh", DSHHome: dshHome,
			ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
			TupleVerifier:   StaticTupleVerifier(PinnedTuple()),
		},
		Lane: LaneConfig{
			Executable: "dsh", ACPProfile: "acp", DSHHome: dshHome, ProfileManifest: profileManifest, Generation: 1,
			TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: newScriptedACPProcess()},
			Receipts: memoryReceiptReader{body: []byte("hello")},
		},
		Doctor: DoctorConfig{
			Executable: "dsh", ACPProfile: "acp", DSHHome: dshHome, ProfileManifest: profileManifest,
			ACPAppManifest: filepath.Join(dshHome, "tuple", "acp-app.json"),
			PluginManifest: filepath.Join(dshHome, "tuple", "plugin.json"), Commands: fakeCommandProbe{},
		},
	}
	drivers, err := NewDrivers(config)
	if err != nil {
		t.Fatalf("construct absent optional DSH integration: %v", err)
	}
	if drivers.Peer == nil || drivers.Lane == nil || drivers.Parent == nil || drivers.Doctor == nil {
		t.Fatalf("drivers = %+v, want total construction", drivers)
	}
	report, err := drivers.Doctor.Probe(context.Background(), productruntime.ProbeRequest{ProductID: ProductID, Depth: productruntime.ProbeIntegration})
	if err != nil || report.State != productruntime.ProbeMissing {
		t.Fatalf("absent DSH doctor = %+v, %v, want missing", report, err)
	}
}

func TestPeerResolvesEachPreparedAttachmentProfileWithoutCrossTarget(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "blue", PinnedVersion)
	writeManagedProfileManifest(t, dshHome, "green", PinnedVersion)
	verifier := &profileRecordingTupleVerifier{tuple: PinnedTuple()}
	gateway, err := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := peer.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	for _, attachment := range []daemon.ManagedAttachment{
		{ID: "blue-peer", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work/blue"},
		{ID: "green-peer", Product: ProductID, ProfileIdentity: "green", Cwd: "/work/green"},
	} {
		if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
			t.Fatalf("Prepare(%s): %v", attachment.ID, err)
		}
	}
	launch := func(id, cwd string) productruntime.NativeCommand {
		t.Helper()
		command, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
			ProductID: ProductID, AttachmentID: id, Cwd: cwd, BootstrapCapabilityID: "cap-" + id,
			BootstrapSecret: productruntime.NewSensitiveValue("secret-" + id),
		})
		if err != nil {
			t.Fatalf("BuildLaunch(%s): %v", id, err)
		}
		return command
	}
	blue, green := launch("blue-peer", "/work/blue"), launch("green-peer", "/work/green")
	if strings.Join(blue.Args, " ") != "--profile blue" || strings.Join(green.Args, " ") != "--profile green" {
		t.Fatalf("isolated commands blue=%v green=%v", blue.Args, green.Args)
	}
	if _, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: "blue-peer", Cwd: "/work/blue", BootstrapCapabilityID: "again",
		BootstrapSecret: productruntime.NewSensitiveValue("again"),
	}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("consumed prepare was cross-target reusable: %v", err)
	}
	rollbackAttachment := daemon.ManagedAttachment{ID: "rolled-back", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work/rollback"}
	if _, err := adapter.Prepare(context.Background(), rollbackAttachment); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Rollback(context.Background(), rollbackAttachment); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: rollbackAttachment.ID, Cwd: rollbackAttachment.Cwd, BootstrapCapabilityID: "rollback",
		BootstrapSecret: productruntime.NewSensitiveValue("rollback"),
	}); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("rolled-back prepare remained launchable: %v", err)
	}
	verifier.mu.Lock()
	seen := append([]string(nil), verifier.profiles...)
	verifier.mu.Unlock()
	if strings.Join(seen, ",") != "blue,green,blue,green,blue" {
		t.Fatalf("tuple profiles = %v, want per-attachment prepare+launch checks", seen)
	}
}

func TestPeerRejectsMissingWrongAndSymlinkedProfileAtOperation(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "valid", PinnedVersion)
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	peer, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "missing", Product: ProductID, ProfileIdentity: "missing", Cwd: "/work"}); !errors.Is(err, productruntime.ErrUnavailable) {
		t.Fatalf("missing profile = %v, want unavailable", err)
	}
	wrong := writeManagedProfileManifest(t, dshHome, "wrong", "0.1.2-alpha.4")
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "wrong", Product: ProductID, ProfileIdentity: "wrong", Cwd: "/work"}); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("wrong tuple profile = %v, want incompatible", err)
	}
	if err := os.Remove(wrong); err != nil {
		t.Fatal(err)
	}
	validManifest := filepath.Join(dshHome, "profiles", "valid", "package.json")
	if err := os.Symlink(validManifest, wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "symlink", Product: ProductID, ProfileIdentity: "wrong", Cwd: "/work"}); !errors.Is(err, productruntime.ErrIncompatible) {
		t.Fatalf("symlinked profile = %v, want incompatible", err)
	}
}

func TestDSHConstructorsContainNoLiveFilesystemOrProcessProbe(t *testing.T) {
	violations, err := analyzeProductionConstructorPurity()
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("constructor purity violations:\n%s", strings.Join(violations, "\n"))
	}
}

func TestProfileIdentityCannotEscapeManagedProfileDirectory(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManifest(t, dshHome, ProfilePackage, PinnedVersion)
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	peer, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	for _, profile := range []string{".", "..", "blue\nother", "blue\x7fother"} {
		if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "escape-" + profile, Product: ProductID, ProfileIdentity: profile, Cwd: "/work"}); !errors.Is(err, productruntime.ErrNativeRejected) {
			t.Fatalf("profile %q = %v, want native rejected", profile, err)
		}
	}
}

func TestFailedReprepareBurnsPriorProfile(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "blue", PinnedVersion)
	writeManagedProfileManifest(t, dshHome, "green", "0.1.2-alpha.4")
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	peer, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	request := productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: "peer", Cwd: "/work", BootstrapCapabilityID: "cap",
		BootstrapSecret: productruntime.NewSensitiveValue("secret"),
	}
	for _, failedProfile := range []string{"green", "missing"} {
		if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "peer", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Prepare(context.Background(), daemon.ManagedAttachment{ID: "peer", Product: ProductID, ProfileIdentity: failedProfile, Cwd: "/work"}); err == nil {
			t.Fatalf("failed profile %q unexpectedly prepared", failedProfile)
		}
		if _, err := peer.BuildLaunch(context.Background(), request); !errors.Is(err, productruntime.ErrStale) {
			t.Fatalf("failed re-Prepare(%q) preserved blue: %v", failedProfile, err)
		}
	}
}

func TestInvalidBuildLaunchBurnsPreparedProfile(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "blue", PinnedVersion)
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	peer, _ := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	attachment := daemon.ManagedAttachment{ID: "peer", Product: ProductID, ProfileIdentity: "blue", Cwd: "/work"}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	invalid := productruntime.PeerLaunchRequest{ProductID: ProductID, AttachmentID: "peer", Cwd: "/work"}
	if _, err := peer.BuildLaunch(context.Background(), invalid); !errors.Is(err, productruntime.ErrNativeRejected) {
		t.Fatalf("invalid launch = %v, want native rejected", err)
	}
	valid := invalid
	valid.BootstrapCapabilityID = "cap"
	valid.BootstrapSecret = productruntime.NewSensitiveValue("secret")
	if _, err := peer.BuildLaunch(context.Background(), valid); !errors.Is(err, productruntime.ErrStale) {
		t.Fatalf("valid retry reused profile after invalid launch: %v", err)
	}
}

func TestConcurrentPreparedProfilesNeverCrossTargets(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	writeManagedProfileManifest(t, dshHome, "blue", PinnedVersion)
	writeManagedProfileManifest(t, dshHome, "green", PinnedVersion)
	gateway, _ := NewCordisGateway(&recordingComponentSender{frames: make(chan component.Frame, 1)}, nil)
	peer, _ := NewPeerDriver(PeerConfig{
		Executable: "dsh", DSHHome: dshHome, ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		Gateway: gateway, TupleVerifier: StaticTupleVerifier(PinnedTuple()),
	})
	adapter, _ := peer.AttachmentAdapter(productruntime.HostDeps{})
	var wait sync.WaitGroup
	errorsCh := make(chan error, 64)
	for index := 0; index < 32; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			profile := "blue"
			if index%2 == 1 {
				profile = "green"
			}
			id := fmt.Sprintf("peer-%d", index)
			cwd := fmt.Sprintf("/work/%d", index)
			attachment := daemon.ManagedAttachment{ID: id, Product: ProductID, ProfileIdentity: profile, Cwd: cwd}
			if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
				errorsCh <- err
				return
			}
			command, err := peer.BuildLaunch(context.Background(), productruntime.PeerLaunchRequest{
				ProductID: ProductID, AttachmentID: id, Cwd: cwd, BootstrapCapabilityID: "cap-" + id,
				BootstrapSecret: productruntime.NewSensitiveValue("secret-" + id),
			})
			if err != nil {
				errorsCh <- err
				return
			}
			if len(command.Args) < 2 || command.Args[0] != "--profile" || command.Args[1] != profile {
				errorsCh <- fmt.Errorf("%s received args %v for %s", id, command.Args, profile)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
}
