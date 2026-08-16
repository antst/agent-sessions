package federator

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWriteShadowRecordPublishesExactProcessStart(t *testing.T) {
	started := processStart(os.Getpid())
	if started == "" {
		t.Skip("platform does not expose a supported process-start token")
	}
	path, err := writeShadowRecord(t.TempDir(), os.Getpid(), filepath.Join(t.TempDir(), "shadow.sock"), Peer{
		GlobalID: "remote-session", DisplayName: "remote-peer",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record registryRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.PID != os.Getpid() || record.ProcStart != started {
		t.Fatalf("shadow identity = pid %d start %q, want pid %d start %q", record.PID, record.ProcStart, os.Getpid(), started)
	}
}

func TestWriteShadowRecordRefusesUnknownProcessIdentity(t *testing.T) {
	registry := t.TempDir()
	if _, err := writeShadowRecord(registry, 1<<30, filepath.Join(t.TempDir(), "shadow.sock"), Peer{
		GlobalID: "remote-session", DisplayName: "remote-peer",
	}); err == nil {
		t.Fatal("shadow record with unknown process identity was published")
	}
	entries, err := os.ReadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed shadow publication left registry entries: %v", entries)
	}
}

func TestDiscoverLocalPeersExportsRealAndSkipsFederatedRecords(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "sessions")
	if err := ensureDir(registry); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "peer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	pid := os.Getpid()
	path := filepath.Join(registry, strconv.Itoa(pid)+".json")
	record := registryRecord{
		PID: pid, SessionID: "session-a", Name: "reviewer", Status: "idle",
		MessagingSocketPath: socket, ProcStart: processStart(pid), StartedAt: time.Now().UnixMilli(),
		PermissionMode: "bypassPermissions",
	}
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	peers, err := discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := peers["host-a/session-a"]
	if !ok || peer.DisplayName != "reviewer--alpha" || peer.GlobalID != globalSessionID("host-a", "session-a") {
		t.Fatalf("peer = %#v, exists=%v", peer, ok)
	}
	// A registry claim alone cannot elevate the exported permission class. This
	// test process was not launched in bypass mode, so argv corroboration wins.
	if peer.PermissionMode != "default" {
		t.Fatalf("permission mode = %q", peer.PermissionMode)
	}
	instanceID := peer.InstanceID
	record.SessionID = "session-b"
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	peers, err = discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rotated := peers["host-a/session-b"]; rotated.InstanceID == "" || rotated.InstanceID != instanceID {
		t.Fatalf("session rotation changed process instance %q to %q", instanceID, rotated.InstanceID)
	}
	record.ProcStart = ""
	record.SessionID = "session-c"
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	peers, err = discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	fallbackInstanceID := peers["host-a/session-c"].InstanceID
	record.SessionID = "session-d"
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	peers, err = discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rotated := peers["host-a/session-d"]; rotated.InstanceID == "" || rotated.InstanceID != fallbackInstanceID {
		t.Fatalf("fallback process instance changed across session rotation: %q to %q", fallbackInstanceID, rotated.InstanceID)
	}
	record.FederatedBy = "peer-federator"
	if err := writeJSONAtomic(path, record); err != nil {
		t.Fatal(err)
	}
	peers, err = discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("federated record was re-exported: %#v", peers)
	}
}

func TestDiscoverGrokPeerUsesCorroboratedDynamicPermissionMode(t *testing.T) {
	pid := os.Getpid()
	procStart := processStart(pid)
	if procStart == "" {
		t.Skip("platform does not expose a supported process-start token")
	}
	record := registryRecord{
		PID: pid, SessionID: "grok-dynamic", Name: "grok-peer", Status: "idle", Entrypoint: "grok",
		ProcStart: procStart, PermissionMode: "default", StartedAt: time.Now().UnixMilli(),
	}
	record.MessagingSocketPath = startInspectionFixture(t, peerInspection{
		Type: "peer_inspection", PID: pid, ProcStart: procStart, SessionID: record.SessionID,
		Entrypoint: "grok", PermissionMode: "bypassPermissions",
	}, true)
	registry := writeRegistryFixture(t, record)
	peers, err := discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := peers["host-a/"+record.SessionID].PermissionMode; got != "bypassPermissions" {
		t.Fatalf("corroborated dynamic permission = %q, want bypassPermissions", got)
	}
}

func TestDiscoverGrokPeerRejectsMismatchedInspection(t *testing.T) {
	pid := os.Getpid()
	procStart := processStart(pid)
	if procStart == "" {
		t.Skip("platform does not expose a supported process-start token")
	}
	base := peerInspection{
		Type: "peer_inspection", PID: pid, ProcStart: procStart, SessionID: "grok-mismatch",
		Entrypoint: "grok", PermissionMode: "bypassPermissions",
	}
	tests := map[string]func(*peerInspection, *registryRecord){
		"response pid":         func(response *peerInspection, _ *registryRecord) { response.PID++ },
		"registry pid":         func(_ *peerInspection, record *registryRecord) { record.PID++ },
		"process start":        func(response *peerInspection, _ *registryRecord) { response.ProcStart = "wrong" },
		"session":              func(response *peerInspection, _ *registryRecord) { response.SessionID = "other" },
		"entrypoint":           func(response *peerInspection, _ *registryRecord) { response.Entrypoint = "codex" },
		"permission":           func(response *peerInspection, _ *registryRecord) { response.PermissionMode = "untrusted" },
		"response type":        func(response *peerInspection, _ *registryRecord) { response.Type = "other" },
		"empty registry start": func(_ *peerInspection, record *registryRecord) { record.ProcStart = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			response := base
			record := registryRecord{
				PID: pid, SessionID: base.SessionID, Name: "grok-peer", Status: "idle", Entrypoint: "grok",
				ProcStart: procStart, PermissionMode: "bypassPermissions", StartedAt: time.Now().UnixMilli(),
			}
			mutate(&response, &record)
			record.MessagingSocketPath = startInspectionFixture(t, response, true)
			registry := writeRegistryFixture(t, record)
			peers, err := discoverLocalPeers(registry, "host-a", "alpha")
			if err != nil {
				t.Fatal(err)
			}
			if got := peers["host-a/"+record.SessionID].PermissionMode; got != "bypassPermissions" {
				t.Fatalf("mismatched inspection permission = %q, want conservative bypass", got)
			}
		})
	}
}

func TestDiscoverGrokPeerFallsBackWhenInspectionIsUnresponsive(t *testing.T) {
	pid := os.Getpid()
	procStart := processStart(pid)
	if procStart == "" {
		t.Skip("platform does not expose a supported process-start token")
	}
	record := registryRecord{
		PID: pid, SessionID: "grok-unresponsive", Name: "grok-peer", Status: "idle", Entrypoint: "grok",
		ProcStart: procStart, PermissionMode: "bypassPermissions", StartedAt: time.Now().UnixMilli(),
	}
	record.MessagingSocketPath = startInspectionFixture(t, peerInspection{}, false)
	registry := writeRegistryFixture(t, record)
	peers, err := discoverLocalPeers(registry, "host-a", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := peers["host-a/"+record.SessionID].PermissionMode; got != "bypassPermissions" {
		t.Fatalf("unresponsive inspection permission = %q, want conservative bypass", got)
	}
}

func writeRegistryFixture(t *testing.T, record registryRecord) string {
	t.Helper()
	registry := filepath.Join(t.TempDir(), "sessions")
	if err := ensureDir(registry); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(registry, strconv.Itoa(os.Getpid())+".json"), record); err != nil {
		t.Fatal(err)
	}
	return registry
}

func startInspectionFixture(t *testing.T, response peerInspection, responsive bool) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				var request map[string]any
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				if !responsive {
					time.Sleep(2 * peerInspectionTimeout)
					return
				}
				_ = json.NewEncoder(conn).Encode(response)
			}(conn)
		}
	}()
	return socket
}

func TestGlobalSessionIDStartsWithSessionUniqueData(t *testing.T) {
	host := "workstation-with-a-long-common-prefix"
	first := globalSessionID(host, "00000000-0000-0000-0000-000000000001")
	second := globalSessionID(host, "00000000-0000-0000-0000-000000000002")
	if len(first) < 8 || len(second) < 8 || first[:8] == second[:8] {
		t.Fatalf("disambiguation refs collide: %q and %q", first, second)
	}
	if len(first) > 100 || len(second) > 100 {
		t.Fatalf("global session IDs exceed the registry limit: %d and %d", len(first), len(second))
	}
}

func TestRegistryDiagnosticsReportLiveSessionWithoutMessagingSocket(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "sessions")
	if err := ensureDir(registry); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	record := registryRecord{
		PID: pid, SessionID: "session-without-socket", Name: "unmessageable",
		ProcStart: processStart(pid),
	}
	if err := writeJSONAtomic(filepath.Join(registry, strconv.Itoa(pid)+".json"), record); err != nil {
		t.Fatal(err)
	}
	ready, live, messageable, unmessageable, shadows := inspectRegistry(registry)
	if !ready || live != 1 || messageable != 0 || unmessageable != 1 || shadows != 0 {
		t.Fatalf(
			"registry diagnostics = ready:%v live:%d messageable:%d unmessageable:%d shadows:%d",
			ready, live, messageable, unmessageable, shadows,
		)
	}
}
