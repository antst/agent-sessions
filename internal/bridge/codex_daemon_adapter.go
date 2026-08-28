package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

// ErrCodexHistoryProjectionUnavailable reports a native App Server history-readiness gap.
var ErrCodexHistoryProjectionUnavailable = errors.New("codex thread history projection is unavailable")

type codexDaemonThread struct {
	ID           string
	Cwd          string
	Profile      string
	PID          int
	ProcStart    string
	HistoryReady bool
}

type codexDaemonClient interface {
	PrepareInteractive(context.Context, daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error)
	InspectThread(context.Context, string, string) (codexDaemonThread, error)
	DeliverFrame(context.Context, string, string, federation.AgentFrame) error
}

type codexDaemonLaneClient interface {
	StartTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (daemonpkg.LaneDispatchResult, error)
	ReconnectTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (daemonpkg.LaneReconnectResult, error)
	InterruptTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) error
	CollectTurn(context.Context, daemonpkg.LaneRecord, daemonpkg.LaneTurnRecord) (daemonpkg.LaneTerminalResult, error)
	Archive(context.Context, daemonpkg.LaneRecord) error
	Cleanup(context.Context, daemonpkg.LaneRecord) error
}

// PrepareInteractive returns the direct Codex vendor handoff for one validated launch intent.
func (adapter *codexDaemonAdapter) PrepareInteractive(ctx context.Context, request daemonpkg.AttachmentPrepareRequest) (daemonpkg.NativeLaunchPlan, error) {
	if adapter == nil || adapter.client == nil {
		return daemonpkg.NativeLaunchPlan{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return adapter.client.PrepareInteractive(ctx, request)
}

type codexDaemonAdapter struct{ client codexDaemonClient }

func newCodexDaemonAdapter(client codexDaemonClient) *codexDaemonAdapter {
	return &codexDaemonAdapter{client: client}
}

// NewCodexDaemonAdapter constructs the daemon-owned Codex adapter. The adapter
// opens at most one App Server client per configured Codex profile and never
// starts a supervisor, shim, or Agent Sessions listener.
func NewCodexDaemonAdapter() *codexDaemonAdapter {
	return newCodexDaemonAdapter(newCodexAppServerCoordinator())
}

// Close releases daemon-owned App Server client connections without affecting
// the vendor App Server or any Codex TUI.
func (adapter *codexDaemonAdapter) Close() {
	if coordinator, ok := adapter.client.(*codexAppServerCoordinator); ok {
		coordinator.close()
	}
}

// Corroborate proves that a prepared Codex attachment selected the expected native thread.
func (adapter *codexDaemonAdapter) Corroborate(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	evidence map[string]any,
) (map[string]any, error) {
	threadID := record.SessionID
	if threadID == "" {
		threadID = stringValue(evidence["thread_id"])
	}
	if threadID == "" {
		threadID = stringValue(record.NativeActor["thread_id"])
	}
	return adapter.inspectExact(ctx, record, threadID, evidence)
}

// Reconnect revalidates an already attached Codex thread after daemon recovery.
func (adapter *codexDaemonAdapter) Reconnect(ctx context.Context, record daemonpkg.AttachmentRecord) (map[string]any, error) {
	return adapter.inspectExact(ctx, record, record.SessionID, record.NativeActor)
}

// Deliver forwards one already-admitted frame through the exact Codex thread.
func (adapter *codexDaemonAdapter) Deliver(
	ctx context.Context,
	destination daemonpkg.AttachmentRecord,
	frame federation.AgentFrame,
) error {
	if _, err := adapter.Reconnect(ctx, destination); err != nil {
		return err
	}
	return adapter.client.DeliverFrame(ctx, codexRecordProfile(destination), destination.SessionID, frame)
}

// Dispatch preserves the daemon's minimum lane-adapter compatibility boundary.
func (adapter *codexDaemonAdapter) Dispatch(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	return adapter.StartTurn(ctx, lane, turn)
}

// StartTurn dispatches one already-committed lane turn through Codex App Server.
func (adapter *codexDaemonAdapter) StartTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	client, err := adapter.laneClient()
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	return client.StartTurn(ctx, lane, turn)
}

// ReconnectTurn recovers one exact native turn without dispatching it again.
func (adapter *codexDaemonAdapter) ReconnectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneReconnectResult, error) {
	client, err := adapter.laneClient()
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	return client.ReconnectTurn(ctx, lane, turn)
}

// InterruptTurn interrupts only the exact current Codex native turn.
func (adapter *codexDaemonAdapter) InterruptTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	client, err := adapter.laneClient()
	if err != nil {
		return err
	}
	return client.InterruptTurn(ctx, lane, turn)
}

// CollectTurn returns stable native terminal data without advancing the daemon cursor.
func (adapter *codexDaemonAdapter) CollectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	client, err := adapter.laneClient()
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	return client.CollectTurn(ctx, lane, turn)
}

// Archive invokes and verifies Codex's vendor-owned thread archive contract.
func (adapter *codexDaemonAdapter) Archive(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.laneClient()
	if err != nil {
		return err
	}
	return client.Archive(ctx, lane)
}

// Cleanup removes only attributable Agent Sessions artifacts, including an
// explicitly requested detached worktree. Vendor history remains untouched.
func (adapter *codexDaemonAdapter) Cleanup(ctx context.Context, lane daemonpkg.LaneRecord) error {
	client, err := adapter.laneClient()
	if err != nil {
		return err
	}
	return client.Cleanup(ctx, lane)
}

func (adapter *codexDaemonAdapter) laneClient() (codexDaemonLaneClient, error) {
	if adapter == nil || adapter.client == nil {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	client, ok := adapter.client.(codexDaemonLaneClient)
	if !ok {
		return nil, errors.New("codex daemon client does not implement lane lifecycle")
	}
	return client, nil
}

func (adapter *codexDaemonAdapter) inspectExact(
	ctx context.Context,
	record daemonpkg.AttachmentRecord,
	threadID string,
	evidence map[string]any,
) (map[string]any, error) {
	if adapter == nil || adapter.client == nil || strings.TrimSpace(threadID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	thread, err := adapter.client.InspectThread(ctx, codexRecordProfile(record), threadID)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex App Server thread: %w", err)
	}
	if !matchesDaemonSession(record, threadID, thread.ID, thread.Cwd, thread.Profile) ||
		!matchesDaemonActorEvidence(record.NativeActor, evidence,
			daemonActorField{key: "pid", value: thread.PID},
			daemonActorField{key: "proc_start", value: thread.ProcStart},
		) {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if !thread.HistoryReady {
		return nil, fmt.Errorf("%w for %s; run `codex migrate-rollouts --apply` and retry", ErrCodexHistoryProjectionUnavailable, threadID)
	}
	return map[string]any{
		"thread_id": thread.ID, "pid": thread.PID, "proc_start": thread.ProcStart,
		"profile": thread.Profile, "cwd": thread.Cwd, "history_ready": true,
	}, nil
}

func codexRecordProfile(record daemonpkg.AttachmentRecord) string {
	return strings.TrimSpace(stringValue(record.ProfileIdentity["profile"]))
}

// codexAppServerCoordinator is the in-process replacement for the legacy
// profile supervisor. It owns only reusable client connections; Codex owns the
// App Server process, TUI processes, threads, and transcript history.
type codexAppServerCoordinator struct {
	mu      sync.Mutex
	clients map[string]*appServerClient
}

func newCodexAppServerCoordinator() *codexAppServerCoordinator {
	return &codexAppServerCoordinator{clients: make(map[string]*appServerClient)}
}

// PrepareInteractive performs one supported App Server selection transaction
// and returns a direct Codex TUI handoff.
func (coordinator *codexAppServerCoordinator) PrepareInteractive(
	ctx context.Context,
	request daemonpkg.AttachmentPrepareRequest,
) (daemonpkg.NativeLaunchPlan, error) {
	profile, err := canonicalCodexProfile(stringValue(request.ProfileIdentity["profile"]))
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	client, err := coordinator.client(ctx, profile)
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	var thread appThread
	switch request.Intent.Mode {
	case "fresh":
		thread, err = coordinator.startThread(ctx, client, request)
	case "resume":
		thread, err = coordinator.resumeThread(ctx, client, profile, request)
	default:
		err = fmt.Errorf("unsupported Codex interactive mode %q", request.Intent.Mode)
	}
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	executable, err := codexDaemonExecutable()
	if err != nil {
		return daemonpkg.NativeLaunchPlan{}, err
	}
	arguments := []string{"--remote", "unix://", "resume", thread.ID}
	if !request.Intent.CwdExplicit {
		arguments = append(arguments, "-C", request.Cwd)
	}
	arguments = append(arguments, request.Intent.NativeArguments...)
	return daemonpkg.NativeLaunchPlan{
		Executable: executable, Arguments: arguments, Environment: map[string]string{"CODEX_HOME": profile},
		SessionID: thread.ID, Cwd: request.Cwd,
		ExpectedNativeActor: map[string]any{
			"thread_id": thread.ID, "pid": client.peerPID, "proc_start": client.peerProcStart,
		},
	}, nil
}

func (coordinator *codexAppServerCoordinator) startThread(
	ctx context.Context,
	client *appServerClient,
	request daemonpkg.AttachmentPrepareRequest,
) (appThread, error) {
	params := map[string]any{"cwd": request.Cwd, "ephemeral": false, "serviceName": "agent-sessions"}
	if request.PermissionMode == "bypassPermissions" {
		params["approvalPolicy"] = "never"
		params["sandbox"] = "danger-full-access"
	}
	var started struct {
		Thread appThread `json:"thread"`
	}
	if err := codexAppServerRequest(ctx, client, 60*time.Second, "thread/start", params, &started); err != nil {
		return appThread{}, err
	}
	if !validSessionID(started.Thread.ID) || validatePreparedRootThread(started.Thread) != nil {
		return appThread{}, errors.New("codex App Server returned an invalid root thread")
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = defaultPeerName(request.Cwd, started.Thread.ID)
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": started.Thread.ID, "name": sanitizeName(name),
	}, nil); err != nil {
		_ = codexAppServerRequest(context.Background(), client, 15*time.Second, "thread/delete", map[string]any{"threadId": started.Thread.ID}, nil)
		return appThread{}, err
	}
	started.Thread.Name = sanitizeName(name)
	return started.Thread, nil
}

func (coordinator *codexAppServerCoordinator) resumeThread(
	ctx context.Context,
	client *appServerClient,
	profile string,
	request daemonpkg.AttachmentPrepareRequest,
) (appThread, error) {
	thread, err := resolveCodexDaemonThread(ctx, client, profile, strings.TrimSpace(request.Intent.Selector))
	if err != nil {
		return appThread{}, err
	}
	params := map[string]any{"threadId": thread.ID, "excludeTurns": true, "cwd": request.Cwd}
	var resumed struct {
		Thread appThread `json:"thread"`
		Cwd    string    `json:"cwd"`
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/resume", params, &resumed); err != nil {
		return appThread{}, err
	}
	if resumed.Thread.ID != thread.ID || strings.TrimSpace(resumed.Cwd) != request.Cwd {
		return appThread{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if request.PermissionMode == "bypassPermissions" {
		if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/settings/update", map[string]any{
			"threadId": thread.ID, "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"},
		}, nil); err != nil {
			return appThread{}, err
		}
	}
	resumed.Thread.Cwd = request.Cwd
	if resumed.Thread.Name == "" {
		resumed.Thread.Name = thread.Name
	}
	return resumed.Thread, nil
}

// InspectThread reads one exact App Server thread and its process identity.
func (coordinator *codexAppServerCoordinator) InspectThread(
	ctx context.Context,
	profile, threadID string,
) (codexDaemonThread, error) {
	canonical, err := canonicalCodexProfile(profile)
	if err != nil {
		return codexDaemonThread{}, err
	}
	client, err := coordinator.client(ctx, canonical)
	if err != nil {
		return codexDaemonThread{}, err
	}
	var read struct {
		Thread appThread `json:"thread"`
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/read", map[string]any{
		"threadId": threadID, "includeTurns": true,
	}, &read); err != nil {
		return codexDaemonThread{}, err
	}
	if read.Thread.ID != threadID || validatePreparedRootThread(read.Thread) != nil {
		return codexDaemonThread{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return codexDaemonThread{
		ID: read.Thread.ID, Cwd: read.Thread.Cwd, Profile: canonical,
		PID: client.peerPID, ProcStart: client.peerProcStart,
		HistoryReady: codexThreadHistoryReady(read.Thread),
	}, nil
}

// DeliverFrame starts or steers one supported App Server turn.
func (coordinator *codexAppServerCoordinator) DeliverFrame(
	ctx context.Context,
	profile, threadID string,
	frame federation.AgentFrame,
) error {
	canonical, err := canonicalCodexProfile(profile)
	if err != nil {
		return err
	}
	client, err := coordinator.client(ctx, canonical)
	if err != nil {
		return err
	}
	thread, err := readCodexDaemonThread(ctx, client, threadID, false)
	if err != nil {
		return err
	}
	input := []map[string]any{{"type": "text", "text": peerMessageText(map[string]any{
		"from": frame.SourceSessionID, "id": frame.MessageID, "message": frame.Content, "sentAt": frame.SentAt,
	})}}
	if statusType(thread.Status) == "active" {
		turnID, activeErr := activeCodexTurn(ctx, client, threadID)
		if activeErr != nil {
			return activeErr
		}
		return codexAppServerRequest(ctx, client, 30*time.Second, "turn/steer", map[string]any{
			"threadId": threadID, "input": input, "expectedTurnId": turnID,
		}, nil)
	}
	return codexAppServerRequest(ctx, client, 60*time.Second, "turn/start", map[string]any{
		"threadId": threadID, "input": input,
	}, nil)
}

// StartTurn starts one daemon-accepted Codex turn. A lane without native
// thread evidence creates its vendor thread exactly once; follow-up turns
// resume the already recorded thread before starting new native work.
func (coordinator *codexAppServerCoordinator) StartTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneDispatchResult, error) {
	if strings.TrimSpace(lane.LaneSessionID) == "" || strings.TrimSpace(turn.TurnID) == "" ||
		turn.LaneSessionID != lane.LaneSessionID || lane.Product != "codex" {
		return daemonpkg.LaneDispatchResult{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if strings.TrimSpace(stringValue(turn.NativeTurnIdentity["turn_id"])) != "" {
		return daemonpkg.LaneDispatchResult{}, daemonpkg.ErrLaneIdempotencyConflict
	}
	prompt, err := codexDaemonLanePrompt(turn.InputReference)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	options, err := codexDaemonNormalizedLaneOptions(lane, turn)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	profile, client, err := coordinator.laneConnection(ctx, lane)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}

	threadID := strings.TrimSpace(stringValue(lane.NativeActor["thread_id"]))
	var thread appThread
	if threadID == "" {
		thread, err = coordinator.startDaemonLaneThread(ctx, client, lane, options)
	} else {
		thread, err = coordinator.resumeLaneThread(ctx, client, profile, lane, threadID)
	}
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	if thread.Cwd == "" {
		thread.Cwd = lane.Cwd
	}
	params := laneTurnStartParams(options, thread.ID, prompt)
	var started struct {
		Turn appTurn `json:"turn"`
	}
	if err := codexAppServerRequest(ctx, client, 60*time.Second, "turn/start", params, &started); err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	if !validSessionID(started.Turn.ID) {
		return daemonpkg.LaneDispatchResult{}, errors.New("codex App Server returned an invalid native turn identity")
	}
	dispatchState, _, err := codexDaemonLaneTurnState(started.Turn.Status)
	if err != nil {
		return daemonpkg.LaneDispatchResult{}, err
	}
	actor := codexDaemonLaneActor(profile, thread, client)
	copyDaemonLaneWorktreeEvidence(actor, daemonLaneNativeOptions(turn))
	return daemonpkg.LaneDispatchResult{
		LaneSessionID: lane.LaneSessionID,
		NativeActor:   actor,
		NativeTurnIdentity: codexDaemonLaneTurnIdentity(
			thread.ID, started.Turn.ID, dispatchState,
		),
		DispatchState: dispatchState,
	}, nil
}

func (coordinator *codexAppServerCoordinator) startDaemonLaneThread(
	ctx context.Context,
	client *appServerClient,
	lane daemonpkg.LaneRecord,
	options laneOptions,
) (appThread, error) {
	params, err := laneThreadStartParams(options)
	if err != nil {
		return appThread{}, err
	}
	params["serviceName"] = "agent-sessions"
	var started struct {
		Thread appThread `json:"thread"`
	}
	if err := codexAppServerRequest(ctx, client, 60*time.Second, "thread/start", params, &started); err != nil {
		return appThread{}, err
	}
	if !validSessionID(started.Thread.ID) || validatePreparedRootThread(started.Thread) != nil {
		return appThread{}, errors.New("codex App Server returned an invalid lane thread")
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/name/set", map[string]any{
		"threadId": started.Thread.ID, "name": sanitizeName(lane.Name),
	}, nil); err != nil {
		_ = codexAppServerRequest(context.Background(), client, 15*time.Second, "thread/delete", map[string]any{"threadId": started.Thread.ID}, nil)
		return appThread{}, err
	}
	started.Thread.Name, started.Thread.Cwd = sanitizeName(lane.Name), lane.Cwd
	return started.Thread, nil
}

func codexDaemonNormalizedLaneOptions(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (laneOptions, error) {
	native := daemonLaneNativeOptions(turn)
	options := laneOptions{laneCommonOptions: newLaneCommonOptions("THREAD_OR_NAME")}
	options.command, options.name, options.cwd = "start", lane.Name, lane.Cwd
	options.model = stringValue(native["model"])
	options.effort = stringValue(native["effort"])
	options.sandbox = stringValue(native["sandbox"])
	options.approvalPolicy = stringValue(native["approval_policy"])
	options.configs = daemonLaneStringSlice(native["config"])
	options.schemaFile = ""
	if value, ok := native["web"]; ok {
		web := boolValue(value)
		options.web = &web
	}
	schema, err := daemonLaneRawJSON(native["output_schema"])
	if err != nil {
		return laneOptions{}, err
	}
	options.outputSchema = schema
	if options.approvalPolicy == "" && lane.PermissionMode == "bypassPermissions" {
		options.approvalPolicy, options.sandbox = "never", "danger-full-access"
	}
	return options, nil
}

func copyDaemonLaneWorktreeEvidence(actor, native map[string]any) {
	if path := strings.TrimSpace(stringValue(native["worktree_path"])); path != "" {
		actor["worktree_path"] = path
		actor["original_cwd"] = strings.TrimSpace(stringValue(native["original_cwd"]))
	}
}

// ReconnectTurn resubscribes to the exact durable App Server thread and reads
// the recorded native turn. It never calls thread/start or turn/start.
func (coordinator *codexAppServerCoordinator) ReconnectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneReconnectResult, error) {
	threadID, turnID, err := codexDaemonLaneIdentities(lane, turn)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	profile, client, err := coordinator.laneConnection(ctx, lane)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	thread, err := coordinator.resumeLaneThread(ctx, client, profile, lane, threadID)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	nativeTurn, err := readExactCodexDaemonLaneTurn(ctx, client, threadID, turnID, "full")
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	dispatchState, outcome, err := codexDaemonLaneTurnState(nativeTurn.Status)
	if err != nil {
		return daemonpkg.LaneReconnectResult{}, err
	}
	result := daemonpkg.LaneReconnectResult{
		NativeActor:        codexDaemonLaneActor(profile, thread, client),
		NativeTurnIdentity: codexDaemonLaneTurnIdentity(threadID, turnID, dispatchState),
		DispatchState:      dispatchState,
		TerminalOutcome:    outcome,
	}
	if outcome != "" {
		result.ResultReference = codexDaemonLaneResult(threadID, nativeTurn, outcome)
	}
	return result, nil
}

// InterruptTurn interrupts the exact daemon-recorded native turn after
// revalidating its App Server process and thread evidence.
func (coordinator *codexAppServerCoordinator) InterruptTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) error {
	threadID, turnID, err := codexDaemonLaneIdentities(lane, turn)
	if err != nil {
		return err
	}
	profile, client, err := coordinator.laneConnection(ctx, lane)
	if err != nil {
		return err
	}
	thread, err := readCodexDaemonThread(ctx, client, threadID, false)
	if err != nil {
		return err
	}
	if err := validateCodexDaemonLaneThread(lane, profile, thread, client); err != nil {
		return err
	}
	return codexAppServerRequest(ctx, client, 30*time.Second, "turn/interrupt", map[string]any{
		"threadId": threadID, "turnId": turnID,
	}, nil)
}

// CollectTurn returns a stable projection of one exact terminal App Server
// turn. The daemon remains the sole owner of its collection cursor.
func (coordinator *codexAppServerCoordinator) CollectTurn(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
	turn daemonpkg.LaneTurnRecord,
) (daemonpkg.LaneTerminalResult, error) {
	threadID, turnID, err := codexDaemonLaneIdentities(lane, turn)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	profile, client, err := coordinator.laneConnection(ctx, lane)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	thread, err := readCodexDaemonThread(ctx, client, threadID, false)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	if err := validateCodexDaemonLaneThread(lane, profile, thread, client); err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	nativeTurn, err := readExactCodexDaemonLaneTurn(ctx, client, threadID, turnID, "full")
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	_, outcome, err := codexDaemonLaneTurnState(nativeTurn.Status)
	if err != nil {
		return daemonpkg.LaneTerminalResult{}, err
	}
	if outcome == "" {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrLaneNotTerminal
	}
	if outcome == daemonpkg.LaneDispatchCompleted && !codexDaemonLaneHasFinalAnswer(nativeTurn) {
		return daemonpkg.LaneTerminalResult{}, daemonpkg.ErrLaneNotTerminal
	}
	return daemonpkg.LaneTerminalResult{
		TerminalOutcome:    outcome,
		ResultReference:    codexDaemonLaneResult(threadID, nativeTurn, outcome),
		NativeTurnIdentity: codexDaemonLaneTurnIdentity(threadID, turnID, outcome),
	}, nil
}

// Archive invokes Codex's native archive call and then proves the exact thread
// is present in the archived App Server membership.
func (coordinator *codexAppServerCoordinator) Archive(ctx context.Context, lane daemonpkg.LaneRecord) error {
	threadID := strings.TrimSpace(stringValue(lane.NativeActor["thread_id"]))
	if threadID == "" {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	profile, client, err := coordinator.laneConnection(ctx, lane)
	if err != nil {
		return err
	}
	archived, err := codexThreadMembership(ctx, client, true)
	if err != nil {
		return err
	}
	if archived[threadID] {
		return nil
	}
	thread, err := readCodexDaemonThread(ctx, client, threadID, false)
	if err != nil {
		return err
	}
	if err := validateCodexDaemonLaneThread(lane, profile, thread, client); err != nil {
		return err
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/archive", map[string]any{
		"threadId": threadID,
	}, nil); err != nil {
		return err
	}
	archived, err = codexThreadMembership(ctx, client, true)
	if err != nil {
		return err
	}
	if !archived[threadID] {
		return errors.New("codex App Server did not confirm the archived lane thread")
	}
	return nil
}

// Cleanup is intentionally empty: the coordinator owns only in-process client
// connections and creates no per-lane Agent Sessions files, sockets, or workers.
func (*codexAppServerCoordinator) Cleanup(ctx context.Context, lane daemonpkg.LaneRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lane.Product != "codex" || strings.TrimSpace(lane.LaneSessionID) == "" {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return cleanupDaemonLaneWorktree(lane)
}

func (coordinator *codexAppServerCoordinator) laneConnection(
	ctx context.Context,
	lane daemonpkg.LaneRecord,
) (string, *appServerClient, error) {
	if lane.Product != "codex" {
		return "", nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	profile, err := canonicalCodexProfile(stringValue(lane.NativeActor["profile"]))
	if err != nil {
		return "", nil, err
	}
	client, err := coordinator.client(ctx, profile)
	if err != nil {
		return "", nil, err
	}
	if !matchesOptionalNumber(lane.NativeActor["pid"], client.peerPID) ||
		!matchesOptionalString(lane.NativeActor["proc_start"], client.peerProcStart) {
		return "", nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return profile, client, nil
}

func (coordinator *codexAppServerCoordinator) resumeLaneThread(
	ctx context.Context,
	client *appServerClient,
	profile string,
	lane daemonpkg.LaneRecord,
	threadID string,
) (appThread, error) {
	observed, err := readCodexDaemonThread(ctx, client, threadID, true)
	if err != nil {
		return appThread{}, err
	}
	if err := validateCodexDaemonLaneThread(lane, profile, observed, client); err != nil {
		return appThread{}, err
	}
	if !codexThreadHistoryReady(observed) {
		return appThread{}, fmt.Errorf("%w for %s; run `codex migrate-rollouts --apply` and retry", ErrCodexHistoryProjectionUnavailable, threadID)
	}
	var resumed struct {
		Thread appThread `json:"thread"`
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/resume", map[string]any{
		"threadId": threadID, "excludeTurns": true,
	}, &resumed); err != nil {
		return appThread{}, err
	}
	if resumed.Thread.ID != threadID {
		return appThread{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	if resumed.Thread.Cwd == "" {
		resumed.Thread.Cwd = observed.Cwd
	}
	if resumed.Thread.Source == nil {
		resumed.Thread.Source = observed.Source
	}
	if err := validateCodexDaemonLaneThread(lane, profile, resumed.Thread, client); err != nil {
		return appThread{}, err
	}
	return resumed.Thread, nil
}

func validateCodexDaemonLaneThread(
	lane daemonpkg.LaneRecord,
	profile string,
	thread appThread,
	client *appServerClient,
) error {
	expectedID := strings.TrimSpace(stringValue(lane.NativeActor["thread_id"]))
	if expectedID != "" && thread.ID != expectedID {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	expectedCwd := lane.Cwd
	if nativeCwd := strings.TrimSpace(stringValue(lane.NativeActor["cwd"])); nativeCwd != "" {
		expectedCwd = nativeCwd
	}
	if !validSessionID(thread.ID) || validatePreparedRootThread(thread) != nil ||
		(strings.TrimSpace(expectedCwd) != "" && thread.Cwd != expectedCwd) ||
		!matchesOptionalString(lane.NativeActor["profile"], profile) ||
		!matchesOptionalNumber(lane.NativeActor["pid"], client.peerPID) ||
		!matchesOptionalString(lane.NativeActor["proc_start"], client.peerProcStart) {
		return daemonpkg.ErrAttachmentEvidenceChanged
	}
	return nil
}

func codexDaemonLaneActor(profile string, thread appThread, client *appServerClient) map[string]any {
	return map[string]any{
		"thread_id": thread.ID, "profile": profile, "cwd": thread.Cwd, "name": thread.Name,
		"pid": client.peerPID, "proc_start": client.peerProcStart, "history_ready": true,
	}
}

func codexDaemonLaneIdentities(lane daemonpkg.LaneRecord, turn daemonpkg.LaneTurnRecord) (string, string, error) {
	if lane.Product != "codex" || turn.LaneSessionID != lane.LaneSessionID {
		return "", "", daemonpkg.ErrAttachmentEvidenceChanged
	}
	threadID := strings.TrimSpace(stringValue(lane.NativeActor["thread_id"]))
	turnID := strings.TrimSpace(stringValue(turn.NativeTurnIdentity["turn_id"]))
	expectedThreadID := strings.TrimSpace(stringValue(turn.NativeTurnIdentity["thread_id"]))
	if threadID == "" || turnID == "" || (expectedThreadID != "" && expectedThreadID != threadID) {
		return "", "", daemonpkg.ErrAttachmentEvidenceChanged
	}
	return threadID, turnID, nil
}

func codexDaemonLanePrompt(reference map[string]any) (string, error) {
	prompt := stringValue(reference["content"])
	if prompt == "" {
		prompt = stringValue(reference["prompt"])
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("codex lane turn requires a non-empty durable input reference")
	}
	return prompt, nil
}

func readExactCodexDaemonLaneTurn(
	ctx context.Context,
	client *appServerClient,
	threadID, turnID, itemsView string,
) (appTurn, error) {
	turns, err := listCodexDaemonLaneTurns(ctx, client, threadID, itemsView)
	if err != nil {
		return appTurn{}, err
	}
	for _, turn := range turns {
		if turn.ID == turnID {
			return turn, nil
		}
	}
	return appTurn{}, daemonpkg.ErrAttachmentEvidenceChanged
}

func codexDaemonLaneTurnState(status any) (string, string, error) {
	normalized := statusType(status)
	if normalized == "active" || normalized == "inProgress" || normalized == "in_progress" || normalized == "running" {
		return daemonpkg.LaneDispatchRunning, "", nil
	}
	switch normalized {
	case "completed":
		return daemonpkg.LaneDispatchCompleted, daemonpkg.LaneDispatchCompleted, nil
	case "interrupted":
		return daemonpkg.LaneDispatchInterrupted, daemonpkg.LaneDispatchInterrupted, nil
	case "failed":
		return daemonpkg.LaneDispatchFailed, daemonpkg.LaneDispatchFailed, nil
	default:
		return "", "", fmt.Errorf("unsupported Codex native turn status %q", normalized)
	}
}

func listCodexDaemonLaneTurns(
	ctx context.Context,
	client *appServerClient,
	threadID, itemsView string,
) ([]appTurn, error) {
	turns := make([]appTurn, 0)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for len(turns) < 1000 {
		params := map[string]any{
			"threadId": threadID, "limit": 100, "sortDirection": "desc", "itemsView": itemsView,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appTurn `json:"data"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/turns/list", params, &page); err != nil {
			return nil, err
		}
		turns = append(turns, page.Data...)
		if page.NextCursor == "" {
			return turns, nil
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return turns, nil
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return turns, nil
}

func codexDaemonLaneHasFinalAnswer(turn appTurn) bool {
	for _, raw := range turn.Items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		itemType := stringValue(item["type"])
		if (itemType == "agentMessage" || itemType == "agent_message") && stringValue(item["phase"]) == "final_answer" {
			return true
		}
	}
	return false
}

func codexDaemonLaneTurnIdentity(threadID, turnID, status string) map[string]any {
	return map[string]any{"thread_id": threadID, "turn_id": turnID, "status": status}
}

func codexDaemonLaneResult(threadID string, turn appTurn, outcome string) map[string]any {
	items := append([]json.RawMessage(nil), turn.Items...)
	return map[string]any{
		"thread_id": threadID, "turn_id": turn.ID, "status": outcome, "items": items,
		"error": turn.Error, "started_at": turn.StartedAt, "completed_at": turn.CompletedAt,
		"duration_ms": turn.DurationMS,
	}
}

func (coordinator *codexAppServerCoordinator) client(ctx context.Context, profile string) (*appServerClient, error) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if current := coordinator.clients[profile]; current != nil {
		select {
		case <-current.done:
			delete(coordinator.clients, profile)
		default:
			return current, nil
		}
	}
	socket := filepath.Join(profile, "app-server-control", "app-server-control.sock")
	client, err := dialAppServer(ctx, socket)
	if err != nil {
		return nil, fmt.Errorf("connect Codex App Server for profile %s: %w", profile, err)
	}
	coordinator.clients[profile] = client
	return client, nil
}

func (coordinator *codexAppServerCoordinator) close() {
	coordinator.mu.Lock()
	clients := coordinator.clients
	coordinator.clients = make(map[string]*appServerClient)
	coordinator.mu.Unlock()
	for _, client := range clients {
		client.close()
	}
}

func canonicalCodexProfile(configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configured = filepath.Join(home, ".codex")
	}
	canonical, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve Codex profile: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = resolved
	}
	return filepath.Clean(canonical), nil
}

func codexDaemonExecutable() (string, error) {
	configured := strings.TrimSpace(os.Getenv("CODEX_PEER_CODEX_BIN"))
	if configured == "" {
		configured = "codex"
	}
	executable, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("resolve Codex executable %q: %w", configured, err)
	}
	return executable, nil
}

func codexAppServerRequest(ctx context.Context, client *appServerClient, timeout time.Duration, method string, params, output any) error {
	requestContext := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > timeout {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	return client.request(requestContext, method, params, output)
}

func readCodexDaemonThread(ctx context.Context, client *appServerClient, threadID string, includeTurns bool) (appThread, error) {
	var read struct {
		Thread appThread `json:"thread"`
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/read", map[string]any{
		"threadId": threadID, "includeTurns": includeTurns,
	}, &read); err != nil {
		return appThread{}, err
	}
	if read.Thread.ID != threadID {
		return appThread{}, daemonpkg.ErrAttachmentEvidenceChanged
	}
	return read.Thread, nil
}

func codexThreadHistoryReady(thread appThread) bool {
	if len(thread.Turns) > 0 || strings.TrimSpace(thread.Path) == "" {
		return true
	}
	info, err := os.Stat(thread.Path)
	// A newly named zero-turn rollout is small. A non-trivial rollout with no
	// projected turns is the silent blank-history condition exposed by remote
	// App Server resume; native Codex migration is the only supported repair.
	return err != nil || info.Size() <= 64*1024
}

func resolveCodexDaemonThread(ctx context.Context, client *appServerClient, profile, target string) (appThread, error) {
	if target == "" {
		return appThread{}, errors.New("codex resume requires an exact UUID or session name")
	}
	if exactLaunchThreadIDRE.MatchString(target) {
		thread, err := readCodexDaemonThread(ctx, client, target, false)
		if err != nil {
			return appThread{}, err
		}
		return thread, validatePreparedRootThread(thread)
	}
	archived, err := codexThreadMembership(ctx, client, true)
	if err != nil {
		return appThread{}, err
	}
	found, err := findListedCodexDaemonThread(ctx, client, target, archived)
	if err != nil {
		return appThread{}, err
	}
	if found != nil {
		return *found, nil
	}
	return findIndexedCodexDaemonThread(ctx, client, profile, target, archived)
}

func findListedCodexDaemonThread(
	ctx context.Context,
	client *appServerClient,
	target string,
	archived map[string]bool,
) (*appThread, error) {
	var found *appThread
	err := visitCodexDaemonThreads(ctx, client, false, func(thread appThread) {
		if found == nil && thread.Name == target && !archived[thread.ID] && validatePreparedRootThread(thread) == nil {
			candidate := thread
			found = &candidate
		}
	})
	return found, err
}

func findIndexedCodexDaemonThread(
	ctx context.Context,
	client *appServerClient,
	profile, target string,
	archived map[string]bool,
) (appThread, error) {
	paths := nativePaths{codexHome: profile}
	index, err := readLaunchSessionIndex(paths)
	if err != nil {
		return appThread{}, err
	}
	seen := make(map[string]struct{})
	for position := len(index) - 1; position >= 0; position-- {
		entry := index[position]
		if _, duplicate := seen[entry.ID]; duplicate {
			continue
		}
		seen[entry.ID] = struct{}{}
		if entry.Name != target || archived[entry.ID] {
			continue
		}
		thread, readErr := readCodexDaemonThread(ctx, client, entry.ID, false)
		if readErr != nil || validatePreparedRootThread(thread) != nil {
			continue
		}
		thread.Name = entry.Name
		return thread, nil
	}
	return appThread{}, fmt.Errorf("codex thread %q was not found", target)
}

func visitCodexDaemonThreads(ctx context.Context, client *appServerClient, archived bool, visit func(appThread)) error {
	cursor := ""
	seen := make(map[string]struct{})
	for {
		params := map[string]any{"archived": archived, "limit": 100, "sortDirection": "desc", "sortKey": "updated_at"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []appThread `json:"data"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/list", params, &page); err != nil {
			return err
		}
		for _, thread := range page.Data {
			visit(thread)
		}
		if page.NextCursor == "" {
			return nil
		}
		if _, duplicate := seen[page.NextCursor]; duplicate {
			return nil
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

func codexThreadMembership(ctx context.Context, client *appServerClient, archived bool) (map[string]bool, error) {
	result := make(map[string]bool)
	err := visitCodexDaemonThreads(ctx, client, archived, func(thread appThread) {
		if validSessionID(thread.ID) {
			result[thread.ID] = true
		}
	})
	return result, err
}

func activeCodexTurn(ctx context.Context, client *appServerClient, threadID string) (string, error) {
	var page struct {
		Data []appTurn `json:"data"`
	}
	if err := codexAppServerRequest(ctx, client, 30*time.Second, "thread/turns/list", map[string]any{
		"threadId": threadID, "limit": 10, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &page); err != nil {
		return "", err
	}
	active := make([]string, 0, len(page.Data))
	for _, turn := range page.Data {
		if statusType(turn.Status) == "active" && turn.ID != "" {
			active = append(active, turn.ID)
		}
	}
	if len(active) == 0 {
		return "", fmt.Errorf("active Codex thread %s did not expose its active turn", threadID)
	}
	sort.Strings(active)
	return active[len(active)-1], nil
}

type daemonActorField struct {
	key   string
	value any
}

func inspectDaemonActor[T any](
	ctx context.Context,
	sessionID, product string,
	available bool,
	inspect func(context.Context, string) (T, error),
	verify func(T) (map[string]any, error),
) (map[string]any, error) {
	if !available || strings.TrimSpace(sessionID) == "" {
		return nil, daemonpkg.ErrAttachmentEvidenceChanged
	}
	actor, err := inspect(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("inspect %s native session: %w", product, err)
	}
	return verify(actor)
}

func matchesDaemonSession(record daemonpkg.AttachmentRecord, expectedID, observedID, cwd, profile string) bool {
	return observedID == expectedID && cwd == record.Cwd && matchesOptionalString(record.ProfileIdentity["profile"], profile)
}

func matchesDaemonActorEvidence(recorded, supplied map[string]any, fields ...daemonActorField) bool {
	for _, field := range fields {
		if !matchesOptionalValue(recorded[field.key], field.value) || !matchesOptionalValue(supplied[field.key], field.value) {
			return false
		}
	}
	return true
}

func matchesOptionalValue(expected, observed any) bool {
	if expected == nil {
		return true
	}
	switch value := observed.(type) {
	case string:
		return matchesOptionalString(expected, value)
	case int:
		return matchesOptionalNumber(expected, value)
	default:
		return reflect.DeepEqual(expected, observed)
	}
}

func matchesOptionalString(expected any, observed string) bool {
	return expected == nil || stringValue(expected) == "" || stringValue(expected) == observed
}

func matchesOptionalNumber(expected any, observed int) bool {
	if expected == nil {
		return true
	}
	switch value := expected.(type) {
	case int:
		return value == observed
	case int64:
		return value == int64(observed)
	case float64:
		return value == float64(observed)
	default:
		return reflect.DeepEqual(expected, observed)
	}
}
