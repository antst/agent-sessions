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
	"github.com/antst/agent-sessions/internal/claudeprofile"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	claudeproduct "github.com/antst/agent-sessions/internal/products/claude"
	codexproduct "github.com/antst/agent-sessions/internal/products/codex"
	kiloproduct "github.com/antst/agent-sessions/internal/products/kilocode"
	ompproduct "github.com/antst/agent-sessions/internal/products/omp"
	opencodeproduct "github.com/antst/agent-sessions/internal/products/opencode"
	"github.com/antst/agent-sessions/internal/products/opencodefamily"
	piproduct "github.com/antst/agent-sessions/internal/products/pi"
	"github.com/antst/agent-sessions/internal/products/pifamily"
	qwenproduct "github.com/antst/agent-sessions/internal/products/qwen"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/structuredprocess"
)

const (
	laneCandidateCodex  = "codex"
	laneCandidateClaude = "claude"
	laneCandidateGrok   = "grok"
	laneCandidateQwen   = "qwen"
)

type hostCoordinator struct {
	ctx                context.Context
	stateRoot          string
	openCodex          func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error)
	reloadCodex        func(context.Context, *bridge.CodexNative) error
	unsubscribeCodex   func(context.Context, string) error
	mu                 sync.Mutex
	noticeMu           sync.Mutex
	codex              *bridge.CodexNative
	pending            map[string]daemonpkg.NativeEvidence
	monitored          map[string]bool
	grokLanes          *daemonpkg.GrokLaneAdapter
	laneProcesses      *structuredprocess.Supervisor
	laneDrivers        *productruntime.LaneRegistry
	lanes              map[string]*laneActor
	liveReports        map[string]liveSessionReport
	reportedLanes      map[string]string
	reportedPeers      map[string]bool
	laneNames          map[string]map[string]laneNameEntry
	laneNamesLoaded    map[string]bool
	presence           *livePresenceServer
	resolveCandidate   func(context.Context, *daemonpkg.Runtime, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool)
	candidateResolvers map[string]func(context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool)
	lanesLoaded        bool
	now                func() time.Time
	runtime            *daemonpkg.Runtime
	runtimeReady       chan *daemonpkg.Runtime
	federation         *daemonpkg.Federation
}

func newHostCoordinator(ctx context.Context, stateRoot string) *hostCoordinator {
	coordinator := &hostCoordinator{
		ctx: ctx, stateRoot: stateRoot,
		openCodex: bridge.OpenCodexNative,
		reloadCodex: func(ctx context.Context, native *bridge.CodexNative) error {
			return native.ReloadMCPServers(ctx)
		},
		pending: map[string]daemonpkg.NativeEvidence{}, monitored: map[string]bool{},
		grokLanes:       daemonpkg.NewGrokLaneAdapter(),
		lanes:           map[string]*laneActor{},
		liveReports:     map[string]liveSessionReport{},
		reportedLanes:   map[string]string{},
		reportedPeers:   map[string]bool{},
		laneNames:       map[string]map[string]laneNameEntry{},
		laneNamesLoaded: map[string]bool{},
		runtimeReady:    make(chan *daemonpkg.Runtime, 1),
		now:             time.Now,
	}
	coordinator.resolveCandidate = coordinator.resolveProductLaneCandidate
	coordinator.candidateResolvers = coordinator.productLaneCandidateResolvers()
	laneProcesses, err := structuredprocess.NewSupervisor()
	if err != nil {
		panic(err)
	}
	coordinator.laneProcesses = laneProcesses
	qwenDescriptor, ok := productcatalog.ByID(qwenproduct.ProductID)
	if !ok {
		panic("Qwen product descriptor is unavailable")
	}
	qwenProcesses, err := qwenproduct.NewStructuredProcessFactory(laneProcesses)
	if err != nil {
		panic(err)
	}
	hostExecutable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	claudeDescriptor, ok := productcatalog.ByID(claudeproduct.ProductID)
	if !ok {
		panic("Claude product descriptor is unavailable")
	}
	claudeProcesses, err := claudeproduct.NewStructuredProcessFactory(laneProcesses)
	if err != nil {
		panic(err)
	}
	claudeLanes, err := claudeproduct.NewLaneDriver(claudeproduct.LaneConfig{
		Descriptor: claudeDescriptor, HostExecutable: hostExecutable,
		Generation: 1, Processes: claudeProcesses,
	})
	if err != nil {
		panic(err)
	}
	qwenLanes, err := qwenproduct.NewLaneDriver(qwenproduct.LaneConfig{
		Executable: qwenDescriptor.NativeExecutable, HostExecutable: hostExecutable,
		Generation: 1, Processes: qwenProcesses,
	})
	if err != nil {
		panic(err)
	}
	codexLanes, err := codexproduct.NewLaneDriver(func() (codexproduct.LaneNative, error) {
		return coordinator.codexNative()
	})
	if err != nil {
		panic(err)
	}
	opencodeDescriptor, ok := productcatalog.ByID(opencodeproduct.ProductID)
	if !ok {
		panic("OpenCode product descriptor is unavailable")
	}
	opencodeServers, err := opencodefamily.NewOwnedServerManager(opencodefamily.OwnedServerManagerConfig{
		ProductID: opencodeproduct.ProductID, Dialect: opencodefamily.DialectOpenCode,
		Executable: opencodeDescriptor.NativeExecutable, Supervisor: laneProcesses,
	})
	if err != nil {
		panic(err)
	}
	opencodeLanes, err := opencodeproduct.NewLaneDriver(opencodeproduct.Config{
		Deps: productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses}, Servers: opencodeServers,
	})
	if err != nil {
		panic(err)
	}
	kiloDescriptor, ok := productcatalog.ByID(kiloproduct.ProductID)
	if !ok {
		panic("Kilo product descriptor is unavailable")
	}
	kiloServers, err := opencodefamily.NewOwnedServerManager(opencodefamily.OwnedServerManagerConfig{
		ProductID: kiloproduct.ProductID, Dialect: opencodefamily.DialectKilo,
		Executable: kiloDescriptor.NativeExecutable, Supervisor: laneProcesses,
	})
	if err != nil {
		panic(err)
	}
	kiloLanes, err := kiloproduct.NewLaneDriver(kiloproduct.Config{
		Deps: productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses}, Servers: kiloServers,
	})
	if err != nil {
		panic(err)
	}
	piDescriptor, ok := productcatalog.ByID(piproduct.ProductID)
	if !ok {
		panic("Pi product descriptor is unavailable")
	}
	piFamilyProcesses, err := pifamily.NewStructuredProcessFactory(laneProcesses)
	if err != nil {
		panic(err)
	}
	piLanes, err := piproduct.NewLaneDriver(piproduct.Config{
		Deps:       productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses},
		Executable: piDescriptor.NativeExecutable, Processes: piFamilyProcesses,
	})
	if err != nil {
		panic(err)
	}
	ompDescriptor, ok := productcatalog.ByID(ompproduct.ProductID)
	if !ok {
		panic("OMP product descriptor is unavailable")
	}
	ompLanes, err := ompproduct.NewLaneDriver(ompproduct.Config{
		Deps:       productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses},
		Executable: ompDescriptor.NativeExecutable, Processes: piFamilyProcesses,
	})
	if err != nil {
		panic(err)
	}
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{
		claudeproduct.ProductID:   claudeLanes,
		codexproduct.ProductID:    codexLanes,
		opencodeproduct.ProductID: opencodeLanes,
		kiloproduct.ProductID:     kiloLanes,
		piproduct.ProductID:       piLanes,
		ompproduct.ProductID:      ompLanes,
		qwenproduct.ProductID:     qwenLanes,
	})
	if err != nil {
		panic(err)
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
	}
}

func (c *hostCoordinator) run(ctx context.Context) error {
	defer func() { _ = c.laneProcesses.Close() }()
	var runtime *daemonpkg.Runtime
	select {
	case <-ctx.Done():
		return nil
	case runtime = <-c.runtimeReady:
	}
	presence, err := startLivePresenceServer(ctx, c.stateRoot,
		func(report liveSessionReport) { c.joinLiveSession(runtime, report) },
		func(report liveSessionReport) { c.leaveLiveSession(runtime, report) },
		func(callCtx context.Context, report liveSessionReport, method string, params json.RawMessage) (json.RawMessage, error) {
			return c.handleLiveSessionCall(callCtx, runtime, report, method, params)
		},
	)
	if err != nil {
		return fmt.Errorf("start live session presence: %w", err)
	}
	defer func() { _ = presence.Close() }()
	c.mu.Lock()
	c.presence = presence
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.presence == presence {
			c.presence = nil
		}
		c.mu.Unlock()
	}()
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

func (c *hostCoordinator) productLaneCandidateResolvers() map[string]func(
	context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate,
) (laneNameEntry, bool) {
	return map[string]func(context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool){
		laneCandidateCodex: func(ctx context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
			native, err := c.codexNative()
			if err != nil {
				return laneNameEntry{}, false
			}
			thread, err := native.ResolveThread(ctx, candidate.NativeSessionID)
			if err != nil || thread.ID != candidate.NativeSessionID {
				return laneNameEntry{}, false
			}
			return laneNameEntry{Name: thread.Name}, true
		},
		laneCandidateClaude: func(_ context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
			source, err := claudeprofile.CurrentSource()
			if err != nil {
				return laneNameEntry{}, false
			}
			name, ok := bridge.ClaudeNativeSessionTitle(source.ConfigRoot, candidate.NativeSessionID)
			return laneNameEntry{Name: name}, ok
		},
		laneCandidateQwen: func(_ context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
			profile, err := qwenprofile.Current()
			if err != nil {
				return laneNameEntry{}, false
			}
			home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
			if err != nil {
				return laneNameEntry{}, false
			}
			name, _, ok := bridge.QwenNativeSessionInfo(home, candidate.NativeSessionID)
			return laneNameEntry{Name: name}, ok
		},
		opencodeproduct.ProductID: productListLaneCandidateResolver(opencodeproduct.ProductID),
		kiloproduct.ProductID:     productListLaneCandidateResolver(kiloproduct.ProductID),
		piproduct.ProductID:       packageListLaneCandidateResolver(piproduct.ProductID, launcher.ListAllPiSessions),
		ompproduct.ProductID:      packageListLaneCandidateResolver(ompproduct.ProductID, launcher.ListAllOMPSessions),
	}
}

func packageListLaneCandidateResolver(
	product string,
	list func(context.Context, string) ([]launcher.ProductSession, error),
) func(context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool) {
	return func(ctx context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		executable, err := launcher.ResolveProductExecutable(product)
		if err != nil {
			return laneNameEntry{}, false
		}
		sessions, err := list(ctx, executable)
		if err != nil {
			return laneNameEntry{}, false
		}
		for _, session := range sessions {
			if session.ID == candidate.NativeSessionID {
				return laneNameEntry{Name: session.Title}, true
			}
		}
		return laneNameEntry{}, false
	}
}

func productListLaneCandidateResolver(product string) func(
	context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate,
) (laneNameEntry, bool) {
	return func(ctx context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
		executable, err := launcher.ResolveProductExecutable(product)
		if err != nil {
			return laneNameEntry{}, false
		}
		sessions, err := launcher.ListProductSessions(ctx, executable)
		if err != nil {
			return laneNameEntry{}, false
		}
		for _, session := range sessions {
			if session.ID == candidate.NativeSessionID {
				return laneNameEntry{Name: session.Title}, true
			}
		}
		return laneNameEntry{}, false
	}
}

func (c *hostCoordinator) resolveProductLaneCandidate(
	ctx context.Context,
	_ *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	candidate daemonpkg.LaneCandidate,
) (laneNameEntry, bool) {
	resolver := c.candidateResolvers[candidate.Product]
	if resolver == nil {
		return laneNameEntry{}, false
	}
	return resolver(ctx, parent, candidate)
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
	case "connector.tool":
		if runtime == nil {
			return nil, errors.New("runtime connector authority is unavailable")
		}
		return c.handleConnectorTool(ctx, runtime, request)
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
	if request.Mode == "list" {
		native, err := c.codexNative()
		if err != nil {
			return launcher.CodexDaemonPrepareResult{}, err
		}
		threads, err := native.ListPeerThreads(ctx)
		if err != nil {
			return launcher.CodexDaemonPrepareResult{}, err
		}
		candidates := make([]launcher.CodexResumeCandidate, 0, len(threads))
		for _, thread := range threads {
			candidates = append(candidates, launcher.CodexResumeCandidate{
				ID: thread.ID, Name: thread.Name, Cwd: thread.Cwd, UpdatedAt: thread.UpdatedAt,
			})
		}
		return launcher.CodexDaemonPrepareResult{Candidates: candidates}, nil
	}
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
	c.startCodexOwnerMonitor(runtime, thread.ID, request.Owner)
	return launcher.CodexDaemonPrepareResult{ThreadID: thread.ID, Name: thread.Name, Cwd: cwd}, nil
}

func runCodexNativePeer(ctx context.Context, launch launcher.CodexNativeLaunch) error {
	command := exec.Command(launch.Executable, launch.Arguments...) //nolint:gosec // product executable and argv were resolved by the launcher.
	command.Env = append([]string(nil), launch.Environment...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runLauncherHeldPeer(ctx, []launcherHeldChild{{
		role: "Codex TUI", command: command, primary: true,
	}}, func(context.Context) (launcherHeldIdentity, error) {
		report := liveSessionReport{
			UUID: launch.ThreadID, Name: launch.Name, Product: connectorProductCodex,
			Groups: append([]string(nil), launch.Groups...),
		}
		return launcherHeldIdentity{report: report, call: func(
			callCtx context.Context, method string, params json.RawMessage,
		) (json.RawMessage, error) {
			return connectorNativeCall(callCtx, report, method, params)
		}}, nil
	})
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

func (c *hostCoordinator) codexNative() (*bridge.CodexNative, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codex != nil {
		return c.codex, nil
	}
	native, err := c.openCodex(c.ctx, bridge.CodexNativeConfig{
		CodexBinary: codexBinary(), CodexHome: codexHome(), Environment: os.Environ(),
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
