package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antst/sessionbus/internal/bridge"
	"github.com/antst/sessionbus/internal/claudeprofile"
	daemonpkg "github.com/antst/sessionbus/internal/daemon"
	"github.com/antst/sessionbus/internal/launcher"
	"github.com/antst/sessionbus/internal/livepresence"
	"github.com/antst/sessionbus/internal/procinfo"
	"github.com/antst/sessionbus/internal/productcatalog"
	"github.com/antst/sessionbus/internal/productruntime"
	claudeproduct "github.com/antst/sessionbus/internal/products/claude"
	codexproduct "github.com/antst/sessionbus/internal/products/codex"
	dshproduct "github.com/antst/sessionbus/internal/products/dsh"
	grokproduct "github.com/antst/sessionbus/internal/products/grok"
	kiloproduct "github.com/antst/sessionbus/internal/products/kilocode"
	ompproduct "github.com/antst/sessionbus/internal/products/omp"
	opencodeproduct "github.com/antst/sessionbus/internal/products/opencode"
	"github.com/antst/sessionbus/internal/products/opencodefamily"
	piproduct "github.com/antst/sessionbus/internal/products/pi"
	"github.com/antst/sessionbus/internal/products/pifamily"
	qwenproduct "github.com/antst/sessionbus/internal/products/qwen"
	"github.com/antst/sessionbus/internal/qwenprofile"
	"github.com/antst/sessionbus/internal/structuredprocess"
)

const (
	laneCandidateCodex  = "codex"
	laneCandidateClaude = "claude"
	laneCandidateQwen   = "qwen"
)

type hostCoordinator struct {
	ctx                  context.Context
	stateRoot            string
	openCodex            func(context.Context, bridge.CodexNativeConfig) (*bridge.CodexNative, error)
	reloadCodex          func(context.Context, *bridge.CodexNative) error
	unsubscribeCodex     func(context.Context, string) error
	mu                   sync.Mutex
	noticeMu             sync.Mutex
	codex                *bridge.CodexNative
	pending              map[string]daemonpkg.NativeEvidence
	pendingCodexLaunches map[string]*pendingCodexLaunch
	monitored            map[string]bool
	laneProcesses        *structuredprocess.Supervisor
	laneDrivers          *productruntime.LaneRegistry
	lanes                map[string]*laneActor
	liveReports          map[string]livepresence.Report
	reportedLanes        map[string]string
	reportedPeers        map[string]bool
	laneNames            map[string]map[string]laneNameEntry
	presence             *livePresenceServer
	resolveCandidate     func(context.Context, *daemonpkg.Runtime, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool)
	candidateResolvers   map[string]func(context.Context, daemonpkg.ManagedAttachment, daemonpkg.LaneCandidate) (laneNameEntry, bool)
	liveTitleResolvers   map[string]func([]daemonpkg.ManagedAttachment) map[string]string
	lanesLoaded          bool
	now                  func() time.Time
	runtime              *daemonpkg.Runtime
	runtimeReady         chan *daemonpkg.Runtime
	federation           *daemonpkg.Federation
}

type pendingCodexLaunch struct {
	request               launcher.CodexDaemonPrepareRequest
	registrationUnixMilli int64
	ctx                   context.Context
	done                  chan pendingCodexLaunchResult
}

type codexLaunchObservation struct {
	event              bridge.CodexNativeEvent
	freshEligible      map[*pendingCodexLaunch]struct{}
	threadUnixMilli    uint64
	threadIDValidation error
}

type pendingCodexLaunchResult struct {
	handoff launcher.CodexDaemonPrepareResult
	err     error
}

func newHostCoordinator(ctx context.Context, stateRoot string) *hostCoordinator {
	coordinator := &hostCoordinator{
		ctx: ctx, stateRoot: stateRoot,
		openCodex: bridge.OpenCodexNative,
		reloadCodex: func(ctx context.Context, native *bridge.CodexNative) error {
			return native.ReloadMCPServers(ctx)
		},
		pending: map[string]daemonpkg.NativeEvidence{}, pendingCodexLaunches: map[string]*pendingCodexLaunch{},
		monitored:     map[string]bool{},
		lanes:         map[string]*laneActor{},
		liveReports:   map[string]livepresence.Report{},
		reportedLanes: map[string]string{},
		reportedPeers: map[string]bool{},
		laneNames:     map[string]map[string]laneNameEntry{},
		liveTitleResolvers: map[string]func([]daemonpkg.ManagedAttachment) map[string]string{
			claudeproduct.ProductID: func(attachments []daemonpkg.ManagedAttachment) map[string]string {
				titles := make(map[string]string, len(attachments))
				source, err := claudeprofile.CurrentSource()
				if err != nil {
					return titles
				}
				for _, attachment := range attachments {
					title, observed := bridge.ClaudeNativeSessionTitle(source.ConfigRoot, attachment.NativeSessionID)
					if observed && title != attachment.NativeSessionID {
						titles[attachment.ID] = title
					}
				}
				return titles
			},
			ompproduct.ProductID: func(attachments []daemonpkg.ManagedAttachment) map[string]string {
				titles := make(map[string]string, len(attachments))
				executable, err := launcher.ResolveProductExecutable(ompproduct.ProductID)
				if err != nil {
					return titles
				}
				sessions, err := launcher.ListAllOMPSessions(ctx, executable)
				if err != nil {
					return titles
				}
				byID := make(map[string]string, len(sessions))
				for _, session := range sessions {
					if strings.TrimSpace(session.Title) != "" {
						byID[session.ID] = session.Title
					}
				}
				for _, attachment := range attachments {
					if title := byID[attachment.NativeSessionID]; title != "" {
						titles[attachment.ID] = title
					}
				}
				return titles
			},
			grokproduct.ProductID: func(attachments []daemonpkg.ManagedAttachment) map[string]string {
				titles := make(map[string]string, len(attachments))
				executable, err := launcher.ResolveProductExecutable(grokproduct.ProductID)
				if err != nil {
					return titles
				}
				for _, attachment := range attachments {
					title, observed := bridge.GrokNativeSessionTitle(
						ctx, executable, attachment.Cwd, os.Environ(), io.Discard, attachment.NativeSessionID,
					)
					if observed {
						titles[attachment.ID] = title
					}
				}
				return titles
			},
		},
		runtimeReady: make(chan *daemonpkg.Runtime, 1),
		now:          time.Now,
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
		Descriptor: claudeDescriptor, Generation: 1, Processes: claudeProcesses,
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
	grokDescriptor, ok := productcatalog.ByID(grokproduct.ProductID)
	if !ok {
		panic("Grok product descriptor is unavailable")
	}
	grokNative, err := newGrokBridgeFactory(BridgeFactoryConfig{
		Executable: grokDescriptor.NativeExecutable, HostExecutable: hostExecutable,
		NativeToolGrant: append([]string(nil), grokDescriptor.NativeToolGrantArgs...),
	})
	if err != nil {
		panic(err)
	}
	grokLaneDriver, err := grokproduct.NewLaneDriver(grokproduct.LaneConfig{
		Descriptor: grokDescriptor, Generation: 1, Native: grokNative,
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
	piExtensionPath, err := launcher.ManagedIntegrationAsset(piproduct.ProductID, "agent-sessions.mjs")
	if err != nil {
		panic(err)
	}
	piLanes, err := piproduct.NewLaneDriver(piproduct.Config{
		Deps:       productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses},
		Executable: piDescriptor.NativeExecutable, ExtensionPath: piExtensionPath, Processes: piFamilyProcesses,
	})
	if err != nil {
		panic(err)
	}
	ompDescriptor, ok := productcatalog.ByID(ompproduct.ProductID)
	if !ok {
		panic("OMP product descriptor is unavailable")
	}
	ompExtensionPath, err := launcher.ManagedIntegrationAsset(ompproduct.ProductID, "agent-sessions.mjs")
	if err != nil {
		panic(err)
	}
	ompLanes, err := ompproduct.NewLaneDriver(ompproduct.Config{
		Deps:       productruntime.HostDeps{Generation: 1, OwnedProcesses: laneProcesses},
		Executable: ompDescriptor.NativeExecutable, ExtensionPath: ompExtensionPath, Processes: piFamilyProcesses,
	})
	if err != nil {
		panic(err)
	}
	dshDescriptor, ok := productcatalog.ByID(dshproduct.ProductID)
	if !ok {
		panic("DSH product descriptor is unavailable")
	}
	dshLanes, err := dshproduct.NewLaneDriver(dshproduct.LaneConfig{
		Executable: dshDescriptor.NativeExecutable, Profile: dshproduct.ManagedProfile,
		Generation: 1, Processes: laneProcesses, Presence: coordinatorDSHPresence{coordinator: coordinator},
	})
	if err != nil {
		panic(err)
	}
	coordinator.laneDrivers, err = productruntime.NewLaneRegistry(map[string]productruntime.LaneDriver{
		claudeproduct.ProductID:   claudeLanes,
		codexproduct.ProductID:    codexLanes,
		grokproduct.ProductID:     grokLaneDriver,
		opencodeproduct.ProductID: opencodeLanes,
		kiloproduct.ProductID:     kiloLanes,
		piproduct.ProductID:       piLanes,
		ompproduct.ProductID:      ompLanes,
		qwenproduct.ProductID:     qwenLanes,
		dshproduct.ProductID:      dshLanes,
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
		func(report livepresence.Report) { c.joinLiveSession(runtime, report) },
		func(report livepresence.Report) { c.leaveLiveSession(runtime, report) },
		func(callCtx context.Context, report livepresence.Report, requestID, method string, params json.RawMessage) (json.RawMessage, error) {
			return c.handleLiveSessionCall(callCtx, runtime, report, requestID, method, params)
		},
		func(report livepresence.Report) { c.retireDepartedLiveSession(runtime, report) },
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
		grokproduct.ProductID: func(ctx context.Context, parent daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
			executable, err := launcher.ResolveProductExecutable(grokproduct.ProductID)
			if err != nil {
				return laneNameEntry{}, false
			}
			name, ok := bridge.GrokNativeSessionTitle(
				ctx, executable, parent.Cwd, os.Environ(), io.Discard, candidate.NativeSessionID,
			)
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
		dshproduct.ProductID: func(ctx context.Context, _ daemonpkg.ManagedAttachment, candidate daemonpkg.LaneCandidate) (laneNameEntry, bool) {
			executable, err := launcher.ResolveProductExecutable(dshproduct.ProductID)
			if err != nil {
				return laneNameEntry{}, false
			}
			observed, found, err := dshproduct.InspectSession(ctx, executable, candidate.NativeSessionID, os.Environ())
			if err != nil || !found {
				return laneNameEntry{}, false
			}
			return laneNameEntry{Name: observed.Name}, true
		},
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
	case "attachment.codex.pending":
		if runtime == nil {
			return nil, errors.New("runtime attachment authority is unavailable")
		}
		var input launcher.CodexDaemonPrepareRequest
		if json.Unmarshal(request.Payload, &input) != nil {
			return nil, errors.New("decode Codex pending launch failed")
		}
		result, err := c.awaitCodexPendingLaunch(ctx, runtime, input)
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

func (c *hostCoordinator) awaitCodexPendingLaunch(
	ctx context.Context,
	_ *daemonpkg.Runtime,
	request launcher.CodexDaemonPrepareRequest,
) (launcher.CodexDaemonPrepareResult, error) {
	tokenBody, err := hex.DecodeString(request.PendingToken)
	if err != nil || len(tokenBody) != 32 {
		return launcher.CodexDaemonPrepareResult{}, errors.New("Codex pending launch token is invalid")
	}
	if err := validatePendingCodexLaunch(request); err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	native, err := c.codexNative()
	if err == nil {
		_, _, _, err = native.RefreshAppServerEvidence(ctx)
	}
	if err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	c.mu.Lock()
	if c.pendingCodexLaunches[request.PendingToken] != nil {
		c.mu.Unlock()
		return launcher.CodexDaemonPrepareResult{}, errors.New("Codex pending launch token is already active")
	}
	for _, waiting := range c.pendingCodexLaunches {
		if pendingCodexLaunchesOverlap(waiting.request, request) {
			pid := waiting.request.Owner.PID
			c.mu.Unlock()
			return launcher.CodexDaemonPrepareResult{}, fmt.Errorf("Codex launch already waiting in %s under wrapper pid %d", request.Cwd, pid)
		}
	}
	record := &pendingCodexLaunch{
		request: request, registrationUnixMilli: c.now().UnixMilli(), ctx: ctx,
		done: make(chan pendingCodexLaunchResult, 1),
	}
	c.pendingCodexLaunches[request.PendingToken] = record
	c.mu.Unlock()
	if !daemonpkg.AdmitControlCall(ctx) {
		c.mu.Lock()
		if c.pendingCodexLaunches[request.PendingToken] == record {
			delete(c.pendingCodexLaunches, request.PendingToken)
		}
		c.mu.Unlock()
		return launcher.CodexDaemonPrepareResult{}, errors.New("Codex pending launch admission is unavailable")
	}
	select {
	case result := <-record.done:
		return result.handoff, result.err
	case <-ctx.Done():
		c.mu.Lock()
		if c.pendingCodexLaunches[request.PendingToken] == record {
			delete(c.pendingCodexLaunches, request.PendingToken)
		}
		c.mu.Unlock()
		return launcher.CodexDaemonPrepareResult{}, ctx.Err()
	}
}

func validatePendingCodexLaunch(request launcher.CodexDaemonPrepareRequest) error {
	switch request.SelectorKind {
	case launcher.CodexLaunchSelectorFresh:
		if request.Selector != "" {
			return errors.New("fresh Codex pending launch selector is invalid")
		}
	case launcher.CodexLaunchSelectorBare:
		if request.Selector != "" {
			return errors.New("bare Codex pending launch selector is invalid")
		}
	case launcher.CodexLaunchSelectorName, launcher.CodexLaunchSelectorID:
		if strings.TrimSpace(request.Selector) == "" {
			return errors.New("Codex pending launch selector is invalid")
		}
	default:
		return fmt.Errorf("unsupported Codex pending launch selector kind %q", request.SelectorKind)
	}
	return nil
}

func pendingCodexLaunchesOverlap(first, second launcher.CodexDaemonPrepareRequest) bool {
	if first.Cwd != second.Cwd {
		return false
	}
	if first.SelectorKind == launcher.CodexLaunchSelectorFresh || first.SelectorKind == launcher.CodexLaunchSelectorBare ||
		second.SelectorKind == launcher.CodexLaunchSelectorFresh || second.SelectorKind == launcher.CodexLaunchSelectorBare {
		return true
	}
	return first.SelectorKind == second.SelectorKind && first.Selector == second.Selector
}

func pendingCodexLaunchMatches(
	record *pendingCodexLaunch,
	observation codexLaunchObservation,
	thread bridge.CodexNativeThread,
) bool {
	request := record.request
	if request.Cwd != thread.Cwd {
		return false
	}
	switch request.SelectorKind {
	case launcher.CodexLaunchSelectorFresh:
		_, eligible := observation.freshEligible[record]
		return observation.event.Kind == "thread/started" && observation.event.Cwd == request.Cwd &&
			eligible && observation.threadIDValidation == nil &&
			record.registrationUnixMilli >= 0 && observation.threadUnixMilli >= uint64(record.registrationUnixMilli)
	case launcher.CodexLaunchSelectorBare:
		return true
	case launcher.CodexLaunchSelectorName:
		return request.Selector == thread.Name
	case launcher.CodexLaunchSelectorID:
		return request.Selector == thread.ID
	default:
		return false
	}
}

func codexUUIDv7UnixMilli(value string) (uint64, error) {
	// RFC 9562 UUIDv7 stores its Unix millisecond timestamp in the first 48 bits.
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return 0, fmt.Errorf("Codex thread/started id %q is malformed: expected canonical 8-4-4-4-12 UUID", value)
	}
	compact := value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 {
		return 0, fmt.Errorf("Codex thread/started id %q is malformed: expected hexadecimal UUID", value)
	}
	if version := decoded[6] >> 4; version != 7 {
		return 0, fmt.Errorf("Codex thread/started id %q is not UUIDv7: version nibble is %x", value, version)
	}
	if variant := decoded[8] >> 6; variant != 2 {
		return 0, fmt.Errorf("Codex thread/started id %q is not UUIDv7: RFC 9562 variant bits are %02b", value, variant)
	}
	return uint64(decoded[0])<<40 | uint64(decoded[1])<<32 | uint64(decoded[2])<<24 |
		uint64(decoded[3])<<16 | uint64(decoded[4])<<8 | uint64(decoded[5]), nil
}

func (c *hostCoordinator) adoptCodexPeer(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request launcher.CodexDaemonPrepareRequest,
	native *bridge.CodexNative,
	thread bridge.CodexNativeThread,
) (launcher.CodexDaemonPrepareResult, error) {
	var err error
	if request.SelectorKind == launcher.CodexLaunchSelectorFresh {
		thread, err = native.NameResolvedThread(ctx, thread, request.Name, request.Cwd)
		if err != nil {
			return launcher.CodexDaemonPrepareResult{}, err
		}
	}
	if strings.TrimSpace(thread.Name) == "" {
		thread.Name = thread.ID
	}
	appPID, appStart, socket, err := native.RefreshAppServerEvidence(ctx)
	if err != nil {
		return launcher.CodexDaemonPrepareResult{}, err
	}
	appServer, err := procinfo.CaptureIdentity(appPID)
	if err != nil || appStart == "" || appServer.Start != appStart {
		return launcher.CodexDaemonPrepareResult{}, errors.New("codex App Server identity changed during attachment")
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
	prepared, err := runtime.Attachments().Prepare(ctx, daemonpkg.ManagedAttachment{
		ID: thread.ID, CapabilityHash: daemonpkg.CapabilityDigest(capability), Product: "codex",
		ProfileIdentity: codexHome(), LaunchIntent: map[bool]string{true: "yolo", false: "non_yolo"}[permission == "bypassPermissions"],
		NativeSessionID: thread.ID, Cwd: request.Cwd, Groups: append([]string(nil), request.Groups...),
		PermissionMode: permission,
	})
	if err == nil {
		_, err = runtime.Attachments().Adopt(ctx, prepared.ID, evidence)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, thread.ID)
		c.mu.Unlock()
		return launcher.CodexDaemonPrepareResult{}, err
	}
	c.startCodexOwnerMonitor(runtime, thread.ID, request.Owner)
	return launcher.CodexDaemonPrepareResult{ThreadID: thread.ID, Name: thread.Name, Cwd: request.Cwd}, nil
}

func runCodexNativePeer(ctx context.Context, launch launcher.CodexNativeLaunch) error {
	command := exec.Command(launch.Executable, launch.Arguments...) //nolint:gosec // product executable and argv were resolved by the launcher.
	command.Env = append([]string(nil), launch.Environment...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runLauncherHeldPeer(ctx, []launcherHeldChild{{
		role: "Codex TUI", command: command, primary: true,
	}}, func(context.Context) (launcherHeldIdentity, error) {
		if launch.Confirm == nil {
			return launcherHeldIdentity{}, errors.New("Codex native identity confirmation is unavailable")
		}
		confirmed, err := launch.Confirm()
		if err != nil {
			return launcherHeldIdentity{}, err
		}
		report := livepresence.Report{
			UUID: confirmed.ThreadID, Name: confirmed.Name, Product: connectorProductCodex,
			Groups: append([]string(nil), launch.Groups...), Info: livepresence.CwdInfo(confirmed.Cwd),
		}
		return launcherHeldIdentity{report: report, call: func(
			callCtx context.Context, method string, params json.RawMessage,
		) (json.RawMessage, error) {
			return connectorNativeCall(callCtx, report, method, params)
		}}, nil
	})
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
					_, _ = runtime.Attachments().Detach(context.Background(), id, "native-owner-exited")
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
	native, err := c.codexNative()
	if err != nil {
		return daemonpkg.NativeEvidence{}, err
	}
	appPID, appStart, socket, err := native.RefreshAppServerEvidence(ctx)
	if err != nil {
		return daemonpkg.NativeEvidence{}, err
	}
	appServer, err := procinfo.CaptureIdentity(appPID)
	if err != nil || appStart == "" || appServer.Start != appStart {
		return daemonpkg.NativeEvidence{}, errors.New("codex App Server identity changed during refresh")
	}
	if _, err := native.ReattachThread(ctx, attachment.NativeSessionID); err != nil {
		return daemonpkg.NativeEvidence{}, fmt.Errorf("reattach Codex App Server thread: %w", err)
	}
	evidence := attachment.Evidence
	evidence.Ancestry = []procinfo.Identity{appServer}
	evidence.SocketPath = socket
	return evidence, nil
}

func (c *hostCoordinator) codexNative() (*bridge.CodexNative, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codex != nil {
		return c.codex, nil
	}
	native, err := c.openCodex(c.ctx, bridge.CodexNativeConfig{
		CodexBinary: codexBinary(), CodexHome: codexHome(), OnEvent: c.observeCodexNativeEvent,
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
	if strings.TrimSpace(event.ThreadID) == "" {
		return
	}
	if event.Kind == "thread/name/updated" && strings.TrimSpace(event.Name) != "" {
		c.mu.Lock()
		report, live := c.liveReports[event.ThreadID]
		runtime := c.runtime
		if live && report.Product == codexproduct.ProductID {
			report.Name = event.Name
			c.liveReports[event.ThreadID] = report
		}
		c.mu.Unlock()
		if live && runtime != nil {
			c.syncLiveSessions(runtime)
		}
	}
	if event.Kind != "thread/started" && event.Kind != "thread/status/changed" {
		return
	}
	observation := codexLaunchObservation{event: event}
	if event.Kind == "thread/started" {
		observation.threadUnixMilli, observation.threadIDValidation = codexUUIDv7UnixMilli(event.ThreadID)
		c.mu.Lock()
		// Capture eligibility at observation time so asynchronous resolution cannot
		// bind this event to a fresh record registered afterward.
		observation.freshEligible = make(map[*pendingCodexLaunch]struct{})
		for _, record := range c.pendingCodexLaunches {
			if record.request.SelectorKind == launcher.CodexLaunchSelectorFresh {
				observation.freshEligible[record] = struct{}{}
			}
		}
		c.mu.Unlock()
	}
	go c.matchPendingCodexLaunch(observation)
}

func (c *hostCoordinator) matchPendingCodexLaunch(observation codexLaunchObservation) {
	threadID := observation.event.ThreadID
	if observation.threadIDValidation != nil {
		if !c.failInvalidFreshCodexPendingLaunch(observation) {
			return
		}
	}
	c.mu.Lock()
	runtime := c.runtime
	hasPending := len(c.pendingCodexLaunches) != 0
	c.mu.Unlock()
	if runtime == nil || !hasPending {
		return
	}
	if _, active, err := runtime.Attachments().ActiveAttachment(threadID); err != nil {
		if !c.failExactCodexPendingLaunch(threadID, err) {
			fmt.Fprintf(os.Stderr, "Codex launch candidate %s attachment check failed: %v\n", threadID, err)
		}
		return
	} else if active {
		c.failExactCodexPendingLaunch(threadID, fmt.Errorf("Codex thread %s is already attached", threadID))
		return
	}
	native, err := c.codexNative()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Codex launch candidate %s native client failed: %v\n", threadID, err)
		return
	}
	thread, err := native.ResolveThread(c.ctx, threadID)
	if err != nil {
		if !c.failExactCodexPendingLaunch(threadID, err) {
			fmt.Fprintf(os.Stderr, "Codex launch candidate %s exact read failed: %v\n", threadID, err)
		}
		return
	}
	c.mu.Lock()
	matches := make([]*pendingCodexLaunch, 0, 1)
	for token, record := range c.pendingCodexLaunches {
		if pendingCodexLaunchMatches(record, observation, thread) {
			matches = append(matches, record)
			delete(c.pendingCodexLaunches, token)
		}
	}
	c.mu.Unlock()
	if len(matches) == 0 {
		return
	}
	if len(matches) > 1 {
		result := pendingCodexLaunchResult{err: fmt.Errorf("ambiguous concurrent Codex launches matched thread %s", thread.ID)}
		for _, record := range matches {
			record.done <- result
		}
		return
	}
	record := matches[0]
	result := pendingCodexLaunchResult{}
	result.handoff, result.err = c.adoptCodexPeer(record.ctx, runtime, record.request, native, thread)
	record.done <- result
}

func (c *hostCoordinator) failInvalidFreshCodexPendingLaunch(observation codexLaunchObservation) bool {
	c.mu.Lock()
	var failed *pendingCodexLaunch
	resumePending := false
	for token, record := range c.pendingCodexLaunches {
		if record.request.SelectorKind != launcher.CodexLaunchSelectorFresh {
			resumePending = true
			continue
		}
		_, eligible := observation.freshEligible[record]
		if failed == nil && eligible && record.request.Cwd == observation.event.Cwd {
			failed = record
			delete(c.pendingCodexLaunches, token)
		}
	}
	c.mu.Unlock()
	if failed != nil {
		failed.done <- pendingCodexLaunchResult{err: observation.threadIDValidation}
	}
	return resumePending
}

func (c *hostCoordinator) failExactCodexPendingLaunch(threadID string, failure error) bool {
	c.mu.Lock()
	var failed *pendingCodexLaunch
	for token, record := range c.pendingCodexLaunches {
		if record.request.SelectorKind == launcher.CodexLaunchSelectorID && record.request.Selector == threadID {
			failed = record
			delete(c.pendingCodexLaunches, token)
			break
		}
	}
	c.mu.Unlock()
	if failed == nil {
		return false
	}
	failed.done <- pendingCodexLaunchResult{err: failure}
	return true
}

type codexPendingControlCall struct{ call *daemonpkg.ControlCall }

func beginCodexPendingLaunch(
	ctx context.Context,
	request launcher.CodexDaemonPrepareRequest,
) (launcher.CodexPendingLaunch, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	endpoint, err := daemonpkg.ControlEndpoint(defaultStateRoot())
	if err != nil {
		return nil, err
	}
	requestID := commandRequestID()
	call, err := daemonpkg.BeginControlCall(ctx, endpoint, daemonpkg.ControlRequest{
		ID: requestID, Role: daemonpkg.RoleLauncher, Operation: "attachment.codex.pending",
		Generation: 1, IdempotencyKey: requestID, Payload: payload, WaitAdmission: true,
	})
	if err != nil {
		return nil, err
	}
	return &codexPendingControlCall{call: call}, nil
}

func (c *codexPendingControlCall) Await() (launcher.CodexDaemonPrepareResult, error) {
	var result launcher.CodexDaemonPrepareResult
	response, err := c.call.Await()
	if err != nil {
		return result, err
	}
	if response.Error != nil {
		return result, errors.New(response.Error.Message)
	}
	if json.Unmarshal(response.Payload, &result) != nil {
		return result, errors.New("daemon returned an invalid Codex native handoff")
	}
	return result, nil
}

func (c *codexPendingControlCall) Close() error { return c.call.Close() }

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
