package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
)

type hostCoordinator struct {
	ctx              context.Context
	stateRoot        string
	openCodex        func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error)
	reloadCodex      func(context.Context, *bridge.CodexNative) error
	unsubscribeCodex func(context.Context, string) error
	mu               sync.Mutex
	noticeMu         sync.Mutex
	codex            *bridge.CodexNative
	pending          map[string]daemonpkg.NativeEvidence
	monitored        map[string]bool
	claudePending    map[string]*claudePending
	grokPending      map[string]*grokPending
	grokObservers    map[string]*bridge.GrokNativeObserver
	qwenPending      map[string]*qwenPending
	claudeLanes      *daemonpkg.ClaudeLaneAdapter
	grokLanes        *daemonpkg.GrokLaneAdapter
	qwenLanes        *daemonpkg.QwenLaneAdapter
	lanes            map[string]*laneActor
	lanesLoaded      bool
	ownerReconciling bool
	now              func() time.Time
	runtime          *daemonpkg.Runtime
	runtimeReady     chan *daemonpkg.Runtime
	federation       *daemonpkg.Federation
}

func newHostCoordinator(ctx context.Context, stateRoot string) *hostCoordinator {
	coordinator := &hostCoordinator{
		ctx: ctx, stateRoot: stateRoot,
		openCodex: bridge.OpenCodexNative,
		reloadCodex: func(ctx context.Context, native *bridge.CodexNative) error {
			return native.ReloadMCPServers(ctx)
		},
		pending: map[string]daemonpkg.NativeEvidence{}, monitored: map[string]bool{},
		claudePending: map[string]*claudePending{},
		grokPending:   map[string]*grokPending{},
		grokObservers: map[string]*bridge.GrokNativeObserver{},
		qwenPending:   map[string]*qwenPending{},
		claudeLanes:   daemonpkg.NewClaudeLaneAdapter(),
		grokLanes:     daemonpkg.NewGrokLaneAdapter(),
		qwenLanes:     daemonpkg.NewQwenLaneAdapter(),
		lanes:         map[string]*laneActor{},
		runtimeReady:  make(chan *daemonpkg.Runtime, 1),
		now:           time.Now,
	}
	coordinator.unsubscribeCodex = func(ctx context.Context, threadID string) error {
		native, err := coordinator.codexNative()
		if err != nil {
			return err
		}
		return native.UnsubscribeThread(ctx, threadID)
	}
	return coordinator
}

// reconcileAttachmentOwners restores the owner monitors that are intentionally
// process-local while keeping attachment authority durable across daemon
// restarts. Dead owners are detached before the restarted daemon accepts their
// attachments as live; surviving owners get a fresh product monitor.
func (c *hostCoordinator) reconcileAttachmentOwners(runtime *daemonpkg.Runtime) error {
	c.mu.Lock()
	c.ownerReconciling = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ownerReconciling = false
		c.mu.Unlock()
	}()
	if err := c.ensureLaneActors(runtime); err != nil {
		c.mu.Lock()
		c.ownerReconciling = false
		c.mu.Unlock()
		return err
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	for id, attachment := range snapshot.Catalog.Attachments {
		if attachment.State != "attached" {
			continue
		}
		owner := attachment.Evidence.Process
		observation := procinfo.ObserveIdentity(owner)
		if observation.Status == procinfo.IdentityStale {
			if _, err := runtime.Attachments().Detach(context.Background(), id, "native-owner-exited"); err != nil {
				return err
			}
			c.archiveIdleLanesForParent(runtime, id)
			continue
		}
		if observation.Status != procinfo.IdentityMatches {
			return fmt.Errorf("attachment %s owner identity is not corroborated", id)
		}
		// An attachment is addressable only in the generation that has
		// recorroborated its native evidence. The previous implementation
		// restored only the process-local owner monitor, which left every
		// surviving peer inactive after a service restart.
		if _, err := runtime.Attachments().Refresh(context.Background(), id); err != nil {
			return fmt.Errorf("refresh attachment %s: %w", id, err)
		}
		switch attachment.Product {
		case "codex":
			c.startCodexOwnerMonitor(runtime, id, owner)
		case "claude":
			c.startClaudeOwnerMonitor(runtime, id, owner)
		case "grok":
			c.startGrokOwnerMonitor(runtime, id, owner)
		case "qwen":
			c.startQwenOwnerMonitor(runtime, id, owner)
		}
	}
	if err := c.reconcileOrphanedLanes(runtime); err != nil {
		c.mu.Lock()
		c.ownerReconciling = false
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.ownerReconciling = false
	c.mu.Unlock()
	return nil
}

func (c *hostCoordinator) adapters() map[string]daemonpkg.AttachmentAdapter {
	return map[string]daemonpkg.AttachmentAdapter{
		"codex": daemonpkg.NewCodexAttachmentAdapter(daemonpkg.CodexAdapterConfig{
			Prepare: func(_ context.Context, attachment daemonpkg.ManagedAttachment) (daemonpkg.NativeEvidence, error) {
				c.mu.Lock()
				defer c.mu.Unlock()
				evidence, ok := c.pending[attachment.ID]
				if !ok {
					return daemonpkg.NativeEvidence{}, errors.New("codex native preparation is unavailable")
				}
				return evidence, nil
			},
			Refresh: c.refreshCodexAttachment,
			Detach: func(ctx context.Context, attachment daemonpkg.ManagedAttachment) error {
				if err := c.unsubscribeCodex(ctx, attachment.NativeSessionID); err != nil {
					return fmt.Errorf("unsubscribe detached Codex thread: %w", err)
				}
				c.mu.Lock()
				delete(c.pending, attachment.ID)
				delete(c.monitored, attachment.ID)
				c.mu.Unlock()
				return nil
			},
			Rollback: func(_ context.Context, attachment daemonpkg.ManagedAttachment) error {
				c.mu.Lock()
				delete(c.pending, attachment.ID)
				delete(c.monitored, attachment.ID)
				c.mu.Unlock()
				return nil
			},
		}),
		"claude": c.claudeAdapter(),
		"grok":   c.grokAdapter(),
		"qwen":   c.qwenAdapter(),
	}
}

func (c *hostCoordinator) run(ctx context.Context) error {
	var runtime *daemonpkg.Runtime
	select {
	case <-ctx.Done():
		return nil
	case runtime = <-c.runtimeReady:
	}
	federationHost, err := c.newFederationHost(runtime)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.federation = federationHost
	c.mu.Unlock()
	federationDone := make(chan error, 1)
	go func() { federationDone <- federationHost.Run(ctx) }()
	for {
		select {
		case <-ctx.Done():
			goto shutdown
		case err = <-federationDone:
			if err != nil {
				return fmt.Errorf("run daemon federation component: %w", err)
			}
			goto shutdown
		}
	}

shutdown:
	c.mu.Lock()
	native := c.codex
	c.codex = nil
	grokObservers := c.grokObservers
	c.grokObservers = map[string]*bridge.GrokNativeObserver{}
	cancels := make([]context.CancelFunc, 0, len(c.lanes))
	for _, actor := range c.lanes {
		if actor.cancel != nil {
			cancels = append(cancels, actor.cancel)
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if native != nil {
		native.Close()
	}
	for _, observer := range grokObservers {
		observer.Close()
	}
	return nil
}

func (c *hostCoordinator) publishRuntime(runtime *daemonpkg.Runtime) {
	c.mu.Lock()
	c.runtime = runtime
	c.mu.Unlock()
	select {
	case c.runtimeReady <- runtime:
	default:
	}
}

//nolint:gocyclo // Control operation dispatch is centralized to keep role and payload checks auditable.
func (c *hostCoordinator) handle(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request daemonpkg.ControlRequest,
) (json.RawMessage, error) {
	switch request.Operation {
	case "attachment.codex.prepare":
		if runtime == nil {
			return nil, errors.New("runtime attachment authority is unavailable")
		}
		var input launcher.CodexDaemonPrepareRequest
		if json.Unmarshal(request.Payload, &input) != nil {
			return nil, errors.New("decode Codex preparation failed")
		}
		result, err := c.prepareCodex(ctx, runtime, input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	case "attachment.claude.prepare":
		if runtime == nil {
			return nil, errors.New("runtime attachment authority is unavailable")
		}
		var input launcher.ClaudeDaemonPrepareRequest
		if json.Unmarshal(request.Payload, &input) != nil {
			return nil, errors.New("decode Claude preparation failed")
		}
		result, err := c.prepareClaude(ctx, runtime, input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	case "attachment.grok.prepare":
		if runtime == nil {
			return nil, errors.New("runtime attachment authority is unavailable")
		}
		var input launcher.GrokDaemonPrepareRequest
		if json.Unmarshal(request.Payload, &input) != nil {
			return nil, errors.New("decode Grok preparation failed")
		}
		result, err := c.prepareGrok(ctx, runtime, input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	case "attachment.qwen.prepare":
		if runtime == nil {
			return nil, errors.New("runtime attachment authority is unavailable")
		}
		var input launcher.QwenDaemonPrepareRequest
		if json.Unmarshal(request.Payload, &input) != nil {
			return nil, errors.New("decode Qwen preparation failed")
		}
		result, err := c.prepareQwen(ctx, runtime, input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	case "connector.initialize", "connector.tools", "connector.call":
		if runtime == nil {
			return nil, errors.New("runtime connector authority is unavailable")
		}
		return c.handleConnector(ctx, runtime, request)
	case "lane.command":
		if runtime == nil {
			return nil, errors.New("runtime lane authority is unavailable")
		}
		return c.handleLaneCommand(ctx, runtime, request)
	case "roster":
		if runtime == nil {
			return nil, errors.New("runtime operator roster is unavailable")
		}
		return c.operatorRoster(runtime)
	case "hook.event":
		return json.Marshal(map[string]any{})
	default:
		return nil, fmt.Errorf("operation %s is not implemented", request.Operation)
	}
}

//nolint:gocyclo // Native thread, App Server, resume, attachment, and rollback gates form one transaction.
func (c *hostCoordinator) prepareCodex(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request launcher.CodexDaemonPrepareRequest,
) (launcher.CodexDaemonPrepareResult, error) {
	if procinfo.ObserveIdentity(request.Owner).Status != procinfo.IdentityMatches {
		return launcher.CodexDaemonPrepareResult{}, errors.New("codex launcher identity is not live")
	}
	native, err := c.codexNative()
	if err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	cwd := request.Cwd
	var thread bridge.CodexNativeThread
	switch request.Mode {
	case "fresh":
		thread, err = native.StartThread(ctx, bridge.CodexStartRequest{
			Cwd: request.Cwd, Name: request.Name, NameSource: request.NameSource,
			ApprovalPolicy: request.ApprovalPolicy, Sandbox: request.Sandbox,
		})
	case "resume":
		thread, err = native.ResolveThread(ctx, request.Target)
		if err == nil {
			request, err = resolveCodexResumeRequest(runtime, request, thread)
		}
		if err == nil {
			// Match native `codex resume` and the established peer launcher:
			// every resume uses the launcher's effective cwd. The thread's
			// persisted cwd is historical metadata and may name a workspace
			// that has since moved or been removed.
			cwd = codexResumeCwd(request, thread)
			thread, err = native.ResumeThread(ctx, thread.ID, cwd, request.ApprovalPolicy, request.Sandbox)
		}
	default:
		err = fmt.Errorf("unsupported Codex peer mode %q", request.Mode)
	}
	if err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	appPID, appStart, socket := native.AppServerEvidence()
	appServer, err := procinfo.CaptureIdentity(appPID)
	if err != nil || appStart == "" || appServer.Start != appStart {
		if request.Mode == "fresh" {
			_ = native.DeleteThread(context.Background(), thread.ID)
		}
		return launcher.CodexDaemonPrepareResult{}, errors.New("codex App Server identity changed during preparation")
	}
	evidence := daemonpkg.NativeEvidence{
		Process: request.Owner, Ancestry: []procinfo.Identity{appServer}, Executable: codexBinary(),
		SocketPath: socket, ThreadID: thread.ID,
	}
	c.mu.Lock()
	c.pending[thread.ID] = evidence
	c.mu.Unlock()
	capability, err := randomCapability()
	if err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	permission := "default"
	if request.ApprovalPolicy == "never" {
		permission = "bypassPermissions"
	}
	launchIntent := "non_yolo"
	if permission == "bypassPermissions" {
		launchIntent = "yolo"
	}
	prepared, err := runtime.Attachments().Prepare(ctx, daemonpkg.ManagedAttachment{
		ID: thread.ID, CapabilityHash: daemonpkg.CapabilityDigest(capability), Product: "codex",
		ProfileIdentity: codexHome(), LaunchIntent: launchIntent,
		NativeSessionID: thread.ID, Cwd: cwd,
		Groups: append([]string(nil), request.Groups...), PermissionMode: permission,
	})
	if err == nil {
		_, err = runtime.Attachments().Adopt(ctx, prepared.ID, evidence)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, thread.ID)
		c.mu.Unlock()
		if request.Mode == "fresh" {
			_ = native.DeleteThread(context.Background(), thread.ID)
		}
		return launcher.CodexDaemonPrepareResult{}, err
	}
	_ = runtime.Attachments().ObserveNativeTitle(thread.ID, thread.ID, bridge.NormalizePeerName(thread.Name))
	c.startCodexOwnerMonitor(runtime, thread.ID, request.Owner)
	return launcher.CodexDaemonPrepareResult{ThreadID: thread.ID, Cwd: cwd}, nil
}

func resolveCodexResumeRequest(
	runtime *daemonpkg.Runtime,
	request launcher.CodexDaemonPrepareRequest,
	thread bridge.CodexNativeThread,
) (launcher.CodexDaemonPrepareRequest, error) {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return launcher.CodexDaemonPrepareRequest{}, err
	}
	selected, found := snapshot.Catalog.Attachments[thread.ID]
	if !found {
		return request, nil
	}
	if selected.Product != "codex" || selected.ProfileIdentity != codexHome() ||
		selected.NativeSessionID != thread.ID {
		return launcher.CodexDaemonPrepareRequest{}, errors.New("managed Codex resume identity does not match the selected native thread")
	}
	if selected.State != "detached" {
		return launcher.CodexDaemonPrepareRequest{}, errors.New("managed Codex session is already live")
	}
	return inheritCodexResumeRequest(request, selected), nil
}

func inheritCodexResumeRequest(
	request launcher.CodexDaemonPrepareRequest,
	selected daemonpkg.ManagedAttachment,
) launcher.CodexDaemonPrepareRequest {
	if !request.GroupsSpecified {
		request.Groups = append([]string(nil), selected.Groups...)
	}
	if !request.PermissionSpecified {
		if selected.LaunchIntent == "yolo" || selected.PermissionMode == "bypassPermissions" {
			request.ApprovalPolicy = "never"
			request.Sandbox = "danger-full-access"
		} else {
			request.ApprovalPolicy = ""
			request.Sandbox = ""
		}
	}
	return request
}

func codexResumeCwd(request launcher.CodexDaemonPrepareRequest, _ bridge.CodexNativeThread) string {
	return request.Cwd
}

func (c *hostCoordinator) startCodexOwnerMonitor(runtime *daemonpkg.Runtime, id string, owner procinfo.Identity) {
	c.mu.Lock()
	if c.monitored[id] {
		c.mu.Unlock()
		return
	}
	c.monitored[id] = true
	c.mu.Unlock()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				observation := procinfo.ObserveIdentity(owner)
				if observation.Status == procinfo.IdentityStale {
					if _, err := runtime.Attachments().Detach(context.Background(), id, "native-owner-exited"); err == nil {
						c.archiveIdleLanesForParent(runtime, id)
					}
					return
				}
			}
		}
	}()
}

func (c *hostCoordinator) refreshCodexAttachment(
	ctx context.Context,
	attachment daemonpkg.ManagedAttachment,
) (daemonpkg.NativeEvidence, error) {
	if procinfo.ObserveIdentity(attachment.Evidence.Process).Status != procinfo.IdentityMatches {
		return daemonpkg.NativeEvidence{}, errors.New("codex TUI owner is not live")
	}
	appServer := procinfo.Identity{}
	if len(attachment.Evidence.Ancestry) > 0 {
		appServer = attachment.Evidence.Ancestry[0]
	}
	if procinfo.ObserveIdentity(appServer).Status != procinfo.IdentityMatches {
		return daemonpkg.NativeEvidence{}, errors.New("codex App Server is not live")
	}
	native, err := c.codexNative()
	if err != nil {
		return daemonpkg.NativeEvidence{}, err
	}
	if _, err := native.ReattachThread(ctx, attachment.NativeSessionID); err != nil {
		return daemonpkg.NativeEvidence{}, fmt.Errorf("reattach Codex App Server thread: %w", err)
	}
	return attachment.Evidence, nil
}

// authorizeLaneConnector preserves the legacy Codex lane boundary. Codex
// hosts every MCP connector below its shared App Server rather than below the
// individual `codex exec` worker, so per-worker environment capabilities are
// unavailable at the connector. The native thread metadata and the exact live
// App Server ancestry are the established product-owned proof instead.
func (c *hostCoordinator) authorizeLaneConnector(
	_ context.Context,
	lane daemonpkg.Lane,
	observed daemonpkg.NativeEvidence,
) error {
	if lane.Product != "codex" || lane.NativeSessionID == "" || observed.ThreadID != lane.NativeSessionID ||
		procinfo.ObserveIdentity(observed.Process).Status != procinfo.IdentityMatches {
		return errors.New("codex lane connector does not match its native thread")
	}
	native, err := c.codexNative()
	if err != nil {
		return err
	}
	appPID, appStart, _ := native.AppServerEvidence()
	appServer, err := procinfo.CaptureIdentity(appPID)
	if err != nil || appStart == "" || appServer.Start != appStart ||
		procinfo.ObserveIdentity(appServer).Status != procinfo.IdentityMatches {
		return errors.New("codex App Server identity is not live")
	}
	for _, ancestor := range observed.Ancestry {
		if sameProcessIdentity(ancestor, appServer) {
			return nil
		}
	}
	return errors.New("codex lane connector is not hosted by the managed App Server")
}

func (c *hostCoordinator) codexNative() (*bridge.CodexNative, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codex != nil {
		return c.codex, nil
	}
	native, err := c.openCodex(c.ctx, bridge.CodexNativeConfig{
		CodexBinary: codexBinary(), CodexHome: codexHome(), Environment: os.Environ(),
		OnEvent: c.observeCodexNativeEvent,
	})
	if err != nil {
		return nil, err
	}
	// Codex owns MCP children inside its long-lived App Server. Replacing the
	// Agent Sessions installation or restarting this daemon must therefore
	// refresh the App Server's plugin inventory once per daemon generation;
	// otherwise Codex can retain a dead or previous-image connector even after
	// the TUI is resumed. This supported reload preserves the App Server and its
	// native threads while replacing only its MCP children.
	if err := c.reloadCodex(c.ctx, native); err != nil {
		native.Close()
		return nil, fmt.Errorf("reload Codex MCP connectors: %w", err)
	}
	c.codex = native
	return native, nil
}

func (c *hostCoordinator) observeCodexNativeEvent(event bridge.CodexNativeEvent) {
	if event.Kind != "thread/name/updated" || strings.TrimSpace(event.ThreadID) == "" {
		return
	}
	c.mu.Lock()
	runtime := c.runtime
	c.mu.Unlock()
	if runtime != nil {
		_ = runtime.Attachments().ObserveNativeTitle(
			event.ThreadID, event.ThreadID, bridge.NormalizePeerName(event.Name),
		)
	}
}

func requestCodexPreparation(
	ctx context.Context,
	request launcher.CodexDaemonPrepareRequest,
) (launcher.CodexDaemonPrepareResult, error) {
	return requestPreparation[launcher.CodexDaemonPrepareRequest, launcher.CodexDaemonPrepareResult](
		ctx, "attachment.codex.prepare", request,
	)
}

func codexBinary() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_PEER_CODEX_BIN")); value != "" {
		return value
	}
	if value, err := exec.LookPath("codex"); err == nil {
		return value
	}
	return "codex"
}

func codexHome() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "codex-home-unavailable")
	}
	return filepath.Join(home, ".codex")
}

func randomCapability() (string, error) {
	body := make([]byte, 32)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return hex.EncodeToString(body), nil
}
