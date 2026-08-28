package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestRemoteLaneNormalizesEveryProductOnlyAtDestination(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	adapters := make(map[string]LaneAdapter)
	normalizers := make(map[string]*remoteTargetNormalizationTestAdapter)
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		normalizer := &remoteTargetNormalizationTestAdapter{
			laneTestAdapter: &laneTestAdapter{dispatches: make(map[string]int), archives: make(map[string]int)},
			product:         product,
		}
		normalizers[product], adapters[product] = normalizer, normalizer
	}
	options := fixture.options()
	options.Adapters = adapters
	target, err := NewLaneEngine(options)
	if err != nil {
		t.Fatal(err)
	}
	component := &federationComponent{
		lanes: target, remoteRequests: make(map[string]string),
		status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 17}, HostID: "host-target"},
	}
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		t.Run(product, func(t *testing.T) {
			request := federation.RemoteLaneRequest{
				RequestID: "request-target-" + product, SourceID: "host-source/parent", TargetHostID: "host-target",
				Product: product, LaneSessionID: "lane-target-" + product, TurnID: "turn-target-" + product,
				Name: product + "-worker", Cwd: ".", ParentHostID: "host-source", ParentSessionID: "parent",
				ParentProduct: "codex", ParentInstanceID: "parent-instance", ParentPermissionMode: "default",
				PermissionMode: "default", InputReference: map[string]any{
					"prompt": "work", "options": map[string]any{
						"command": "start", "arguments": []any{"--name", product + "-worker", "--native-target-option"},
					},
				},
			}
			if _, err := component.StartRemoteLane(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			lane, err := target.ReadLane(context.Background(), request.LaneSessionID)
			if err != nil {
				t.Fatal(err)
			}
			wantCwd := filepath.Join("/target", product)
			if lane.Cwd != wantCwd {
				t.Fatalf("destination cwd = %q, want %q", lane.Cwd, wantCwd)
			}
			turn, err := target.ReadTurn(context.Background(), request.LaneSessionID, request.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			native := attachmentEvidenceMap(attachmentEvidenceMap(turn.InputReference["options"])["native"])
			if native["normalized_on"] != "host-target" || native["product"] != product {
				t.Fatalf("target-native options = %#v", native)
			}
			if got := attachmentEvidenceMap(request.InputReference["options"])["native"]; got != nil {
				t.Fatalf("source request was mutated with native options: %#v", got)
			}
			if _, err := target.Complete(context.Background(), LaneTerminalRequest{
				LaneSessionID: request.LaneSessionID, TurnID: request.TurnID, Outcome: LaneDispatchCompleted,
			}); err != nil {
				t.Fatal(err)
			}
			resume := request
			resume.RequestID += "-resume"
			resume.TurnID += "-resume"
			resume.Cwd = "/source-host/path-must-not-replace-target-state"
			resume.InputReference = map[string]any{
				"prompt": "follow up", "options": map[string]any{"command": "resume", "arguments": []string{"--model", "target-model"}},
			}
			if _, err := component.StartRemoteLane(context.Background(), resume); err != nil {
				t.Fatal(err)
			}
			calls := normalizers[product].callsSnapshot()
			if len(calls) != 2 || calls[0].Cwd == "/source-host/path-must-not-replace-target-state" ||
				calls[1].Cwd != wantCwd || calls[1].NativeActor["lane"] != request.LaneSessionID {
				t.Fatalf("target normalization calls = %#v", calls)
			}
		})
	}
}

func TestRemoteLaneArchiveRunsOnlyDestinationAdapterAndRecordsSourceAcknowledgement(t *testing.T) {
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		t.Run(product, func(t *testing.T) {
			suffix := "-" + product
			sourceFixture := newLaneTestFixture(t, nil, nil)
			parent := sourceFixture.attach(t, "codex", "parent-archive"+suffix, []string{"session:host-test/parent-archive" + suffix})
			_, _, err := sourceFixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
				TargetHostID: "host-target",
				Request: LaneStartRequest{
					LaneSessionID: "lane-archive-remote" + suffix, TurnID: "turn-archive-remote" + suffix,
					SourceAttachmentID: parent.AttachmentID, Product: product, Name: "remote-archive" + suffix,
					Cwd: ".", PermissionMode: "default", InputReference: map[string]any{"prompt": "work"},
					RemoteRequestID: "request-archive-remote" + suffix,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := sourceFixture.engine.MarkRemoteSourceRunning(context.Background(), "request-archive-remote"+suffix, 1); err != nil {
				t.Fatal(err)
			}
			if err := (&federationComponent{lanes: sourceFixture.engine}).acceptRemoteLaneResponse(context.Background(), federation.RemoteLaneResponse{
				Type: "lane_exit", RequestID: "request-archive-remote" + suffix, ExitCode: 0,
			}); err != nil {
				t.Fatal(err)
			}

			targetFixture := newLaneTestFixture(t, nil, nil)
			target := &federationComponent{
				lanes: targetFixture.engine, remoteRequests: make(map[string]string),
				status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 1}, HostID: "host-target"},
			}
			if _, err := target.StartRemoteLane(context.Background(), federation.RemoteLaneRequest{
				RequestID: "request-archive-remote" + suffix, SourceID: "host-test/parent-archive" + suffix,
				TargetHostID: "host-target", Product: product, LaneSessionID: "lane-archive-remote" + suffix,
				TurnID: "turn-archive-remote" + suffix, Name: "remote-archive" + suffix, Cwd: ".",
				ParentHostID: "host-test", ParentSessionID: "parent-archive" + suffix, ParentProduct: "codex",
				ParentInstanceID: "parent-instance" + suffix, ParentPermissionMode: "default", PermissionMode: "default",
				InputReference: map[string]any{"prompt": "work", "options": map[string]any{"command": "start"}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := targetFixture.engine.Complete(context.Background(), LaneTerminalRequest{
				LaneSessionID: "lane-archive-remote" + suffix, TurnID: "turn-archive-remote" + suffix,
				Outcome: LaneDispatchCompleted,
			}); err != nil {
				t.Fatal(err)
			}
			ack, err := target.archiveRemoteLane(context.Background(), federation.RemoteLaneArchive{
				RequestID: "archive-operation" + suffix, RemoteRequestID: "request-archive-remote" + suffix,
				SourceID: "host-test/parent-archive" + suffix, TargetHostID: "host-target", Product: product,
				LaneSessionID: "lane-archive-remote" + suffix,
			})
			if err != nil || ack.ArchiveRevision != 1 || targetFixture.adapter.archiveCount("lane-archive-remote"+suffix) != 1 {
				t.Fatalf("destination archive = %#v, %v; target calls=%d", ack, err, targetFixture.adapter.archiveCount("lane-archive-remote"+suffix))
			}
			archived, err := sourceFixture.engine.ArchiveRemoteSource(context.Background(), "lane-archive-remote"+suffix, "host-target", ack.ArchiveRevision)
			if err != nil || archived.State != LaneStateArchived || sourceFixture.adapter.archiveCount("lane-archive-remote"+suffix) != 0 {
				t.Fatalf("source archive acknowledgement = %#v, %v; source calls=%d", archived, err, sourceFixture.adapter.archiveCount("lane-archive-remote"+suffix))
			}
		})
	}
}

func TestRemoteLaneLifecycleSelectorsAndDoctorUseExactTargetHost(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-lifecycle", []string{"session:host-test/parent-lifecycle"})
	for _, host := range []string{"host-a", "host-b"} {
		_, _, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
			TargetHostID: host,
			Request: LaneStartRequest{
				LaneSessionID: "lane-" + host, TurnID: "turn-" + host, SourceAttachmentID: parent.AttachmentID,
				Product: "qwen", Name: "worker-" + host, Cwd: ".", PermissionMode: "default",
				InputReference: map[string]any{"prompt": "work"}, RemoteRequestID: "request-" + host,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	component := &federationComponent{lanes: fixture.engine}
	listed, err := component.executeRemoteLaneCommand(context.Background(), fixture.engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "qwen", Command: "list", Host: "host-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	lanes := listed["lanes"].([]LaneRecord)
	if len(lanes) != 1 || lanes[0].RemoteHostID != "host-a" {
		t.Fatalf("host-filtered list = %#v", lanes)
	}
	if _, err := component.executeRemoteLaneCommand(context.Background(), fixture.engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "qwen", Command: "status", Host: "host-a", Arguments: []string{"lane-host-b"},
	}); !errors.Is(err, ErrLaneNotFound) {
		t.Fatalf("cross-host status error = %v", err)
	}
	roster := federation.Roster{Hosts: []federation.Host{
		{ID: "host-a", Name: "host-a", Capabilities: []string{"codex-lane", "claude-lane", "grok-lane", "qwen-lane"}},
		{ID: "host-b", Name: "host-b", Capabilities: []string{"codex-lane"}},
	}}
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		if ready, err := remoteLaneDoctorReady(roster, "host-a", product); err != nil || !ready {
			t.Fatalf("%s target doctor ready = %t, %v", product, ready, err)
		}
	}
	if ready, err := remoteLaneDoctorReady(roster, "host-b", "qwen"); err == nil || ready || !strings.Contains(err.Error(), "does not advertise") {
		t.Fatalf("target doctor capability refusal = %t, %v", ready, err)
	}
}

func TestRemoteLaneNameSelectorsFilterTargetHostBeforeAmbiguityForEveryLifecycleOperation(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-selector", []string{"shared", "session:host-test/parent-selector"})
	accept := func(source AttachmentRecord, host, laneID, name string) {
		t.Helper()
		_, _, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
			TargetHostID: host,
			Request: LaneStartRequest{
				LaneSessionID: laneID, TurnID: "turn-" + laneID, SourceAttachmentID: source.AttachmentID,
				Product: "qwen", Name: name, Cwd: ".", Groups: []string{"shared"}, PermissionMode: "default",
				InputReference: map[string]any{"prompt": "work"}, AllowDuplicateName: true,
				RemoteRequestID: "request-" + laneID,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	accept(parent, "host-a", "lane-selector-a", "same-worker")
	accept(parent, "host-b", "lane-selector-b", "same-worker")

	for _, operation := range []struct {
		name            string
		includeArchived bool
	}{
		{name: "wait", includeArchived: true},
		{name: "status", includeArchived: true},
		{name: "resume", includeArchived: true},
		{name: "interrupt", includeArchived: false},
		{name: "archive", includeArchived: true},
	} {
		t.Run(operation.name, func(t *testing.T) {
			lane, err := resolveRemoteLaneCommandSelector(
				context.Background(), fixture.engine, parent.AttachmentID,
				LaneCommandRequest{Product: "qwen", Command: operation.name, Host: "host-a"},
				"same-worker", operation.includeArchived, false,
			)
			if err != nil || lane.LaneSessionID != "lane-selector-a" || lane.RemoteHostID != "host-a" {
				t.Fatalf("%s selector = %#v, %v", operation.name, lane, err)
			}
		})
	}

	otherParent := fixture.attach(t, "claude", "other-selector", []string{"shared", "session:host-test/other-selector"})
	accept(parent, "host-a", "lane-mine-parent", "mine-worker")
	accept(otherParent, "host-a", "lane-mine-other", "mine-worker")
	request := LaneCommandRequest{Product: "qwen", Command: "status", Host: "host-a"}
	if _, err := resolveRemoteLaneCommandSelector(
		context.Background(), fixture.engine, parent.AttachmentID, request, "mine-worker", true, false,
	); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("unqualified same-host duplicate error = %v", err)
	}
	mine, err := resolveRemoteLaneCommandSelector(
		context.Background(), fixture.engine, parent.AttachmentID, request, "mine-worker", true, true,
	)
	if err != nil || mine.LaneSessionID != "lane-mine-parent" ||
		mine.ParentHostID != parent.HostID || mine.ParentSessionID != parent.SessionID {
		t.Fatalf("remote --mine selector = %#v, %v", mine, err)
	}
}

func TestRemoteLaneDuplicateNamePolicyIsEnforcedOnlyByDestinationAuthority(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	component := &federationComponent{
		lanes: fixture.engine, remoteRequests: make(map[string]string),
		status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 17}, HostID: "host-target"},
	}
	request := federation.RemoteLaneRequest{
		RequestID: "request-duplicate-first", SourceID: "host-source/parent", TargetHostID: "host-target",
		Product: "qwen", LaneSessionID: "lane-duplicate-first", TurnID: "turn-duplicate-first",
		Name: "duplicate-worker", Cwd: ".", ParentHostID: "host-source", ParentSessionID: "parent",
		ParentProduct: "codex", ParentInstanceID: "parent-instance", ParentPermissionMode: "default",
		PermissionMode: "default", InputReference: map[string]any{
			"prompt": "work", "options": map[string]any{"command": "start"},
		},
	}
	if _, err := component.StartRemoteLane(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.RequestID, conflict.LaneSessionID, conflict.TurnID =
		"request-duplicate-conflict", "lane-duplicate-conflict", "turn-duplicate-conflict"
	if _, err := component.StartRemoteLane(context.Background(), conflict); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("destination duplicate-name error = %v", err)
	}
	allowed := conflict
	allowed.RequestID, allowed.LaneSessionID, allowed.TurnID =
		"request-duplicate-allowed", "lane-duplicate-allowed", "turn-duplicate-allowed"
	allowed.AllowDuplicateName = true
	if _, err := component.StartRemoteLane(context.Background(), allowed); err != nil {
		t.Fatalf("destination rejected explicit duplicate-name admission: %v", err)
	}
	if fixture.adapter.dispatchCount("turn-duplicate-first") != 1 ||
		fixture.adapter.dispatchCount("turn-duplicate-conflict") != 0 ||
		fixture.adapter.dispatchCount("turn-duplicate-allowed") != 1 {
		t.Fatalf("destination dispatch counts first=%d conflict=%d allowed=%d",
			fixture.adapter.dispatchCount("turn-duplicate-first"),
			fixture.adapter.dispatchCount("turn-duplicate-conflict"),
			fixture.adapter.dispatchCount("turn-duplicate-allowed"))
	}
}

func TestRemoteLanePromptFileIsRejectedBeforeDurableSourceAcceptance(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-prompt-file", []string{"session:host-test/parent-prompt-file"})
	component := &federationComponent{lanes: fixture.engine}
	_, err := component.executeRemoteLaneCommand(
		context.Background(), fixture.engine, fixture.attachments, parent.AttachmentID,
		LaneCommandRequest{
			Product: "qwen", Command: "start", Host: "host-b",
			Arguments: []string{"--name", "worker", "--prompt-file", "/source-only/prompt.txt"}, Input: "already-read-source",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "not supported for remote lanes") ||
		!strings.Contains(err.Error(), "stdin") {
		t.Fatalf("remote prompt-file error = %v", err)
	}
	if turns := fixture.engine.RemoteTurns(true); len(turns) != 0 {
		t.Fatalf("unsupported remote prompt-file mutated durable state: %#v", turns)
	}
}

func TestRemoteLaneSourceRecoveryNeverRedispatchesAcceptedWork(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-remote", []string{"project", "session:host-test/parent-remote"})
	request := RemoteLaneSourceRequest{
		TargetHostID: "host-b",
		Request: LaneStartRequest{
			LaneSessionID: "remote-lane", TurnID: "remote-turn", SourceAttachmentID: parent.AttachmentID,
			Product: "qwen", Name: "remote-worker", Cwd: "/workspace", Groups: []string{"shared"},
			InheritParentGroups: true, PermissionMode: "bypassPermissions",
			InputReference: map[string]any{"input_id": "input-remote"}, RemoteRequestID: "request-remote",
		},
	}
	lane, turn, err := fixture.engine.AcceptRemoteSource(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if lane.RemoteHostID != "host-b" || turn.RemoteRequestID != "request-remote" ||
		turn.DispatchState != LaneDispatchAccepted || fixture.adapter.callCount() != 0 {
		t.Fatalf("source acceptance = lane %#v turn %#v adapter calls %d", lane, turn, fixture.adapter.callCount())
	}
	wantGroups := laneTestSortedUnique([]string{
		"shared", "project", "session:host-test/parent-remote", "session:host-b/remote-lane",
	})
	if !reflect.DeepEqual(lane.Groups, wantGroups) || lane.ParentHostID != parent.HostID ||
		lane.ParentSessionID != parent.SessionID || lane.PermissionMode != "bypassPermissions" {
		t.Fatalf("remote parent/group propagation = %#v, want groups %q", lane, wantGroups)
	}
	if _, _, err := fixture.engine.MarkRemoteSourceRunning(context.Background(), "request-remote", 7); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatalf("recover source proxy: %v", err)
	}
	if fixture.adapter.callCount() != 0 {
		t.Fatalf("source recovery invoked a local adapter %d time(s)", fixture.adapter.callCount())
	}
	component := &federationComponent{lanes: restarted}
	response := federation.RemoteLaneResponse{Type: "lane_exit", RequestID: "request-remote", ExitCode: 0}
	if err := component.acceptRemoteLaneResponse(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	if err := component.acceptRemoteLaneResponse(context.Background(), response); err != nil {
		t.Fatalf("duplicate terminal response was not idempotent: %v", err)
	}
	collection, err := restarted.Collect(context.Background(), LaneCollectRequest{
		LaneSessionID: "remote-lane", TurnID: "remote-turn", SourceAttachmentID: parent.AttachmentID,
	})
	if err != nil || collection.Outcome != LaneDispatchCompleted || fixture.adapter.callCount() != 0 {
		t.Fatalf("remote collection = %#v, %v; adapter calls %d", collection, err, fixture.adapter.callCount())
	}
}

func TestRemoteLaneLostWireAcceptancePreservesDurableUncertainty(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-wire-loss", []string{"session:host-test/parent-wire-loss"})
	server, client := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer func() { _ = server.Close() }()
		scanner := bufio.NewScanner(server)
		if !scanner.Scan() { // hello
			return
		}
		_, _ = server.Write([]byte(`{"type":"hello_ok","version":3}` + "\n"))
		if !scanner.Scan() { // snapshot
			return
		}
		_ = scanner.Scan() // lane_exec, then lose its acceptance
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{}, 1)
	agent, err := federation.NewHostAgent(federation.HostAgentOptions{
		HubAddress: "hub.invalid:7443", HostID: "host-test", HostName: "host-test",
		DialContext:    func(context.Context, string, string) (net.Conn, error) { return client, nil },
		InitialBackoff: time.Hour, MaximumBackoff: time.Hour, DeliveryTimeout: time.Second,
		Callbacks: federation.AgentCallbacks{
			Snapshot: func(context.Context) ([]federation.Peer, error) { return nil, nil },
			StateChanged: func(status federation.AgentStatus) {
				if status.State == federation.AgentConnected {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- agent.Run(ctx) }()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("source host did not connect")
	}
	component := &federationComponent{
		client: agent, lanes: fixture.engine,
		status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 17}, HostID: "host-test", HostName: "host-test"},
	}
	_, err = component.executeRemoteLaneCommand(context.Background(), fixture.engine, fixture.attachments, parent.AttachmentID, LaneCommandRequest{
		Product: "qwen", Command: "start", Host: "host-b", Arguments: []string{"--name", "wire-loss"}, Input: "work",
	})
	if err == nil || !strings.Contains(err.Error(), "acceptance is unconfirmed") {
		t.Fatalf("lost acceptance error = %v", err)
	}
	turns := fixture.engine.RemoteTurns(true)
	if len(turns) != 1 || turns[0].DispatchState != LaneDispatchAccepted || turns[0].TerminalOutcome != "" ||
		fixture.adapter.callCount() != 0 {
		t.Fatalf("lost acceptance durable state = %#v; adapter calls %d", turns, fixture.adapter.callCount())
	}
	<-serverDone
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLaneSourceRestartReplaysExactEnvelopeWithoutNativeRedispatch(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-replay", []string{"global-project", "session:host-test/parent-replay"})
	envelope := federation.RemoteLaneEnvelope{
		RequestID: "request-replay", SourceID: "host-test/parent-replay", TargetHostID: "host-b",
		Parent: federation.Peer{
			ID: "host-test/parent-replay", HostID: "host-test", HostName: "host-test", SessionID: "parent-replay",
			InstanceID: parent.AttachmentID, Entrypoint: "codex", PermissionMode: "default",
			Groups: []string{"global-project", "session:host-test/parent-replay"},
		},
		Product: "qwen", LaneSessionID: "lane-replay", TurnID: "turn-replay", Name: "worker", Cwd: "/workspace",
		PermissionMode: "default", InputReference: map[string]any{"prompt": "resume exact accepted work"},
	}
	evidence, err := remoteLaneEnvelopeEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
		TargetHostID: "host-b",
		Request: LaneStartRequest{
			LaneSessionID: envelope.LaneSessionID, TurnID: envelope.TurnID, SourceAttachmentID: parent.AttachmentID,
			Product: envelope.Product, Name: envelope.Name, Cwd: envelope.Cwd, PermissionMode: envelope.PermissionMode,
			InputReference: envelope.InputReference, RemoteRequestID: envelope.RequestID, RemoteEnvelope: evidence,
		},
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	serverDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer func() { _ = server.Close() }()
		scanner := bufio.NewScanner(server)
		if !scanner.Scan() {
			serverDone <- scanner.Err()
			return
		}
		if _, err := server.Write([]byte(`{"type":"hello_ok","version":3}` + "\n")); err != nil {
			serverDone <- err
			return
		}
		if !scanner.Scan() { // snapshot
			serverDone <- scanner.Err()
			return
		}
		if _, err := server.Write([]byte(`{"type":"roster","version":3}` + "\n")); err != nil {
			serverDone <- err
			return
		}
		if !scanner.Scan() {
			serverDone <- scanner.Err()
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			serverDone <- err
			return
		}
		body, _ := json.Marshal(frame["remote_lane"])
		var replay federation.RemoteLaneEnvelope
		if err := json.Unmarshal(body, &replay); err != nil || !reflect.DeepEqual(replay, envelope) {
			serverDone <- errors.New("recovered source did not replay its exact durable envelope")
			return
		}
		accepted := federation.RemoteLaneAccepted{
			RequestID: envelope.RequestID, LaneSessionID: envelope.LaneSessionID,
			TurnID: envelope.TurnID, AcceptedRevision: 9,
		}
		acceptedBody, _ := json.Marshal(map[string]any{
			"type": "lane_accepted", "request_id": envelope.RequestID, "remote_accepted": accepted,
		})
		if _, err := server.Write(append(acceptedBody, '\n')); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
		<-ctx.Done()
	}()
	component, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{HostID: "host-test", HostName: "host-test", HubAddress: "hub.invalid:7443", RemoteLanesEnabled: true},
		generation:    17, runtimeVersion: "test", runtimeIdentity: "sha256:test",
		attachments: fixture.attachments, lanes: restarted, laneAdapters: fixture.options().Adapters,
		dialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- component.Run(ctx) }()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovered source request was not replayed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, turn, readErr := restarted.RemoteTurn(envelope.RequestID)
		if readErr == nil && turn.DispatchState == LaneDispatchRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed source did not converge to running: %#v, %v", turn, readErr)
		}
		time.Sleep(time.Millisecond)
	}
	if fixture.adapter.callCount() != 0 {
		t.Fatalf("source replay redispatched a native adapter %d time(s)", fixture.adapter.callCount())
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLaneDestinationRecoveryReconstructsRequestProvenance(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	component := &federationComponent{
		lanes: fixture.engine, remoteRequests: make(map[string]string),
		status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 17}, HostID: "host-b"},
	}
	accepted, err := component.StartRemoteLane(context.Background(), federation.RemoteLaneRequest{
		RequestID: "request-destination", SourceID: "host-a/parent", TargetHostID: "host-b",
		Product: "claude", LaneSessionID: "lane-destination", TurnID: "turn-destination",
		Name: "worker", Cwd: "/workspace", ParentHostID: "host-a", ParentSessionID: "parent",
		ParentProduct: "codex", ParentInstanceID: "parent-instance", ParentPermissionMode: "default",
		ParentGroups: []string{"global-project"}, Groups: []string{"child"}, InheritParentGroups: true,
		PermissionMode: "default", InputReference: map[string]any{"input_id": "input-destination"},
	})
	if err != nil || accepted.RequestID != "request-destination" || fixture.adapter.dispatchCount("turn-destination") != 1 {
		t.Fatalf("destination acceptance = %#v, %v", accepted, err)
	}
	turn, err := fixture.engine.ReadTurn(context.Background(), "lane-destination", "turn-destination")
	if err != nil || turn.RemoteRequestID != "request-destination" {
		t.Fatalf("destination provenance = %#v, %v", turn, err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	published := 0
	recovered, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{HostID: "host-b", HostName: "host-b", RemoteLanesEnabled: true},
		generation:    17, runtimeVersion: "test", runtimeIdentity: "sha256:test",
		attachments: fixture.attachments, lanes: restarted, laneAdapters: fixture.options().Adapters,
		PublishRemoteLaneResult: func(_ context.Context, result federation.RemoteLaneResult) error {
			if result.ResultReference["result_id"] != "result-destination" {
				t.Fatalf("destination result evidence = %#v", result.ResultReference)
			}
			return nil
		},
		PublishRemoteLaneNotice: func(_ context.Context, notice federation.RemoteLaneNotice) error {
			published++
			if notice.RequestID != "request-destination" || notice.TargetHostID != "host-a" || notice.TargetSessionID != "parent" {
				t.Fatalf("recovered terminal notice = %#v", notice)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.remoteRequests[laneTurnKey("lane-destination", "turn-destination")]; got != "request-destination" {
		t.Fatalf("recovered destination request = %q", got)
	}
	if fixture.adapter.dispatchCount("turn-destination") != 1 {
		t.Fatalf("destination was redispatched during federation recovery")
	}
	terminal, err := restarted.Complete(context.Background(), LaneTerminalRequest{
		LaneSessionID: "lane-destination", TurnID: "turn-destination", Outcome: LaneDispatchCompleted,
		ResultReference: map[string]any{"result_id": "result-destination"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := federation.RemoteLaneResult{
		RequestID: "request-destination", LaneSessionID: "lane-destination", TurnID: "turn-destination",
		Outcome: terminal.TerminalOutcome, ResultReference: terminal.ResultReference,
	}
	if _, err := recovered.PublishRemoteLaneResult(context.Background(), result); err != nil {
		t.Fatalf("publish terminal after destination restart: %v", err)
	}
	if _, err := recovered.PublishRemoteLaneResult(context.Background(), result); err != nil {
		t.Fatalf("repeat recovered terminal publication: %v", err)
	}
	if published != 1 {
		t.Fatalf("terminal notice publications = %d, want exactly one", published)
	}
	acknowledged, err := restarted.ReadTurn(context.Background(), "lane-destination", "turn-destination")
	if err != nil || acknowledged.RemoteNoticeAcknowledgedAt == 0 {
		t.Fatalf("durable terminal outbox acknowledgement = %#v, %v", acknowledged, err)
	}
}

func TestRemoteLaneDestinationRestartReconcilesDurableTerminalOutbox(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	component := &federationComponent{
		lanes: fixture.engine, remoteRequests: make(map[string]string),
		status: FederationStateRecord{RecordHeader: RecordHeader{Generation: 17}, HostID: "host-b"},
	}
	if _, err := component.StartRemoteLane(context.Background(), federation.RemoteLaneRequest{
		RequestID: "request-outbox-restart", SourceID: "host-a/parent", TargetHostID: "host-b",
		Product: "qwen", LaneSessionID: "lane-outbox-restart", TurnID: "turn-outbox-restart",
		Name: "worker", Cwd: "/workspace", ParentHostID: "host-a", ParentSessionID: "parent",
		ParentProduct: "codex", ParentInstanceID: "parent-instance", ParentPermissionMode: "default",
		PermissionMode: "default", InputReference: map[string]any{"input_id": "outbox-restart"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.Complete(context.Background(), LaneTerminalRequest{
		LaneSessionID: "lane-outbox-restart", TurnID: "turn-outbox-restart", Outcome: LaneDispatchCompleted,
		ResultReference: map[string]any{"native_result_id": "result-outbox-restart"},
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	resultCalls, noticeCalls := 0, 0
	recovered, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{HostID: "host-b", HostName: "host-b", RemoteLanesEnabled: true},
		generation:    18, runtimeVersion: "test", runtimeIdentity: "sha256:test",
		attachments: fixture.attachments, lanes: restarted, laneAdapters: fixture.options().Adapters,
		PublishRemoteLaneResult: func(_ context.Context, result federation.RemoteLaneResult) error {
			resultCalls++
			if resultCalls == 1 {
				return errors.New("simulated lost result acknowledgement")
			}
			if result.ResultReference["native_result_id"] != "result-outbox-restart" {
				t.Fatalf("restarted result evidence = %#v", result.ResultReference)
			}
			return nil
		},
		PublishRemoteLaneNotice: func(_ context.Context, notice federation.RemoteLaneNotice) error {
			noticeCalls++
			if notice.RequestID != "request-outbox-restart" {
				t.Fatalf("restarted terminal notice = %#v", notice)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, queued := recovered.recoveredTerminalOutbox["request-outbox-restart"]; !queued {
		t.Fatal("restart did not reconstruct the durable terminal outbox")
	}
	recovered.reconcileRecoveredRemoteSources(context.Background())
	if resultCalls != 2 || noticeCalls != 1 {
		t.Fatalf("restart reconciliation result=%d notice=%d, want retry 2/1", resultCalls, noticeCalls)
	}
	turn, err := restarted.ReadTurn(context.Background(), "lane-outbox-restart", "turn-outbox-restart")
	if err != nil || turn.RemoteNoticeAcknowledgedAt == 0 {
		t.Fatalf("restart outbox acknowledgement = %#v, %v", turn, err)
	}
	recovered.reconcileRecoveredRemoteSources(context.Background())
	if resultCalls != 2 || noticeCalls != 1 {
		t.Fatalf("acknowledged restart outbox replayed result=%d notice=%d", resultCalls, noticeCalls)
	}
	if fixture.adapter.dispatchCount("turn-outbox-restart") != 1 {
		t.Fatalf("restart reconciliation redispatched native work %d times", fixture.adapter.dispatchCount("turn-outbox-restart"))
	}
}

func TestRemoteLaneTerminalObservationSchedulesDurableOutboxRetry(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	resultCalls, noticeCalls := 0, 0
	component, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{HostID: "host-b", HostName: "host-b", RemoteLanesEnabled: true},
		generation:    19, runtimeVersion: "test", runtimeIdentity: "sha256:test",
		attachments: fixture.attachments, lanes: fixture.engine, laneAdapters: fixture.options().Adapters,
		PublishRemoteLaneResult: func(_ context.Context, _ federation.RemoteLaneResult) error {
			resultCalls++
			if resultCalls == 1 {
				return errors.New("simulated in-session acknowledgement loss")
			}
			return nil
		},
		PublishRemoteLaneNotice: func(_ context.Context, _ federation.RemoteLaneNotice) error {
			noticeCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := component.DispatchRemoteLane(context.Background(), federation.RemoteLaneEnvelope{
		RequestID: "request-observed-outbox", SourceID: "host-a/parent", TargetHostID: "host-b",
		Parent: federation.Peer{
			ID: "host-a/parent", HostID: "host-a", SessionID: "parent", Entrypoint: "codex",
			InstanceID: "parent-instance", PermissionMode: "default",
		},
		Product: "claude", LaneSessionID: "lane-observed-outbox", TurnID: "turn-observed-outbox",
		Name: "worker", Cwd: "/workspace", PermissionMode: "default",
		InputReference: map[string]any{"input_id": "observed-outbox"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.Complete(context.Background(), LaneTerminalRequest{
		LaneSessionID: "lane-observed-outbox", TurnID: "turn-observed-outbox", Outcome: LaneDispatchCompleted,
		ResultReference: map[string]any{"native_result_id": "observed-result"},
	}); err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	component.mu.Lock()
	component.lifetimeContext = lifetime
	component.mu.Unlock()
	component.observeLaneTerminal(LaneObservation{
		LaneSessionID: "lane-observed-outbox", TurnID: "turn-observed-outbox", Outcome: LaneDispatchCompleted,
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		turn, readErr := fixture.engine.ReadTurn(context.Background(), "lane-observed-outbox", "turn-observed-outbox")
		if readErr == nil && turn.RemoteNoticeAcknowledgedAt != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-session terminal outbox was not retried: %#v, %v; result=%d notice=%d queue=%#v reconciling=%v",
				turn, readErr, resultCalls, noticeCalls, component.recoveredTerminalOutbox, component.reconcilingRemote)
		}
		time.Sleep(time.Millisecond)
	}
	if resultCalls != 2 || noticeCalls != 1 {
		t.Fatalf("in-session retry result=%d notice=%d, want 2/1", resultCalls, noticeCalls)
	}
}

func TestRemoteLaneResultReferenceAndTerminalSourceOwnershipSurviveCollection(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-result", []string{"session:host-test/parent-result"})
	_, _, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
		TargetHostID: "host-b",
		Request: LaneStartRequest{
			LaneSessionID: "lane-result", TurnID: "turn-result", SourceAttachmentID: parent.AttachmentID,
			Product: "qwen", Name: "worker", Cwd: "/workspace", PermissionMode: "default",
			InputReference: map[string]any{"input_id": "result"}, RemoteRequestID: "request-result",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &federationComponent{lanes: fixture.engine}
	result := &federation.RemoteLaneResult{
		RequestID: "request-result", LaneSessionID: "lane-result", TurnID: "turn-result", Outcome: LaneDispatchCompleted,
		ResultReference: map[string]any{"native_result_id": "destination-result"},
	}
	if err := component.acceptRemoteLaneResponse(context.Background(), federation.RemoteLaneResponse{
		Type: "lane_result", RequestID: result.RequestID, Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	notice := federation.RemoteLaneNotice{
		NoticeID: "notice-result", RequestID: result.RequestID, TargetHostID: "host-test", TargetSessionID: parent.SessionID,
		LaneSessionID: result.LaneSessionID, TurnID: result.TurnID, Outcome: result.Outcome,
	}
	noticeBody, _ := json.Marshal(notice)
	frameBody, _ := json.Marshal(federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "delivery", Content: string(noticeBody)})
	forged := federation.RoutedDelivery{
		SourceID: "host-forged/lane-result", TargetID: "host-test/" + parent.SessionID, Frame: frameBody,
	}
	if err := component.acceptRemoteLaneNotice(context.Background(), forged); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("forged terminal source error = %v", err)
	}
	valid := forged
	valid.SourceID = "host-b/lane-result"
	if err := component.acceptRemoteLaneNotice(context.Background(), valid); err == nil || !strings.Contains(err.Error(), "delivery authority") {
		t.Fatalf("terminal staging error = %v", err)
	}
	collection, err := fixture.engine.Collect(context.Background(), LaneCollectRequest{
		LaneSessionID: "lane-result", TurnID: "turn-result", SourceAttachmentID: parent.AttachmentID,
	})
	if err != nil || collection.ResultReference["native_result_id"] != "destination-result" {
		t.Fatalf("remote result collection = %#v, %v", collection, err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.StageRemoteResult(
		context.Background(), result.RequestID, result.Outcome, result.ResultReference,
	)
	if err != nil || replayed.RemoteResultReference["native_result_id"] != "destination-result" {
		t.Fatalf("terminal result replay after restart = %#v, %v", replayed, err)
	}
	restartedComponent := &federationComponent{lanes: restarted}
	if err := restartedComponent.acceptRemoteLaneNotice(context.Background(), valid); err == nil ||
		!strings.Contains(err.Error(), "delivery authority") {
		t.Fatalf("idempotent terminal notice replay error = %v", err)
	}
	afterReplay, err := restarted.ReadTurn(context.Background(), "lane-result", "turn-result")
	if err != nil || afterReplay.TerminalNoticeID != collection.Turn.TerminalNoticeID ||
		afterReplay.CollectionRevision != collection.CollectionRevision {
		t.Fatalf("terminal replay duplicated notice or collection: %#v, %v", afterReplay, err)
	}
	changed := cloneAttachmentEvidence(result.ResultReference)
	changed["native_result_id"] = "changed"
	if _, err := restarted.StageRemoteResult(context.Background(), result.RequestID, result.Outcome, changed); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("changed terminal replay error = %v", err)
	}
}

func TestRemoteLaneOversizedEncodedInputFailsBeforeDurableAcceptance(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-large", []string{"session:host-test/parent-large"})
	component := &federationComponent{lanes: fixture.engine}
	_, err := component.startRemoteLaneCommand(context.Background(), fixture.engine, parent, parent.AttachmentID, LaneCommandRequest{
		Product: "qwen", Command: "start", Host: "host-b", Input: strings.Repeat("x", federation.MaxLaneInputBytes+1),
	}, laneCommandOptions{name: "oversized"}, nil, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized remote input error = %v", err)
	}
	if turns := fixture.engine.RemoteTurns(true); len(turns) != 0 {
		t.Fatalf("oversized input mutated durable turns: %#v", turns)
	}
}

func TestRemoteLaneLostAcceptanceStaysUncertainAndAcceptsLaterTerminal(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "claude", "parent-uncertain", []string{"session:host-test/parent-uncertain"})
	_, turn, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
		TargetHostID: "host-b",
		Request: LaneStartRequest{
			LaneSessionID: "lane-uncertain", TurnID: "turn-uncertain", SourceAttachmentID: parent.AttachmentID,
			Product: "grok", Name: "uncertain", Cwd: "/workspace", PermissionMode: "default",
			InputReference: map[string]any{"input_id": "uncertain"}, RemoteRequestID: "request-uncertain",
		},
	})
	if err != nil || turn.DispatchState != LaneDispatchAccepted || turn.TerminalOutcome != "" {
		t.Fatalf("uncertain acceptance = %#v, %v", turn, err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.ReadTurn(context.Background(), "lane-uncertain", "turn-uncertain")
	if err != nil || recovered.DispatchState != LaneDispatchAccepted || recovered.TerminalOutcome != "" || fixture.adapter.callCount() != 0 {
		t.Fatalf("recovered uncertain request = %#v, %v; adapter calls %d", recovered, err, fixture.adapter.callCount())
	}
	component := &federationComponent{lanes: restarted}
	if err := component.acceptRemoteLaneResponse(context.Background(), federation.RemoteLaneResponse{
		Type: "lane_exit", RequestID: "request-uncertain", ExitCode: 0,
	}); err != nil {
		t.Fatalf("accept terminal after lost acceptance: %v", err)
	}
	terminal, err := restarted.ReadTurn(context.Background(), "lane-uncertain", "turn-uncertain")
	if err != nil || terminal.TerminalOutcome != LaneDispatchCompleted || fixture.adapter.callCount() != 0 {
		t.Fatalf("late terminal = %#v, %v; adapter calls %d", terminal, err, fixture.adapter.callCount())
	}
}

func TestRemoteLaneCancellationDecisionIsDurableAndRefusalIsReported(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "claude", "parent-cancel", []string{"session:host-test/parent-cancel"})
	_, _, err := fixture.engine.AcceptRemoteSource(context.Background(), RemoteLaneSourceRequest{
		TargetHostID: "host-b",
		Request: LaneStartRequest{
			LaneSessionID: "lane-cancel", TurnID: "turn-cancel", SourceAttachmentID: parent.AttachmentID,
			Product: "grok", Name: "cancel", Cwd: "/workspace", PermissionMode: "default",
			InputReference: map[string]any{"input_id": "cancel"}, RemoteRequestID: "request-cancel",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := fixture.engine.RequestRemoteCancellation(context.Background(), "request-cancel")
	if err != nil || pending.RemoteCancellationState != "pending" {
		t.Fatalf("pending cancellation = %#v, %v", pending, err)
	}
	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	_, recovered, err := restarted.RemoteTurn("request-cancel")
	if err != nil || recovered.RemoteCancellationState != "pending" {
		t.Fatalf("recovered cancellation = %#v, %v", recovered, err)
	}
	refused, err := restarted.ResolveRemoteCancellation(context.Background(), "request-cancel", false, "native refusal")
	if err != nil || refused.RemoteCancellationState != "refused" || refused.RemoteCancellationError != "native refusal" {
		t.Fatalf("refused cancellation = %#v, %v", refused, err)
	}
}

func TestRemoteLaneCancellationAcceptanceDoesNotBecomeRefusalWhenTerminalRouteIsDown(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	interruptAdapter := &remoteLaneInterruptTestAdapter{laneTestAdapter: fixture.adapter}
	options := fixture.options()
	for product := range options.Adapters {
		options.Adapters[product] = interruptAdapter
	}
	var err error
	fixture.engine, err = NewLaneEngine(options)
	if err != nil {
		t.Fatal(err)
	}
	component, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{HostID: "host-b", HostName: "host-b", RemoteLanesEnabled: true},
		generation:    20, runtimeVersion: "test", runtimeIdentity: "sha256:test",
		attachments: fixture.attachments, lanes: fixture.engine, laneAdapters: fixture.options().Adapters,
		PublishRemoteLaneResult: func(context.Context, federation.RemoteLaneResult) error {
			return errors.New("terminal result route unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := component.DispatchRemoteLane(context.Background(), federation.RemoteLaneEnvelope{
		RequestID: "request-cancel-accepted", SourceID: "host-a/parent", TargetHostID: "host-b",
		Parent: federation.Peer{
			ID: "host-a/parent", HostID: "host-a", SessionID: "parent", Entrypoint: "codex",
			InstanceID: "parent-instance", PermissionMode: "default",
		},
		Product: "grok", LaneSessionID: "lane-cancel-accepted", TurnID: "turn-cancel-accepted",
		Name: "worker", Cwd: "/workspace", PermissionMode: "default",
		InputReference: map[string]any{"input_id": "cancel-accepted"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := component.cancelRemoteLane(context.Background(), federation.RemoteLaneCancellation{
		RequestID: "request-cancel-accepted",
	}); err != nil {
		t.Fatalf("accepted native cancellation became a refusal: %v", err)
	}
	turn, err := fixture.engine.ReadTurn(context.Background(), "lane-cancel-accepted", "turn-cancel-accepted")
	if err != nil || turn.TerminalOutcome != LaneDispatchInterrupted {
		t.Fatalf("cancelled destination turn = %#v, %v", turn, err)
	}
	if _, queued := component.recoveredTerminalOutbox["request-cancel-accepted"]; !queued {
		t.Fatal("failed terminal publication was not retained in the durable outbox")
	}
}

type remoteTargetNormalizationTestAdapter struct {
	*laneTestAdapter
	mu      sync.Mutex
	product string
	calls   []LaneCommandNormalizationRequest
}

func (adapter *remoteTargetNormalizationTestAdapter) NormalizeLaneCommand(
	_ context.Context,
	request LaneCommandNormalizationRequest,
) (LaneCommandNormalization, error) {
	adapter.mu.Lock()
	request.Arguments = append([]string(nil), request.Arguments...)
	request.NativeActor = cloneAttachmentEvidence(request.NativeActor)
	adapter.calls = append(adapter.calls, request)
	adapter.mu.Unlock()
	return LaneCommandNormalization{
		Cwd: filepath.Join("/target", adapter.product), PermissionMode: request.PermissionMode,
		NativeOptions: map[string]any{"normalized_on": "host-target", "product": adapter.product},
	}, nil
}

func (adapter *remoteTargetNormalizationTestAdapter) callsSnapshot() []LaneCommandNormalizationRequest {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	result := make([]LaneCommandNormalizationRequest, len(adapter.calls))
	copy(result, adapter.calls)
	return result
}

type remoteLaneInterruptTestAdapter struct{ *laneTestAdapter }

func (*remoteLaneInterruptTestAdapter) InterruptTurn(context.Context, LaneRecord, LaneTurnRecord) error {
	return nil
}
