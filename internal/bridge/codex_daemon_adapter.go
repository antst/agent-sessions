package bridge

import (
	"context"
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
