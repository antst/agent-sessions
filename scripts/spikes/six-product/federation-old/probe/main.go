package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

const (
	oldHostID          = "old-v030"
	oldSourceSession   = "old-source"
	baselinePeerID     = "new-baseline/baseline"
	baselineClaudeID   = "new-baseline/claude"
	baselineGrokID     = "new-baseline/grok"
	baselineQwenID     = "new-baseline/qwen"
	baselinePeerName   = "baseline-updated"
	futurePeerID       = "new-future/opencode"
	transportMarker    = "federation-peer-products"
	sharedGroup        = "mixed-v3"
	probeMessageID     = "old-to-new-with-receipt-data"
	probeMessage       = "protocol-3 old source routed through the new hub"
	probeBuildMetadata = "real-v0.3.0-source"
)

type hostObservation struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
}

type peerObservation struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Product string `json:"product"`
}

type probeResult struct {
	SourceCommit                     string            `json:"source_commit"`
	ProtocolVersion                  int               `json:"protocol_version"`
	RemoteHosts                      []hostObservation `json:"remote_hosts"`
	RemotePeers                      []peerObservation `json:"remote_peers"`
	SawFuturePeer                    bool              `json:"saw_future_peer"`
	SawNonBaselinePeer               bool              `json:"saw_non_baseline_peer"`
	BaselineRosterUpdated            bool              `json:"baseline_roster_updated"`
	UnknownMarkerTolerated           bool              `json:"unknown_marker_tolerated"`
	HeartbeatWindowSurvived          bool              `json:"heartbeat_window_survived"`
	ConnectedAfterHeartbeat          bool              `json:"connected_after_heartbeat"`
	RouteWithExtraReceiptDataIgnored bool              `json:"route_with_extra_receipt_data_ignored"`
	Error                            string            `json:"error,omitempty"`
}

func main() {
	hub := flag.String("hub", "", "current federation hub address")
	commit := flag.String("source-commit", "", "old source commit used to build this probe")
	flag.Parse()

	result := run(*hub, *commit)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "encode probe result: %v\n", err)
		os.Exit(1)
	}
	if result.Error != "" {
		os.Exit(1)
	}
}

func run(hub, commit string) (result probeResult) {
	result.SourceCommit = commit
	result.ProtocolVersion = federation.ProtocolVersion
	if hub == "" || commit == "" {
		result.Error = "hub and source-commit are required"
		return result
	}

	source, err := federation.BuildPeer(
		oldHostID, oldHostID, oldSourceSession, oldSourceSession,
		"idle", "/mixed-version", "codex", "default", "codex:old-source", "", []string{sharedGroup},
	)
	if err != nil {
		result.Error = fmt.Sprintf("build old source peer: %v", err)
		return result
	}
	host, err := federation.NewEmbeddedHost(federation.EmbeddedHostOptions{
		Hub: hub, HostID: oldHostID, HostName: oldHostID, Build: probeBuildMetadata,
		ScanInterval: 15 * time.Millisecond, HeartbeatInterval: 25 * time.Millisecond,
		HeartbeatTimeout: 150 * time.Millisecond,
		Snapshot: func(context.Context) ([]federation.Peer, error) {
			return []federation.Peer{source}, nil
		},
		Deliver: func(context.Context, federation.Peer, federation.Peer, federation.AgentFrame) error {
			return nil
		},
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		result.Error = fmt.Sprintf("construct old embedded host: %v", err)
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && result.Error == "" {
				result.Error = fmt.Sprintf("old embedded host stopped: %v", runErr)
			}
		case <-time.After(2 * time.Second):
			if result.Error == "" {
				result.Error = "old embedded host did not stop"
			}
		}
	}()

	var target federation.Peer
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		peers := host.RemotePeers()
		observePeers(peers, &result)
		for _, peer := range peers {
			if peer.ID == baselinePeerID && peer.Name == baselinePeerName {
				target = peer
				result.BaselineRosterUpdated = true
				break
			}
		}
		if result.BaselineRosterUpdated {
			break
		}
		select {
		case <-ctx.Done():
			result.Error = "timed out waiting for the filtered baseline roster update"
			return result
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !result.BaselineRosterUpdated {
		result.Error = "timed out waiting for the filtered baseline roster update"
		return result
	}

	heartbeatDeadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(heartbeatDeadline) {
		observePeers(host.RemotePeers(), &result)
		if !host.Connected() {
			result.Error = "old host disconnected during the ping/pong heartbeat window"
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	result.HeartbeatWindowSurvived = true
	result.ConnectedAfterHeartbeat = host.Connected()
	if !result.ConnectedAfterHeartbeat {
		result.Error = "old host was disconnected after the ping/pong heartbeat window"
		return result
	}

	result.RemoteHosts = observeHosts(host.RemoteHosts())
	result.UnknownMarkerTolerated = markerWasIgnored(result.RemoteHosts)
	if !result.UnknownMarkerTolerated {
		result.Error = "old host did not preserve the baseline capability while ignoring the unknown transport marker"
		return result
	}
	result.RemotePeers = peerObservations(host.RemotePeers())
	if result.SawFuturePeer || result.SawNonBaselinePeer || len(result.RemotePeers) != 4 {
		result.Error = "old host observed a peer outside the filtered original-four roster"
		return result
	}

	routeCtx, routeCancel := context.WithTimeout(ctx, 3*time.Second)
	err = host.Send(routeCtx, source, target, probeMessageID, probeMessage, sharedGroup)
	routeCancel()
	if err != nil {
		result.Error = fmt.Sprintf("old-to-new baseline route failed: %v", err)
		return result
	}
	// The current destination acknowledges this Send with receipt JSON in
	// Message.Data. The v0.3.0 Send path can only succeed by decoding the frame
	// and deliberately ignoring that additive data.
	result.RouteWithExtraReceiptDataIgnored = true

	observationDeadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(observationDeadline) {
		observePeers(host.RemotePeers(), &result)
		time.Sleep(10 * time.Millisecond)
	}
	result.RemotePeers = peerObservations(host.RemotePeers())
	if result.SawFuturePeer || result.SawNonBaselinePeer {
		result.Error = "old host observed the new-product peer after the routed delivery"
	}
	return result
}

func observePeers(peers []federation.Peer, result *probeResult) {
	for _, peer := range peers {
		if peer.ID == futurePeerID {
			result.SawFuturePeer = true
		}
		if !isBaselinePeer(peer.ID) {
			result.SawNonBaselinePeer = true
		}
	}
}

func isBaselinePeer(peerID string) bool {
	switch peerID {
	case baselinePeerID, baselineClaudeID, baselineGrokID, baselineQwenID:
		return true
	default:
		return false
	}
}

func observeHosts(hosts []federation.Host) []hostObservation {
	result := make([]hostObservation, 0, len(hosts))
	for _, host := range hosts {
		capabilities := make([]string, len(host.Capabilities))
		copy(capabilities, host.Capabilities)
		sort.Strings(capabilities)
		result = append(result, hostObservation{ID: host.ID, Capabilities: capabilities})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func markerWasIgnored(hosts []hostObservation) bool {
	foundBaseline, foundFuture := false, false
	for _, host := range hosts {
		for _, capability := range host.Capabilities {
			if capability == transportMarker {
				return false
			}
		}
		switch host.ID {
		case "new-baseline":
			foundBaseline = equalStrings(host.Capabilities, []string{federation.CapabilityCodexLane})
		case "new-future":
			foundFuture = len(host.Capabilities) == 0
		}
	}
	return foundBaseline && foundFuture
}

func peerObservations(peers []federation.Peer) []peerObservation {
	result := make([]peerObservation, 0, len(peers))
	for _, peer := range peers {
		result = append(result, peerObservation{ID: peer.ID, Name: peer.Name, Product: peer.Product})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
