package dsh

import (
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/component"
)

func TestDriversBindDistinctPeerAndACPProfilesToOneManagedHome(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	peerManifest := writeManagedProfileManifest(t, dshHome, "peer-ui", PinnedVersion)
	laneManifest := writeManagedProfileManifest(t, dshHome, "acp", PinnedVersion)
	manifestRoot := t.TempDir()
	config := DriverConfig{
		ComponentSender: &recordingComponentSender{frames: make(chan component.Frame, 1)},
		Processes:       descendantInspector{},
		Peer: PeerConfig{
			Executable: "dsh", Profile: "peer-ui", DSHHome: dshHome, ProfileManifest: peerManifest,
			ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
			TupleVerifier:   StaticTupleVerifier(PinnedTuple()),
		},
		Lane: LaneConfig{
			Executable: "dsh", ACPProfile: "acp", DSHHome: dshHome, ProfileManifest: laneManifest, Generation: 1,
			TupleVerifier: StaticTupleVerifier(PinnedTuple()), Processes: oneProcessFactory{process: newScriptedACPProcess()},
			Leases: &recordingLease{}, Receipts: memoryReceiptReader{body: []byte("hello")},
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
	config.Peer.ProfileManifest = writeManagedProfileManifest(t, otherHome, "peer-ui", PinnedVersion)
	if _, err := NewDrivers(config); err == nil {
		t.Fatal("drivers accepted peer and ACP profiles from different managed DSH homes")
	}
}

func TestPeerRejectsManifestForAnotherConfiguredProfile(t *testing.T) {
	dshHome := managedTestDSHHome(t)
	wrongManifest := writeManagedProfileManifest(t, dshHome, "sibling", PinnedVersion)
	if _, err := NewPeerDriver(PeerConfig{
		Executable: "dsh", Profile: "peer-ui", DSHHome: dshHome, ProfileManifest: wrongManifest,
		ComponentSocket: filepath.Join(dshHome, "run", component.ComponentSocketName),
		TupleVerifier:   StaticTupleVerifier(PinnedTuple()),
	}); err == nil {
		t.Fatal("peer accepted a tuple manifest for a different same-home profile")
	}
}

func TestDriversRequireParentAncestryInspectorAtComposition(t *testing.T) {
	if _, err := NewDrivers(DriverConfig{}); err == nil {
		t.Fatal("driver composition accepted a missing parent ancestry inspector")
	}
}
