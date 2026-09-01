package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"
)

const (
	realOldFederationBinaryEnv = "AGENT_SESSIONS_REAL_OLD_FEDERATION_BINARY"
	realOldFederationCommitEnv = "AGENT_SESSIONS_REAL_OLD_FEDERATION_COMMIT"
	realOldFederationCommit    = "679fe9d3068b6362df867f8d78ce6708c4ce1342"
	realOldSourcePeerID        = "old-v030/old-source"
	realOldBaselinePeerID      = "new-baseline/baseline"
	realOldFuturePeerID        = "new-future/opencode"
	realOldUpdatedName         = "baseline-updated"
	realOldSharedGroup         = "mixed-v3"
)

type realOldHostObservation struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
}

type realOldPeerObservation struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Product string `json:"product"`
}

type realOldProbeResult struct {
	SourceCommit                     string                   `json:"source_commit"`
	ProtocolVersion                  int                      `json:"protocol_version"`
	RemoteHosts                      []realOldHostObservation `json:"remote_hosts"`
	RemotePeers                      []realOldPeerObservation `json:"remote_peers"`
	SawFuturePeer                    bool                     `json:"saw_future_peer"`
	SawNonBaselinePeer               bool                     `json:"saw_non_baseline_peer"`
	BaselineRosterUpdated            bool                     `json:"baseline_roster_updated"`
	UnknownMarkerTolerated           bool                     `json:"unknown_marker_tolerated"`
	HeartbeatWindowSurvived          bool                     `json:"heartbeat_window_survived"`
	ConnectedAfterHeartbeat          bool                     `json:"connected_after_heartbeat"`
	RouteWithExtraReceiptDataIgnored bool                     `json:"route_with_extra_receipt_data_ignored"`
	Error                            string                   `json:"error,omitempty"`
}

type realOldDelivery struct {
	Source Peer
	Target Peer
	Frame  AgentFrame
}

func TestRealPreFeatureProtocolThreeHostAgainstCurrentHub(t *testing.T) {
	oldBinary := os.Getenv(realOldFederationBinaryEnv)
	if oldBinary == "" {
		t.Skipf("set %s via scripts/spikes/six-product/federation-old/run.sh", realOldFederationBinaryEnv)
	}
	if os.Getenv(realOldFederationCommitEnv) != realOldFederationCommit {
		t.Fatalf("old source commit = %q, want %s", os.Getenv(realOldFederationCommitEnv), realOldFederationCommit)
	}
	if info, err := os.Stat(oldBinary); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("real old federation probe is not executable: path=%q info=%v err=%v", oldBinary, info, err)
	}

	address := unusedTestAddress(t)
	stopHub, hubDone := runTestHub(t, address)
	defer func() {
		stopHub()
		if err := <-hubDone; err != nil {
			t.Errorf("current hub stopped: %v", err)
		}
	}()

	baseline := []Peer{
		mustTestPeer(t, "new-baseline", "baseline", "codex", realOldSharedGroup),
		mustTestPeer(t, "new-baseline", "claude", "claude", realOldSharedGroup),
		mustTestPeer(t, "new-baseline", "grok", "grok", realOldSharedGroup),
		mustTestPeer(t, "new-baseline", "qwen", "qwen", realOldSharedGroup),
	}
	var baselineMu sync.RWMutex
	baselineSnapshot := func(context.Context) ([]Peer, error) {
		baselineMu.RLock()
		peers := make([]Peer, len(baseline))
		for index, peer := range baseline {
			peer.Groups = append([]string(nil), peer.Groups...)
			peers[index] = peer
		}
		baselineMu.RUnlock()
		return peers, nil
	}
	deliveries := make(chan realOldDelivery, 1)
	currentHost, err := NewEmbeddedHost(EmbeddedHostOptions{
		Hub: address, HostID: "new-baseline", HostName: "new-baseline", Build: "current-feature-source",
		Capabilities: []string{CapabilityCodexLane}, ScanInterval: 15 * time.Millisecond,
		HeartbeatInterval: 25 * time.Millisecond, HeartbeatTimeout: time.Second,
		Snapshot: baselineSnapshot,
		DeliverData: func(_ context.Context, source, target Peer, frame AgentFrame) ([]byte, error) {
			deliveries <- realOldDelivery{Source: source, Target: target, Frame: frame}
			return []byte(`{"delivery_id":"new-delivery","receipt_id":"new-receipt","receipt_sequence":41,"future_receipt_key":"ignored"}`), nil
		},
		RunLane: func(context.Context, RemoteLaneRequest) (RemoteLaneResult, error) {
			return RemoteLaneResult{}, errors.New("lane execution is outside this compatibility probe")
		},
		Logger: discardTestLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(currentHost.advertisedCapabilities(), []string{CapabilityCodexLane, transportFeatureOpaquePeerProducts}) {
		t.Fatalf("current host marker advertisement = %q", currentHost.advertisedCapabilities())
	}
	currentCtx, stopCurrent := context.WithCancel(context.Background())
	currentDone := runTestHost(currentHost, currentCtx)
	defer func() {
		stopCurrent()
		waitTestHost(t, currentDone)
	}()

	futureConn := connectRawHubClient(t, address, Message{
		Type: "hello", Version: ProtocolVersion, HostID: "new-future", HostName: "new-future",
		Build: "current-new-product-source", Capabilities: []string{transportFeatureOpaquePeerProducts},
	})
	futureDecoder := json.NewDecoder(futureConn)
	expectRawFrameType(t, futureDecoder, "hello_ok")
	_ = futureConn.SetDeadline(time.Time{})
	future := mustTestPeer(t, "new-future", "opencode", "codex", "future-private-proof")
	future.Product, future.Entrypoint, future.InstanceID = "opencode", "opencode", "opencode:live"
	if err := newWireConn(futureConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}
	futureReadDone := make(chan error, 1)
	go func() {
		futureReadDone <- scanMessages(futureConn, func(Message) error { return nil })
	}()
	defer func() {
		_ = futureConn.Close()
		select {
		case <-futureReadDone:
		case <-time.After(time.Second):
			t.Error("raw new-product client reader did not stop")
		}
	}()
	waitTest(t, func() bool {
		return currentHost.Connected() && containsPeerID(currentHost.RemotePeers(), realOldFuturePeerID)
	}, "current marked host to observe the live new-product peer")

	probeCtx, stopProbe := context.WithTimeout(context.Background(), 12*time.Second)
	defer stopProbe()
	cmd := exec.CommandContext(probeCtx, oldBinary,
		"-hub", address,
		"-source-commit", realOldFederationCommit,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	probeWaited := false
	defer func() {
		if !probeWaited {
			stopProbe()
			_ = cmd.Wait()
		}
	}()
	waitTest(t, func() bool {
		peers := currentHost.RemotePeers()
		return containsPeerID(peers, realOldSourcePeerID) && containsPeerID(peers, realOldFuturePeerID)
	}, "current host to observe both the old source and live new-product peer")

	// Trigger both kinds of post-connect roster update. The old binary must
	// accept the baseline change and never observe the updated new-product row.
	future.Name = "opencode-updated"
	if err := newWireConn(futureConn).Send(Message{Type: "snapshot", Peers: []Peer{future}}); err != nil {
		t.Fatal(err)
	}
	baselineMu.Lock()
	baseline[0].Name = realOldUpdatedName
	baseline[0].DisplayName = realOldUpdatedName + "@new-baseline"
	baselineMu.Unlock()

	waitErr := cmd.Wait()
	probeWaited = true
	var result realOldProbeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode real old probe result: %v; wait=%v stdout=%q stderr=%q", err, waitErr, stdout.String(), stderr.String())
	}
	if waitErr != nil || result.Error != "" {
		t.Fatalf("real old probe failed: wait=%v result=%+v stderr=%q", waitErr, result, stderr.String())
	}
	if probeCtx.Err() != nil {
		t.Fatalf("real old probe timed out: %v", probeCtx.Err())
	}
	if result.SourceCommit != realOldFederationCommit || result.ProtocolVersion != ProtocolVersion {
		t.Fatalf("real old source identity = commit %q protocol %d", result.SourceCommit, result.ProtocolVersion)
	}
	if !result.UnknownMarkerTolerated || !result.BaselineRosterUpdated || !result.HeartbeatWindowSurvived || !result.ConnectedAfterHeartbeat {
		t.Fatalf("real old liveness/roster proof = %+v", result)
	}
	if result.SawFuturePeer || result.SawNonBaselinePeer {
		t.Fatalf("new-product peer leaked into the real old roster: %+v", result)
	}
	wantPeers := []realOldPeerObservation{
		{ID: realOldBaselinePeerID, Name: realOldUpdatedName, Product: "codex"},
		{ID: "new-baseline/claude", Name: "claude", Product: "claude"},
		{ID: "new-baseline/grok", Name: "grok", Product: "grok"},
		{ID: "new-baseline/qwen", Name: "qwen", Product: "qwen"},
	}
	if !reflect.DeepEqual(result.RemotePeers, wantPeers) {
		t.Fatalf("real old filtered peers = %#v, want %#v", result.RemotePeers, wantPeers)
	}
	wantHosts := []realOldHostObservation{
		{ID: "new-baseline", Capabilities: []string{CapabilityCodexLane}},
		{ID: "new-future", Capabilities: []string{}},
	}
	if !reflect.DeepEqual(result.RemoteHosts, wantHosts) {
		t.Fatalf("real old normalized hosts = %#v, want %#v", result.RemoteHosts, wantHosts)
	}
	if !result.RouteWithExtraReceiptDataIgnored {
		t.Fatalf("real old delivery did not accept additive Message.Data: %+v", result)
	}

	select {
	case delivery := <-deliveries:
		if delivery.Source.ID != realOldSourcePeerID || delivery.Target.ID != realOldBaselinePeerID ||
			delivery.Frame.MessageID != "old-to-new-with-receipt-data" ||
			delivery.Frame.Content != "protocol-3 old source routed through the new hub" {
			t.Fatalf("current destination received unexpected old route: %#v", delivery)
		}
	case <-time.After(time.Second):
		t.Fatal("current destination did not receive the old host's baseline route")
	}
	if !containsPeerID(currentHost.RemotePeers(), realOldFuturePeerID) {
		t.Fatal("new-product peer was not live at the current host after the old compatibility probe")
	}
	t.Logf("real old federation source %s passed marker, filtered roster, heartbeat, route, and additive receipt-data checks", result.SourceCommit)
}
