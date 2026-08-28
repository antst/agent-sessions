package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestLaneStartCommitsExactParentContextBeforeNativeDispatch(t *testing.T) {
	for _, product := range productcatalog.ProductDescriptors() {
		for _, inherit := range []bool{false, true} {
			name := product.ID + "/inherit=" + fmt.Sprint(inherit)
			t.Run(name, func(t *testing.T) {
				fixture := newLaneTestFixture(t, nil, nil)
				parentSessionID := "parent-" + product.ID
				parentAnchor := "session:host-test/" + parentSessionID
				parent := fixture.attach(t, "codex", parentSessionID, []string{"parent-project", parentAnchor})
				laneSessionID := "lane-" + product.ID + "-" + fmt.Sprint(inherit)
				turnID := "turn-" + product.ID + "-" + fmt.Sprint(inherit)

				fixture.adapter.onDispatch = func(lane LaneRecord, turn LaneTurnRecord) {
					var durable laneTestCatalogProjection
					if _, err := fixture.state.records.Read(context.Background(), "lanes", &durable); err != nil {
						t.Fatalf("read durable lane catalog at dispatch: %v", err)
					}
					persistedLane, laneOK := laneTestFindLane(durable.Lanes, laneSessionID)
					persistedTurn, turnOK := laneTestFindTurn(durable.Turns, turnID)
					if !laneOK || !turnOK {
						t.Fatalf("native dispatch preceded durable acceptance: lanes=%#v turns=%#v", durable.Lanes, durable.Turns)
					}
					if persistedLane.ActiveTurnID != turnID || persistedTurn.DispatchState != LaneDispatchStateAccepted {
						t.Fatalf("pre-dispatch durable state = lane %#v turn %#v", persistedLane, persistedTurn)
					}
					if lane.Revision == 0 || turn.Revision == 0 || lane.LaneSessionID != laneSessionID || turn.TurnID != turnID {
						t.Fatalf("adapter received non-durable identity: lane=%#v turn=%#v", lane, turn)
					}
				}

				lane, turn, err := fixture.engine.Start(context.Background(), LaneStartRequest{
					LaneSessionID: laneSessionID, TurnID: turnID, SourceAttachmentID: parent.AttachmentID,
					Product: product.ID, Name: "worker-" + product.ID, Cwd: "/workspace",
					Groups: []string{"child-explicit"}, InheritParentGroups: inherit,
					PermissionMode: "bypassPermissions", InputReference: map[string]any{"input_id": "input-" + turnID},
				})
				if err != nil {
					t.Fatalf("start lane: %v", err)
				}
				if fixture.adapter.dispatchCount(turnID) != 1 {
					t.Fatalf("native dispatch count = %d, want one", fixture.adapter.dispatchCount(turnID))
				}
				if lane.ParentHostID != parent.HostID || lane.ParentSessionID != parent.SessionID ||
					!reflect.DeepEqual(lane.ParentGroups, parent.Groups) || lane.InheritParentGroups != inherit {
					t.Fatalf("lane parent context = %#v, parent=%#v", lane, parent)
				}
				wantGroups := []string{
					"child-explicit", parentAnchor, "session:host-test/" + laneSessionID,
				}
				if inherit {
					wantGroups = append(wantGroups, parent.Groups...)
				}
				wantGroups = laneTestSortedUnique(wantGroups)
				if !reflect.DeepEqual(lane.Groups, wantGroups) {
					t.Fatalf("effective groups = %q, want %q", lane.Groups, wantGroups)
				}
				if lane.PermissionMode != "bypassPermissions" || lane.Product != product.ID || lane.Cwd != "/workspace" {
					t.Fatalf("exact lane target context = %#v", lane)
				}
				if turn.ParentContextRevision != parent.Revision || turn.LaneSessionID != laneSessionID ||
					turn.DispatchState != LaneDispatchStateRunning || turn.NativeTurnIdentity["turn"] != turnID {
					t.Fatalf("accepted turn = %#v, parent revision=%d", turn, parent.Revision)
				}
			})
		}
	}
}

func TestLaneAcceptedTurnIsNeverDispatchedTwice(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "claude", "parent-duplicate", []string{
		"project", "session:host-test/parent-duplicate",
	})
	request := LaneStartRequest{
		LaneSessionID: "lane-duplicate", TurnID: "turn-duplicate", SourceAttachmentID: parent.AttachmentID,
		Product: "grok", Name: "duplicate-worker", Cwd: "/workspace", Groups: []string{"child"},
		PermissionMode: "default", InputReference: map[string]any{"input_id": "stable-input"},
	}
	firstLane, firstTurn, err := fixture.engine.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	secondLane, secondTurn, err := fixture.engine.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !reflect.DeepEqual(firstLane, secondLane) || !reflect.DeepEqual(firstTurn, secondTurn) {
		t.Fatalf("same request changed accepted result: first=%#v/%#v second=%#v/%#v", firstLane, firstTurn, secondLane, secondTurn)
	}
	if got := fixture.adapter.dispatchCount(request.TurnID); got != 1 {
		t.Fatalf("same-process retry dispatch count = %d, want one", got)
	}

	restarted, err := NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatalf("reopen lane authority: %v", err)
	}
	thirdLane, thirdTurn, err := restarted.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("retry after daemon reconstruction: %v", err)
	}
	if !reflect.DeepEqual(firstLane, thirdLane) || !reflect.DeepEqual(firstTurn, thirdTurn) ||
		fixture.adapter.dispatchCount(request.TurnID) != 1 {
		t.Fatalf("restart retry duplicated work: first=%#v/%#v third=%#v/%#v dispatches=%d",
			firstLane, firstTurn, thirdLane, thirdTurn, fixture.adapter.dispatchCount(request.TurnID))
	}

	changed := request
	changed.InputReference = map[string]any{"input_id": "different-work"}
	if _, _, err := restarted.Start(context.Background(), changed); !errors.Is(err, ErrLaneIdempotencyConflict) {
		t.Fatalf("changed work under accepted IDs = %v, want ErrLaneIdempotencyConflict", err)
	}
	if fixture.adapter.dispatchCount(request.TurnID) != 1 {
		t.Fatal("idempotency conflict reached the native adapter")
	}
}

func TestLaneStartDuplicateNameAdmissionIsAtomic(t *testing.T) {
	for _, test := range []struct {
		name           string
		allowDuplicate bool
		wantAccepted   int
	}{
		{name: "default", wantAccepted: 1},
		{name: "explicitly-allowed", allowDuplicate: true, wantAccepted: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLaneTestFixture(t, nil, nil)
			parent := fixture.attach(t, "codex", "parent-concurrent-"+test.name, []string{"project"})
			start := make(chan struct{})
			results := make(chan error, 2)
			var ready sync.WaitGroup
			ready.Add(2)
			for index := 0; index < 2; index++ {
				index := index
				go func() {
					ready.Done()
					<-start
					_, _, err := fixture.engine.Start(context.Background(), LaneStartRequest{
						LaneSessionID:      fmt.Sprintf("lane-concurrent-%s-%d", test.name, index),
						TurnID:             fmt.Sprintf("turn-concurrent-%s-%d", test.name, index),
						SourceAttachmentID: parent.AttachmentID,
						Product:            "claude",
						Name:               "same-visible-name",
						Cwd:                "/workspace",
						PermissionMode:     "default",
						InputReference:     map[string]any{"input_id": fmt.Sprintf("input-%d", index)},
						AllowDuplicateName: test.allowDuplicate,
					})
					results <- err
				}()
			}
			ready.Wait()
			close(start)

			accepted, conflicts := 0, 0
			for index := 0; index < 2; index++ {
				switch err := <-results; {
				case err == nil:
					accepted++
				case errors.Is(err, ErrLaneIdempotencyConflict):
					conflicts++
				default:
					t.Fatalf("concurrent start error = %v", err)
				}
			}
			if accepted != test.wantAccepted || conflicts != 2-test.wantAccepted {
				t.Fatalf("concurrent admission accepted=%d conflicts=%d, want %d/%d", accepted, conflicts, test.wantAccepted, 2-test.wantAccepted)
			}
		})
	}
}

func TestLaneTerminalNoticeAndConcurrentCollectionAreExactlyOnce(t *testing.T) {
	const resultCanary = "LANE_RESULT_MUST_NOT_REACH_LOGS_08a5d3"
	var (
		observationMu sync.Mutex
		observations  []LaneObservation
	)
	fixture := newLaneTestFixture(t, nil, func(observation LaneObservation) {
		observationMu.Lock()
		defer observationMu.Unlock()
		observations = append(observations, observation)
	})
	parent := fixture.attach(t, "qwen", "parent-collect", []string{
		"project", "session:host-test/parent-collect",
	})
	_, started, err := fixture.engine.Start(context.Background(), LaneStartRequest{
		LaneSessionID: "lane-collect", TurnID: "turn-collect", SourceAttachmentID: parent.AttachmentID,
		Product: "codex", Name: "collector", Cwd: "/workspace", PermissionMode: "default",
		InputReference: map[string]any{"input_id": "collect-input"},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalRequest := LaneTerminalRequest{
		LaneSessionID: started.LaneSessionID, TurnID: started.TurnID, Outcome: LaneTerminalCompleted,
		NativeTurnIdentity: map[string]any{"turn": started.TurnID},
		ResultReference:    map[string]any{"content": resultCanary, "native_result_id": "result-1"},
	}
	terminal, err := fixture.engine.Complete(context.Background(), terminalRequest)
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	replayedTerminal, err := fixture.engine.Complete(context.Background(), terminalRequest)
	if err != nil || !reflect.DeepEqual(terminal, replayedTerminal) {
		t.Fatalf("terminal replay = %#v, %v; want %#v", replayedTerminal, err, terminal)
	}
	if terminal.TerminalNoticeID == "" {
		t.Fatalf("terminal turn has no durable notice: %#v", terminal)
	}
	notice, err := fixture.engine.ReadNotice(context.Background(), terminal.TerminalNoticeID)
	if err != nil {
		t.Fatalf("read terminal notice: %v", err)
	}
	if notice.ParentHostID != parent.HostID || notice.ParentSessionID != parent.SessionID ||
		notice.LaneSessionID != started.LaneSessionID || notice.TurnID != started.TurnID ||
		notice.Outcome != LaneTerminalCompleted {
		t.Fatalf("terminal notice = %#v", notice)
	}

	const collectors = 16
	results := make(chan LaneCollection, collectors)
	errorsSeen := make(chan error, collectors)
	var wait sync.WaitGroup
	for range collectors {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, collectErr := fixture.engine.Collect(context.Background(), LaneCollectRequest{
				LaneSessionID: started.LaneSessionID, TurnID: started.TurnID,
				SourceAttachmentID: parent.AttachmentID,
			})
			if collectErr != nil {
				errorsSeen <- collectErr
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for collectErr := range errorsSeen {
		t.Errorf("concurrent collect: %v", collectErr)
	}
	var (
		collected       []LaneCollection
		firstCollectors int
	)
	for result := range results {
		collected = append(collected, result)
		if !result.AlreadyCollected {
			firstCollectors++
		}
	}
	if len(collected) != collectors || firstCollectors != 1 {
		t.Fatalf("collection outcomes = %d, first collectors=%d, want %d/1: %#v", len(collected), firstCollectors, collectors, collected)
	}
	for _, result := range collected {
		if result.LaneSessionID != started.LaneSessionID || result.TurnID != started.TurnID ||
			result.Outcome != LaneTerminalCompleted || result.CollectionRevision == 0 ||
			!reflect.DeepEqual(result.ResultReference, terminalRequest.ResultReference) {
			t.Fatalf("unstable collection result = %#v", result)
		}
	}
	for _, result := range collected[1:] {
		if result.CollectionRevision != collected[0].CollectionRevision {
			t.Fatalf("collection revision changed: first=%#v later=%#v", collected[0], result)
		}
	}
	storedTurn, err := fixture.engine.ReadTurn(context.Background(), started.TurnID)
	if err != nil || storedTurn.CollectionRevision != collected[0].CollectionRevision || storedTurn.CollectedAt == 0 {
		t.Fatalf("durable collection cursor = %#v, %v", storedTurn, err)
	}
	storedLane, err := fixture.engine.ReadLane(context.Background(), started.LaneSessionID)
	if err != nil || storedLane.CollectionCursor != started.TurnID {
		t.Fatalf("lane collection cursor = %#v, %v", storedLane, err)
	}

	observationMu.Lock()
	body, marshalErr := json.Marshal(observations)
	observationMu.Unlock()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(body), resultCanary) {
		t.Fatalf("lane observation leaked result content: %s", body)
	}
	if len(observations) == 0 {
		t.Fatal("lane lifecycle emitted no metadata observation")
	}
}

func TestLaneTerminalWatcherCompletesNotifiesAndUnblocksWaitWithoutRedispatch(t *testing.T) {
	fixture := newLaneTestFixture(t, nil, nil)
	parent := fixture.attach(t, "codex", "parent-watch", []string{"team", "session:host-test/parent-watch"})
	waiter := &laneWaitTestAdapter{
		laneTestAdapter: fixture.adapter,
		terminal:        make(chan LaneTerminalResult, 1),
	}
	fixture.engine.adapters["claude"] = waiter
	lane, turn, err := fixture.engine.Start(context.Background(), LaneStartRequest{
		LaneSessionID: "lane-watch", TurnID: "turn-watch", SourceAttachmentID: parent.AttachmentID,
		Product: "claude", Name: "watch", Cwd: "/workspace", PermissionMode: "default",
		InputReference: map[string]any{"input_id": "input-watch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	collected := make(chan LaneCollection, 1)
	waitErrors := make(chan error, 1)
	go func() {
		result, waitErr := fixture.engine.Wait(context.Background(), LaneCollectRequest{
			LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, SourceAttachmentID: parent.AttachmentID,
		})
		if waitErr != nil {
			waitErrors <- waitErr
			return
		}
		collected <- result
	}()
	waiter.terminal <- LaneTerminalResult{
		TerminalOutcome:    LaneTerminalCompleted,
		NativeTurnIdentity: map[string]any{"turn": turn.TurnID},
		ResultReference:    map[string]any{"native_result_id": "result-watch"},
	}
	select {
	case waitErr := <-waitErrors:
		t.Fatal(waitErr)
	case result := <-collected:
		if result.Outcome != LaneTerminalCompleted || result.CollectionRevision != 1 || result.AlreadyCollected {
			t.Fatalf("watched collection = %#v", result)
		}
		if result.Turn.TerminalNoticeID == "" {
			t.Fatalf("watched terminal turn omitted notice: %#v", result.Turn)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon-owned lane terminal watcher did not unblock collection")
	}
	if got := fixture.adapter.dispatchCount(turn.TurnID); got != 1 {
		t.Fatalf("terminal watch redispatched native turn %d times", got)
	}
}

func TestLaneArchiveIsIdempotentAndCleanupFailureBecomesDebt(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		fixture := newLaneTestFixture(t, nil, nil)
		parent := fixture.attach(t, "codex", "parent-archive", []string{
			"project", "session:host-test/parent-archive",
		})
		lane, turn := laneTestStartAndComplete(t, fixture, parent, "lane-archive", "turn-archive")
		request := LaneArchiveRequest{LaneSessionID: lane.LaneSessionID, SourceAttachmentID: parent.AttachmentID}
		first, err := fixture.engine.Archive(context.Background(), request)
		if err != nil {
			t.Fatalf("archive lane: %v", err)
		}
		second, err := fixture.engine.Archive(context.Background(), request)
		if err != nil {
			t.Fatalf("idempotent archive: %v", err)
		}
		if first.State != LaneStateArchived || first.ArchiveRevision == 0 || !reflect.DeepEqual(first, second) {
			t.Fatalf("archive results = first %#v second %#v", first, second)
		}
		if got := fixture.adapter.archiveCount(lane.LaneSessionID); got != 1 {
			t.Fatalf("native archive count = %d, want one", got)
		}
		if archivedTurn, readErr := fixture.engine.ReadTurn(context.Background(), turn.TurnID); readErr != nil ||
			archivedTurn.TerminalOutcome != LaneTerminalCompleted {
			t.Fatalf("archive changed terminal turn = %#v, %v", archivedTurn, readErr)
		}
	})

	t.Run("cleanup debt", func(t *testing.T) {
		cleanupErr := errors.New("changed native archive ownership")
		fixture := newLaneTestFixture(t, nil, nil)
		fixture.adapter.archiveErr = cleanupErr
		parent := fixture.attach(t, "grok", "parent-debt", []string{
			"project", "session:host-test/parent-debt",
		})
		lane, _ := laneTestStartAndComplete(t, fixture, parent, "lane-debt", "turn-debt")
		if _, err := fixture.engine.Archive(context.Background(), LaneArchiveRequest{
			LaneSessionID: lane.LaneSessionID, SourceAttachmentID: parent.AttachmentID,
		}); !errors.Is(err, cleanupErr) {
			t.Fatalf("cleanup failure = %v, want %v", err, cleanupErr)
		}
		retained, err := fixture.engine.ReadLane(context.Background(), lane.LaneSessionID)
		if err != nil || retained.State != LaneStateDebt || len(retained.CleanupDebtIDs) != 1 || retained.ArchiveRevision != 0 {
			t.Fatalf("lane cleanup debt = %#v, %v", retained, err)
		}
		debt, _, err := fixture.state.ReadDebt(context.Background(), retained.CleanupDebtIDs[0])
		if err != nil {
			t.Fatalf("read cleanup debt: %v", err)
		}
		if debt.Operation != "archive" || debt.ResourceKind != "lane" || debt.ResourceIdentity != lane.LaneSessionID ||
			debt.CauseCode == "" || debt.RetryPredicate == "" || debt.ProhibitedScope == "" {
			t.Fatalf("cleanup debt lost exact retry authority: %#v", debt)
		}
		if fixture.adapter.archiveCount(lane.LaneSessionID) != 1 {
			t.Fatal("cleanup failure retried native archive without a new exact observation")
		}
	})
}

func TestLaneResourceFailuresRejectBeforeDurableAcceptanceOrNativeDispatch(t *testing.T) {
	for _, resource := range []string{"disk", "memory", "file_descriptors", "processes"} {
		t.Run(resource, func(t *testing.T) {
			resourceErr := &LaneResourceError{Resource: resource}
			fixture := newLaneTestFixture(t, func(context.Context, LaneStartRequest) error { return resourceErr }, nil)
			parent := fixture.attach(t, "claude", "parent-resource-"+resource, []string{
				"project", "session:host-test/parent-resource-" + resource,
			})
			laneID, turnID := "lane-resource-"+resource, "turn-resource-"+resource
			if _, _, err := fixture.engine.Start(context.Background(), LaneStartRequest{
				LaneSessionID: laneID, TurnID: turnID, SourceAttachmentID: parent.AttachmentID,
				Product: "qwen", Name: "resource-test", Cwd: "/workspace", PermissionMode: "default",
				InputReference: map[string]any{"input_id": "resource-input"},
			}); !errors.Is(err, resourceErr) {
				t.Fatalf("%s admission error = %v, want %v", resource, err, resourceErr)
			}
			if fixture.adapter.callCount() != 0 {
				t.Fatalf("%s resource rejection reached native adapter", resource)
			}
			if _, err := fixture.engine.ReadLane(context.Background(), laneID); !errors.Is(err, ErrLaneNotFound) {
				t.Fatalf("%s rejected lane durable state = %v, want ErrLaneNotFound", resource, err)
			}
			if _, err := fixture.engine.ReadTurn(context.Background(), turnID); !errors.Is(err, ErrLaneNotFound) {
				t.Fatalf("%s rejected turn durable state = %v, want ErrLaneNotFound", resource, err)
			}
			var durable laneTestCatalogProjection
			if _, err := fixture.state.records.Read(context.Background(), "lanes", &durable); err == nil &&
				(len(durable.Lanes) != 0 || len(durable.Turns) != 0) {
				t.Fatalf("%s rejection committed lane catalog: %#v", resource, durable)
			}
		})
	}
}

type laneTestFixture struct {
	state       *StateStore
	attachments *AttachmentRegistry
	engine      *LaneEngine
	adapter     *laneTestAdapter
	clock       func() time.Time
	preflight   func(context.Context, LaneStartRequest) error
	observe     func(LaneObservation)
}

func newLaneTestFixture(
	t *testing.T,
	preflight func(context.Context, LaneStartRequest) error,
	observe func(LaneObservation),
) *laneTestFixture {
	t.Helper()
	state, err := OpenStateStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	clock := attachmentTestClock()
	attachmentAdapter := &attachmentTestAdapter{}
	attachments, err := NewAttachmentRegistry(AttachmentRegistryOptions{
		State: state, Generation: 17, HostID: "host-test", Now: clock,
		Capability: func() (string, error) { return "lane-test-capability", nil },
		Adapters: map[string]AttachmentAdapter{
			"codex": attachmentAdapter, "claude": attachmentAdapter,
			"grok": attachmentAdapter, "qwen": attachmentAdapter,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &laneTestAdapter{dispatches: make(map[string]int), archives: make(map[string]int)}
	fixture := &laneTestFixture{
		state: state, attachments: attachments, adapter: adapter, clock: clock,
		preflight: preflight, observe: observe,
	}
	fixture.engine, err = NewLaneEngine(fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *laneTestFixture) options() LaneEngineOptions {
	adapters := make(map[string]LaneAdapter)
	for _, product := range productcatalog.ProductDescriptors() {
		adapters[product.ID] = fixture.adapter
	}
	return LaneEngineOptions{
		State: fixture.state, Attachments: fixture.attachments, Generation: 17,
		Now: fixture.clock, Adapters: adapters, Preflight: fixture.preflight, Observe: fixture.observe,
	}
}

func (fixture *laneTestFixture) attach(
	t *testing.T,
	product, sessionID string,
	groups []string,
) AttachmentRecord {
	t.Helper()
	return attachDeliveryLaneTestParticipant(
		t, fixture.attachments, product, "parent", sessionID, "/workspace", groups, len(sessionID)+2000,
	)
}

type laneTestAdapter struct {
	mu         sync.Mutex
	dispatches map[string]int
	archives   map[string]int
	archiveErr error
	onDispatch func(LaneRecord, LaneTurnRecord)
}

type laneWaitTestAdapter struct {
	*laneTestAdapter
	terminal chan LaneTerminalResult
}

func (adapter *laneWaitTestAdapter) WaitTurn(
	ctx context.Context,
	_ LaneRecord,
	_ LaneTurnRecord,
) (LaneTerminalResult, error) {
	select {
	case result := <-adapter.terminal:
		return result, nil
	case <-ctx.Done():
		return LaneTerminalResult{}, ctx.Err()
	}
}

func (adapter *laneTestAdapter) Dispatch(
	_ context.Context,
	lane LaneRecord,
	turn LaneTurnRecord,
) (LaneDispatchResult, error) {
	adapter.mu.Lock()
	adapter.dispatches[turn.TurnID]++
	hook := adapter.onDispatch
	adapter.mu.Unlock()
	if hook != nil {
		hook(lane, turn)
	}
	return LaneDispatchResult{
		NativeActor:        map[string]any{"lane": lane.LaneSessionID},
		NativeTurnIdentity: map[string]any{"turn": turn.TurnID},
	}, nil
}

func (adapter *laneTestAdapter) Archive(_ context.Context, lane LaneRecord) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.archives[lane.LaneSessionID]++
	return adapter.archiveErr
}

func (adapter *laneTestAdapter) dispatchCount(turnID string) int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.dispatches[turnID]
}

func (adapter *laneTestAdapter) archiveCount(laneSessionID string) int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.archives[laneSessionID]
}

func (adapter *laneTestAdapter) callCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	result := 0
	for _, count := range adapter.dispatches {
		result += count
	}
	for _, count := range adapter.archives {
		result += count
	}
	return result
}

func laneTestStartAndComplete(
	t *testing.T,
	fixture *laneTestFixture,
	parent AttachmentRecord,
	laneSessionID, turnID string,
) (LaneRecord, LaneTurnRecord) {
	t.Helper()
	lane, _, err := fixture.engine.Start(context.Background(), LaneStartRequest{
		LaneSessionID: laneSessionID, TurnID: turnID, SourceAttachmentID: parent.AttachmentID,
		Product: "claude", Name: laneSessionID, Cwd: "/workspace", PermissionMode: "default",
		InputReference: map[string]any{"input_id": "input-" + turnID},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.engine.Complete(context.Background(), LaneTerminalRequest{
		LaneSessionID: laneSessionID, TurnID: turnID, Outcome: LaneTerminalCompleted,
		NativeTurnIdentity: map[string]any{"turn": turnID},
		ResultReference:    map[string]any{"native_result_id": "result-" + turnID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return lane, turn
}

type laneTestCatalogProjection struct {
	Lanes   []LaneRecord     `json:"lanes"`
	Turns   []LaneTurnRecord `json:"turns"`
	Notices []LaneNotice     `json:"notices"`
}

func laneTestFindLane(records []LaneRecord, laneSessionID string) (LaneRecord, bool) {
	for _, record := range records {
		if record.LaneSessionID == laneSessionID {
			return record, true
		}
	}
	return LaneRecord{}, false
}

func laneTestFindTurn(records []LaneTurnRecord, turnID string) (LaneTurnRecord, bool) {
	for _, record := range records {
		if record.TurnID == turnID {
			return record, true
		}
	}
	return LaneTurnRecord{}, false
}

func laneTestSortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
