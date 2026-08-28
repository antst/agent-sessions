package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/testutil"
)

func TestProductionLegacyInventoryReadsRejectSymlinksAndFIFOsWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	vendorRoot := filepath.Join(root, ".claude")
	if err := os.Mkdir(vendorRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	vendorRecord := filepath.Join(vendorRoot, "vendor.json")
	if err := os.WriteFile(vendorRecord, []byte(`{"value":"vendor-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkRecord := filepath.Join(root, "legacy.json")
	if err := os.Symlink(vendorRecord, symlinkRecord); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if _, err := readProductionLegacyJSON(symlinkRecord, &decoded); err == nil {
		t.Fatal("legacy JSON reader followed a vendor-owned symlink")
	}
	if decoded.Value != "" {
		t.Fatalf("vendor record was decoded through symlink: %q", decoded.Value)
	}
	if _, err := readProductionLegacyServiceDefinition(symlinkRecord); err == nil {
		t.Fatal("legacy service reader followed a vendor-owned symlink")
	}
	nestedVendor := filepath.Join(vendorRoot, "nested")
	if err := os.Mkdir(nestedVendor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedVendor, "state.json"), []byte(`{"value":"nested-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	intermediateSymlink := filepath.Join(root, "legacy-estate")
	if err := os.Symlink(vendorRoot, intermediateSymlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readProductionLegacyJSON(filepath.Join(intermediateSymlink, "nested", "state.json"), &decoded); err == nil {
		t.Fatal("legacy JSON reader followed an intermediate vendor-owned directory symlink")
	}
	if _, err := readProductionLegacyDirectory(vendorRoot); err != nil {
		t.Fatalf("owned real directory read failed: %v", err)
	}
	symlinkDirectory := filepath.Join(root, "legacy-directory")
	if err := os.Symlink(vendorRoot, symlinkDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := readProductionLegacyDirectory(symlinkDirectory); err == nil {
		t.Fatal("legacy directory reader followed a vendor-owned symlink")
	}

	fifo := filepath.Join(root, "legacy.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readProductionLegacyServiceDefinition(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("legacy service reader accepted a FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy service reader blocked on a FIFO")
	}
}

func TestProductionLegacyInventoryReadPinsOpenedInode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	original := []byte(`{"value":"original"}`)
	replacement := []byte(`{"value":"replacement"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := openProductionLegacyRegular(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	if err := os.Rename(path, filepath.Join(root, "original.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := opened.readBounded(1, maxProductionLegacyRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("descriptor read crossed to replacement inode: %s", body)
	}
	current, present, err := observeProductionLegacyRegular(path)
	if err != nil || !present {
		t.Fatalf("observe replacement: present=%v err=%v", present, err)
	}
	if current.same(opened.identity) {
		t.Fatal("test replacement did not change inode identity")
	}
}

func TestProductionLegacyEndpointRetirementPreservesReplacementSocket(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "mfr-", "original.sock")
	endpoint := filepath.Join(root, "legacy.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()

	process := procinfo.Read(os.Getpid())
	evidence := exactLegacyCandidateEvidence("shim", 941)
	evidence.PID, evidence.ProcStart, evidence.StrongStart = os.Getpid(), process.Start, process.StrongStart
	evidence.Process.PID, evidence.Process.ProcStart = os.Getpid(), process.Start
	evidence.Process.StrongStart = process.StrongStart
	evidence.Endpoint = LegacyEndpointObservation{
		Status: "responsive", Path: endpoint, Type: "unix", OwnerUID: os.Getuid(),
		OwnerPID: os.Getpid(), OwnerProcStart: process.Start, RuntimeIdentity: evidence.RuntimeIdentity,
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &productionLegacyRetirementLifecycle{}
	var replacement net.Listener
	lifecycle.beforeArtifactUnlink = func(path string) {
		if path != endpoint || replacement != nil {
			return
		}
		if err := os.Rename(endpoint, filepath.Join(root, "original.sock")); err != nil {
			t.Fatal(err)
		}
		replacement, err = net.Listen("unix", endpoint)
		if err != nil {
			t.Fatal(err)
		}
		replacement.(*net.UnixListener).SetUnlinkOnClose(false)
	}
	defer func() {
		if replacement != nil {
			_ = replacement.Close()
		}
	}()
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil ||
		!strings.Contains(err.Error(), "inode or type changed") {
		t.Fatalf("endpoint replacement retirement error = %v", err)
	}
	identity, present, err := observeProductionLegacyPath(endpoint)
	if err != nil || !present || !identity.ownedSocket() {
		t.Fatalf("replacement endpoint was removed: present=%v identity=%+v err=%v", present, identity, err)
	}
}

func TestProductionLegacyEndpointRetirementDoesNotUnlinkConnectedUnresponsiveSocket(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "mfh-", "hung-supervisor.sock")
	endpoint := filepath.Join(root, "hung-supervisor.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()
	identity, present, err := observeProductionLegacyPath(endpoint)
	if err != nil || !present {
		t.Fatalf("observe hung endpoint: present=%v err=%v", present, err)
	}
	candidate := LegacyRuntimeCandidate{
		SchemaVersion: MigrationSchemaVersion, CandidateID: "hung-supervisor-socket",
		Kind: "supervisor_socket_artifact", SourcePath: endpoint,
		SourceRevision: productionLegacySocketIdentityRevision(identity),
		ProcessStatus:  "absent", EndpointPath: endpoint, EndpointStatus: "absent", EndpointType: "unix",
		EndpointOwnerUID: os.Getuid(), Classification: LegacyClassificationStale,
		EvidenceRevision: 1, LastObservedAt: 1,
	}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	lifecycle := &productionLegacyRetirementLifecycle{}
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil {
		t.Fatal("connected but unresponsive endpoint was retired")
	}
	current, stillPresent, err := observeProductionLegacyPath(endpoint)
	if err != nil || !stillPresent || !current.same(identity) {
		t.Fatalf("hung live endpoint changed: present=%v identity=%+v err=%v", stillPresent, current, err)
	}
}

func TestProductionLegacyRecordRetirementUsesRawRevisionAndRejectsInPlaceMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	original := []byte("{\n  \"sessionId\": \"record-session\",\n  \"prompt\": \"content-canary\"\n}\n")
	write := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newCandidate := func(body []byte) LegacyRuntimeCandidate {
		identity, present, err := observeProductionLegacyRegular(path)
		if err != nil || !present {
			t.Fatalf("observe record artifact: present=%v err=%v", present, err)
		}
		return LegacyRuntimeCandidate{
			SchemaVersion: MigrationSchemaVersion, CandidateID: "legacy-record-session", Kind: "shim",
			SourcePath: path, SourceRevision: "sha256:" + strings.Repeat("a", 64),
			ArtifactRevision: "sha256:" + productionDigest(body),
			ArtifactIdentity: productionLegacyArtifactIdentityRevision(identity),
			ProcessStatus:    "absent", EndpointStatus: "absent", RelatedSessionIDs: []string{"record-session"},
			Classification: LegacyClassificationStale, EvidenceRevision: 1, LastObservedAt: 1,
		}
	}
	write(original)
	lifecycle := &productionLegacyRetirementLifecycle{}
	if err := lifecycle.RetireEndpoint(context.Background(), newCandidate(original)); err != nil {
		t.Fatalf("formatted content-bearing record retirement = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("exact record was not retired: %v", err)
	}

	write(original)
	candidate := newCandidate(original)
	mutated := []byte("{\"sessionId\":\"record-session\",\"prompt\":\"changed-canary\"}")
	lifecycle.beforeArtifactUnlink = func(target string) {
		if target == path {
			if err := os.WriteFile(path, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil ||
		!strings.Contains(err.Error(), "content identity changed") {
		t.Fatalf("in-place record mutation retirement = %v", err)
	}
	remaining, err := os.ReadFile(path)
	if err != nil || string(remaining) != string(mutated) {
		t.Fatalf("mutated record was removed or changed: body=%q err=%v", remaining, err)
	}
}

func TestProductionLegacyServiceRetirementPreservesReplacementDefinition(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	systemctl := filepath.Join(binDir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nprintf 'loaded\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	servicePath := filepath.Join(root, "peer-federator-agent.service")
	body := []byte("[Service]\nExecStart=/opt/agent-sessions agent\n")
	if err := os.WriteFile(servicePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := LegacyCandidateEvidence{
		CandidateID: "candidate-host-agent-service", Kind: "host_agent_service_job", SourcePath: servicePath,
		SourceRevision: "sha256:" + productionDigest(body),
		Process:        LegacyProcessObservation{Status: "absent"},
		Endpoint:       LegacyEndpointObservation{Status: "absent"},
		Service: LegacyServiceObservation{
			Status: "loaded", Manager: "systemd-user", Unit: "peer-federator-agent.service",
			Executable: "/opt/agent-sessions", ArgvRole: "agent",
		},
		EvidenceRevision: 1, ObservedAt: 1,
	}
	candidate, err := ClassifyLegacyCandidate(evidence)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &productionLegacyRetirementLifecycle{}
	lifecycle.beforeArtifactUnlink = func(path string) {
		if path != servicePath {
			return
		}
		if err := os.Rename(servicePath, servicePath+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(servicePath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil ||
		!strings.Contains(err.Error(), "inode or type changed") {
		t.Fatalf("service replacement retirement error = %v", err)
	}
	remaining, err := os.ReadFile(servicePath)
	if err != nil || string(remaining) != string(body) {
		t.Fatalf("replacement service definition was removed or changed: body=%q err=%v", remaining, err)
	}
}

func TestProductionLegacySupervisorRetirementPreservesReplacementRecordAndLock(t *testing.T) {
	for _, swapped := range []string{"record-between-calls", "record", "lock", "lock-fifo"} {
		t.Run(swapped, func(t *testing.T) {
			root := t.TempDir()
			recordPath := filepath.Join(root, "profile", "supervisor.json")
			lockPath := filepath.Join(filepath.Dir(recordPath), "supervisor-start.lock")
			if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
				t.Fatal(err)
			}
			state := productionLegacySupervisorState{
				ControlSocket: filepath.Join(root, "runtime", "supervisor.sock"), PID: 777,
				ProcStart: "start-777", RuntimeIdentity: "sha256:runtime-777", PluginVersion: "0.2.4",
			}
			body, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(recordPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			evidence := exactLegacyCandidateEvidence("supervisor", 777)
			evidence.SourcePath, evidence.SourceRevision = recordPath, "sha256:"+productionDigest(body)
			evidence.RuntimeIdentity = state.RuntimeIdentity
			evidence.PID, evidence.ProcStart = state.PID, state.ProcStart
			evidence.Process.PID, evidence.Process.ProcStart = state.PID, state.ProcStart
			evidence.Endpoint.Path = state.ControlSocket
			evidence.Endpoint.OwnerPID, evidence.Endpoint.OwnerProcStart = state.PID, state.ProcStart
			evidence.Endpoint.RuntimeIdentity = state.RuntimeIdentity
			candidate, err := ClassifyLegacyCandidate(evidence)
			if err != nil {
				t.Fatal(err)
			}
			lifecycle := &productionLegacyRetirementLifecycle{}
			target := recordPath
			replacement := body
			if strings.HasPrefix(swapped, "lock") {
				target, replacement = lockPath, nil
			}
			replace := func() {
				if err := os.Rename(target, target+".original"); err != nil {
					t.Fatal(err)
				}
				if swapped == "lock-fifo" {
					if err := unix.Mkfifo(target, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(target, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if swapped == "record-between-calls" {
				if _, err := lifecycle.ReattestEndpoint(context.Background(), candidate); err != nil {
					t.Fatal(err)
				}
				replace()
			} else {
				lifecycle.beforeArtifactUnlink = func(path string) {
					if path == target {
						replace()
					}
				}
			}
			if err := lifecycle.RetireEndpoint(context.Background(), candidate); err == nil ||
				(!strings.Contains(err.Error(), "inode or type changed") &&
					!strings.Contains(err.Error(), "owned regular file")) {
				t.Fatalf("%s replacement retirement error = %v", swapped, err)
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("replacement %s was removed: %v", swapped, err)
			}
			if _, err := os.Lstat(recordPath); err != nil {
				t.Fatalf("record disappeared during ambiguous retirement: %v", err)
			}
		})
	}
}
