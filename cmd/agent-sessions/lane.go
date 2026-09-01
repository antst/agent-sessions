package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const laneOutputLimit = 16 << 20
const defaultUnifiedLaneAutoArchiveDelay = time.Minute

type codexLaneNative interface {
	StartThread(context.Context, bridge.CodexStartRequest) (bridge.CodexNativeThread, error)
	PrepareLaneThread(context.Context, string, string, string, string, bool) (bridge.CodexNativeThread, error)
	StartLaneTurn(context.Context, bridge.CodexLaneTurnRequest) (string, error)
	WaitLaneTurn(context.Context, string, string) (bridge.CodexLaneTurnResult, error)
	InterruptLaneTurn(context.Context, string, string) error
	ArchiveThread(context.Context, string) error
}

func newCodexLaneAdapter(native codexLaneNative) (*daemonpkg.CodexLaneAdapter, error) {
	if native == nil {
		return nil, errors.New("codex App Server lane backend is unavailable")
	}
	return daemonpkg.NewCodexLaneAdapter(daemonpkg.CodexLaneAdapterConfig{
		Start: func(ctx context.Context, request daemonpkg.CodexLaneRequest) (daemonpkg.CodexLaneSession, error) {
			thread, err := native.StartThread(ctx, bridge.CodexStartRequest{
				Cwd: request.Cwd, Name: request.Name, NameSource: "lane",
				ApprovalPolicy: request.ApprovalPolicy, Sandbox: request.Sandbox,
			})
			return daemonpkg.CodexLaneSession{ID: thread.ID, Cwd: thread.Cwd}, err
		},
		Resume: func(ctx context.Context, request daemonpkg.CodexLaneRequest) (daemonpkg.CodexLaneSession, error) {
			thread, err := native.PrepareLaneThread(
				ctx, request.NativeSession, request.Cwd, request.ApprovalPolicy, request.Sandbox, request.Unarchive,
			)
			return daemonpkg.CodexLaneSession{ID: thread.ID, Cwd: thread.Cwd}, err
		},
		StartTurn: func(ctx context.Context, prompt daemonpkg.CodexLanePrompt) (string, error) {
			return native.StartLaneTurn(ctx, bridge.CodexLaneTurnRequest{
				ThreadID: prompt.ThreadID, Prompt: prompt.Prompt, Effort: prompt.Effort,
				ApprovalPolicy: prompt.ApprovalPolicy, Sandbox: prompt.Sandbox,
				SchemaPath: prompt.SchemaPath, Arguments: append([]string(nil), prompt.Arguments...),
			})
		},
		Wait: func(ctx context.Context, threadID, turnID string) (daemonpkg.CodexLaneTerminal, error) {
			result, err := native.WaitLaneTurn(ctx, threadID, turnID)
			return daemonpkg.CodexLaneTerminal{
				ThreadID: result.ThreadID, TurnID: result.TurnID, Outcome: result.Outcome, Result: result.Result,
			}, err
		},
		Interrupt: native.InterruptLaneTurn,
		Archive:   native.ArchiveThread,
	})
}

type laneActor struct {
	id, product, name, cwd, parentID, nativeID string
	nativeTurnID                               string
	capability                                 string
	approvalPolicy, sandbox, effort, schema    string
	groups, arguments                          []string
	explicitGroups                             []string
	inheritGroups                              bool
	permission                                 string
	persistent                                 bool
	autoArchive                                bool
	autoArchiveDelay                           time.Duration
	autoArchiveAt                              int64
	turnTimeout                                time.Duration
	turnID                                     string
	state, outcome, result, failure            string
	startedAt, completedAt, deadlineAt         int64
	cancel                                     context.CancelFunc
	done                                       chan struct{}
	collecting                                 bool
	interruptRequested                         bool
}

type laneCommandEnvelope struct {
	Product            string   `json:"product"`
	Arguments          []string `json:"arguments"`
	Input              string   `json:"input"`
	Cwd                string   `json:"cwd,omitempty"`
	SourceAttachmentID string   `json:"source_attachment_id"`
	Command            string   `json:"command,omitempty"`
	Host               string   `json:"host,omitempty"`
}

type parsedLaneCommand struct {
	command, target, name, cwd, timeout, permission string
	approvalPolicy, sandbox, effort, schema         string
	groups                                          []string
	permissionExplicit                              bool
	inheritGroups, noInheritGroups                  bool
	persistent, persistentSet, all, mine            bool
	noAutoArchive, autoArchiveAfterSet              bool
	autoArchiveAfter                                time.Duration
	native                                          []string
}

func (c *hostCoordinator) handleLaneCommand(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request daemonpkg.ControlRequest,
) (json.RawMessage, error) {
	var envelope laneCommandEnvelope
	if json.Unmarshal(request.Payload, &envelope) != nil {
		return nil, errors.New("decode lane command failed")
	}
	if envelope.Command != "" {
		envelope.Arguments = append([]string{envelope.Command}, envelope.Arguments...)
	}
	parsed, err := parseUnifiedLaneCommand(envelope.Arguments)
	if err != nil {
		return nil, err
	}
	// Readiness inspection is intentionally available outside a managed peer.
	// The legacy lane wrappers used doctor during preflight, before an
	// attachment existed, and it is a read-only product inventory operation.
	if parsed.command == "doctor" && strings.TrimSpace(envelope.Host) == "" {
		result, err := doctorLane(ctx, envelope.Product, envelope.Cwd)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	sourceID := strings.TrimSpace(envelope.SourceAttachmentID)
	if sourceID == "" {
		sourceID = strings.TrimSpace(request.AttachmentID)
	}
	parent, ok, err := c.activeLocalParent(runtime, sourceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("lane command requires one live attested parent")
	}
	if err := c.ensureLaneActors(runtime); err != nil {
		return nil, err
	}
	if strings.TrimSpace(envelope.Host) != "" {
		return c.handleRemoteLaneCommand(ctx, runtime, parent, envelope, request.IdempotencyKey)
	}
	return c.dispatchLaneCommand(ctx, runtime, parent, envelope.Product, parsed, envelope.Input)
}

func (c *hostCoordinator) dispatchLaneCommand(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	product string,
	parsed parsedLaneCommand,
	input string,
) (json.RawMessage, error) {
	var err error
	var result map[string]any
	switch parsed.command {
	case "run", "start":
		result, err = c.startLane(ctx, runtime, parent, product, parsed, input, parsed.command == "run")
	case "resume":
		result, err = c.resumeLane(ctx, runtime, parent, product, parsed, input)
	case "wait":
		result, err = c.waitLane(ctx, runtime, parent, product, parsed)
	case "status":
		result, err = c.statusLane(runtime, parent, product, parsed)
	case "list":
		result, err = c.listLanes(runtime, parent, product, parsed)
	case "interrupt":
		result, err = c.interruptLane(runtime, parent, product, parsed)
	case "archive":
		result, err = c.archiveLane(runtime, parent, product, parsed)
	case "doctor":
		result, err = doctorLane(ctx, product, parent.Cwd)
	default:
		err = fmt.Errorf("unsupported lane command %q", parsed.command)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func parseUnifiedLaneCommand(arguments []string) (parsedLaneCommand, error) { //nolint:gocyclo // The wrapper owns one deliberately small cross-product option layer.
	if len(arguments) == 0 {
		return parsedLaneCommand{}, errors.New("lane command is required")
	}
	result := parsedLaneCommand{command: arguments[0]}
	remaining := arguments[1:]
	for len(remaining) > 0 {
		argument := remaining[0]
		remaining = remaining[1:]
		value := func(name string) (string, error) {
			if len(remaining) == 0 || strings.TrimSpace(remaining[0]) == "" {
				return "", fmt.Errorf("%s requires a value", name)
			}
			v := remaining[0]
			remaining = remaining[1:]
			return v, nil
		}
		switch argument {
		case "-n", "--name", "--peer-name":
			var err error
			result.name, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "-C", "--cd", "--cwd":
			var err error
			result.cwd, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "-g", "--group":
			group, err := value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
			result.groups = append(result.groups, group)
		case "--timeout":
			var err error
			result.timeout, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "--permission-mode", "--approval-mode":
			var err error
			result.permission, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
			result.permissionExplicit = true
		case "--approval-policy":
			var err error
			result.approvalPolicy, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
			result.permissionExplicit = true
		case "--sandbox":
			var err error
			result.sandbox, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "--effort", "--reasoning-effort":
			var err error
			result.effort, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "--schema":
			var err error
			result.schema, err = value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
		case "--inherit-groups":
			result.inheritGroups = true
		case "--no-inherit-groups":
			result.noInheritGroups = true
		case "--persistent":
			result.persistent = true
			result.persistentSet = true
		case "--no-auto-archive":
			result.noAutoArchive = true
		case "--auto-archive-after":
			text, err := value(argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
			result.autoArchiveAfter, err = parseLaneSeconds(text, true, argument)
			if err != nil {
				return parsedLaneCommand{}, err
			}
			result.autoArchiveAfterSet = true
		case "--all":
			result.all = true
		case "--mine":
			result.mine = true
		case "--json", "-":
		case "--notify":
			return parsedLaneCommand{}, errors.New("unified daemon lanes notify their immediate parent automatically; --notify is not supported")
		case "--no-notify":
			return parsedLaneCommand{}, errors.New("unified daemon lanes notify their immediate parent automatically; --no-notify is not supported")
		case "--yolo", "--always-approve", "--dangerously-bypass-approvals-and-sandbox":
			result.permission = "bypassPermissions"
			result.permissionExplicit = true
		case "--no-yolo":
			result.permission = "default"
			result.permissionExplicit = true
		default:
			switch {
			case strings.HasPrefix(argument, "-"):
				result.native = append(result.native, argument)
				if laneNativeOptionTakesValue(argument) {
					v, err := value(argument)
					if err != nil {
						return parsedLaneCommand{}, err
					}
					result.native = append(result.native, v)
				}
			case result.target == "":
				result.target = argument
			default:
				return parsedLaneCommand{}, errors.New("lane command accepts only one selector")
			}
		}
	}
	if result.inheritGroups && result.noInheritGroups {
		return parsedLaneCommand{}, errors.New("--inherit-groups and --no-inherit-groups are mutually exclusive")
	}
	if result.noAutoArchive && result.autoArchiveAfterSet {
		return parsedLaneCommand{}, errors.New("--auto-archive-after and --no-auto-archive are mutually exclusive")
	}
	// Codex approval policy is the explicit native authorization contract. It
	// must take precedence over both an inherited parent mode and any generic
	// permission flag, independent of option order.
	if result.approvalPolicy != "" {
		result.permission = lanePermissionForApprovalPolicy(result.approvalPolicy)
	}
	return result, nil
}

func lanePermissionForApprovalPolicy(approvalPolicy string) string {
	if approvalPolicy == "never" {
		return "bypassPermissions"
	}
	return "default"
}

func parseLaneSeconds(value string, positive bool, option string) (time.Duration, error) {
	seconds, err := time.ParseDuration(strings.TrimSpace(value) + "s")
	if err != nil || seconds < 0 || positive && seconds <= 0 {
		return 0, fmt.Errorf("%s requires %s seconds", option, map[bool]string{true: "positive", false: "non-negative"}[positive])
	}
	return seconds, nil
}

func laneNativeOptionTakesValue(argument string) bool {
	if strings.Contains(argument, "=") {
		return false
	}
	for _, candidate := range []string{"-m", "--model", "--effort", "--reasoning-effort", "--sandbox", "--approval-policy", "--config", "-c", "--schema", "--max-budget-usd", "--tools", "--allowed-tools", "--disallowed-tools", "--qwen-home"} {
		if argument == candidate {
			return true
		}
	}
	return false
}

//nolint:gocyclo // Start is one durable authorization, native dispatch, publication, and rollback transaction.
func (c *hostCoordinator) startLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand, input string, wait bool) (map[string]any, error) {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(options.name) == "" {
		return nil, errors.New("lane start/run requires --name and non-empty input")
	}
	if err := validateGrokLanePermission(product, options, true); err != nil {
		return nil, err
	}
	cwd := options.cwd
	if cwd == "" {
		cwd = parent.Cwd
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(parent.Cwd, cwd)
	}
	cwd, err := pathidentity.ExistingDirectory(filepath.Clean(cwd))
	if err != nil {
		return nil, errors.New("lane cwd is unavailable")
	}
	nativePath, err := laneExecutable(product)
	if err != nil {
		return nil, err
	}
	readiness := inspectLaneProductReadiness(ctx, product, nativePath, cwd)
	if ready, _ := readiness["ready"].(bool); !ready {
		return nil, fmt.Errorf("%s lane readiness is not established: %v", product, readiness["readiness_error"])
	}
	explicitGroups := uniqueStrings(options.groups)
	groups := append([]string(nil), explicitGroups...)
	if options.inheritGroups {
		groups = uniqueStrings(append(groups, parent.Groups...))
	}
	permission := options.permission
	if permission == "" {
		permission = laneDefaultPermission(product, parent.PermissionMode)
	}
	id, err := newLaneUUID()
	if err != nil {
		return nil, err
	}
	groups, err = c.anchorLaneGroups(runtime, groups, parent.ID, id)
	if err != nil {
		return nil, err
	}
	actor := &laneActor{
		id: id, product: product, name: options.name, cwd: cwd, parentID: parent.ID,
		groups: groups, explicitGroups: explicitGroups, inheritGroups: options.inheritGroups,
		permission: permission, persistent: options.persistent,
		autoArchive: !options.noAutoArchive, autoArchiveDelay: defaultUnifiedLaneAutoArchiveDelay,
		approvalPolicy: options.approvalPolicy, sandbox: options.sandbox,
		effort: options.effort, schema: options.schema,
		arguments: append([]string(nil), options.native...), done: make(chan struct{}), state: "preparing",
	}
	if options.autoArchiveAfterSet {
		actor.autoArchive, actor.autoArchiveDelay = true, options.autoArchiveAfter
	}
	if options.timeout != "" {
		actor.turnTimeout, err = parseLaneSeconds(options.timeout, false, "--timeout")
		if err != nil {
			return nil, err
		}
	}
	if product == "claude" {
		actor.nativeID = id
	}
	c.mu.Lock()
	if conflict := c.liveLaneNameLocked(runtime, parent, options.name); conflict {
		c.mu.Unlock()
		return nil, fmt.Errorf("visible lane name %q is already live", options.name)
	}
	c.lanes[id] = actor
	c.mu.Unlock()
	turnID := commandRequestID()
	actor.turnID = turnID
	if err := c.commitNewLane(runtime, actor); err != nil {
		c.mu.Lock()
		delete(c.lanes, id)
		c.mu.Unlock()
		return nil, err
	}
	if err := c.dispatchLaneTurn(runtime, actor, input, false, false); err != nil {
		return nil, err
	}
	if !wait {
		return laneReadyResult(actor), nil
	}
	return c.waitLaneActor(ctx, runtime, actor)
}

//nolint:gocyclo // Resume revalidates ownership, native identity, permissions, and dispatch atomically.
func (c *hostCoordinator) resumeLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand, input string) (map[string]any, error) {
	if strings.TrimSpace(options.target) == "" || strings.TrimSpace(input) == "" {
		return nil, errors.New("lane resume requires one selector and non-empty input")
	}
	if err := validateGrokLanePermission(product, options, false); err != nil {
		return nil, err
	}
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if actor.state == "running" {
		c.mu.Unlock()
		return nil, errors.New("collect or interrupt the active lane turn before resume")
	}
	unarchive := actor.state == "archived"
	prepareLaneTurnLocked(actor)
	actor.parentID = parent.ID
	if len(options.groups) > 0 {
		actor.explicitGroups = uniqueStrings(options.groups)
	}
	if options.inheritGroups {
		actor.inheritGroups = true
	}
	if options.noInheritGroups {
		actor.inheritGroups = false
	}
	if options.approvalPolicy != "" {
		actor.approvalPolicy = options.approvalPolicy
	}
	if options.sandbox != "" {
		actor.sandbox = options.sandbox
	}
	if options.effort != "" {
		actor.effort = options.effort
	}
	if options.schema != "" {
		actor.schema = options.schema
	}
	if len(options.native) > 0 {
		actor.arguments = append([]string(nil), options.native...)
	}
	if options.permission != "" {
		actor.permission = options.permission
	}
	if options.persistentSet {
		actor.persistent = options.persistent
	}
	if options.autoArchiveAfterSet {
		actor.autoArchive, actor.autoArchiveDelay = true, options.autoArchiveAfter
	}
	if options.noAutoArchive {
		actor.autoArchive, actor.autoArchiveAt = false, 0
	}
	actor.turnTimeout = 0
	if options.timeout != "" {
		actor.turnTimeout, err = parseLaneSeconds(options.timeout, false, "--timeout")
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	actor.groups, err = c.effectiveLaneGroups(runtime, actor, parent)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	if err := c.commitResumeLane(runtime, actor, false); err != nil {
		return nil, err
	}
	if err := c.dispatchLaneTurn(runtime, actor, input, true, unarchive); err != nil {
		return nil, err
	}
	return c.waitLaneActor(ctx, runtime, actor)
}

func (c *hostCoordinator) waitLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	waitCtx := ctx
	if options.timeout != "" && options.timeout != "0" {
		duration, parseErr := parseLaneSeconds(options.timeout, false, "lane --timeout")
		if parseErr != nil {
			return nil, parseErr
		}
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	return c.waitLaneActor(waitCtx, runtime, actor)
}

func (c *hostCoordinator) waitLaneActor(ctx context.Context, runtime *daemonpkg.Runtime, actor *laneActor) (map[string]any, error) {
	c.mu.Lock()
	if actor.collecting {
		c.mu.Unlock()
		return nil, errors.New("lane already has an active collector")
	}
	actor.collecting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		actor.collecting = false
		c.mu.Unlock()
	}()
	c.mu.Lock()
	state, done := actor.state, actor.done
	if state == "idle" || state == "archived" || done == nil {
		c.mu.Unlock()
		return nil, errors.New("lane has no live turn result")
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	c.mu.Lock()
	result := laneActorResult(actor)
	if actor.state == "terminal" {
		actor.state = "idle"
	}
	c.mu.Unlock()
	c.armLaneAutoArchive(runtime, actor)
	return result, nil
}

func (c *hostCoordinator) statusLane(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, options.all)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	result := laneActorStatus(actor)
	c.mu.Unlock()
	return result, nil
}

func (c *hostCoordinator) listLanes(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	lanes := make([]map[string]any, 0)
	for _, actor := range c.lanes {
		if actor.product != product || options.mine && actor.parentID != parent.ID || !options.all && actor.state == "archived" || !groupsIntersect(parentGroups, actor.groups) && actor.parentID != parent.ID {
			continue
		}
		lanes = append(lanes, laneActorStatus(actor))
	}
	c.mu.Unlock()
	sort.Slice(lanes, func(i, j int) bool { return fmt.Sprint(lanes[i]["name"]) < fmt.Sprint(lanes[j]["name"]) })
	return map[string]any{"type": "lane.list", "product": product, "lanes": lanes}, nil
}

func (c *hostCoordinator) interruptLane(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cancel := actor.cancel
	running := actor.state == "running"
	actorProduct, nativeThreadID, nativeTurnID := actor.product, actor.nativeID, actor.nativeTurnID
	if running && cancel != nil {
		actor.interruptRequested = true
		actor.state = "interrupting"
	}
	c.mu.Unlock()
	if !running || cancel == nil {
		return nil, errors.New("lane has no active turn")
	}
	if err := c.commitLaneState(runtime, actor.id, "interrupting"); err != nil {
		return nil, err
	}
	if err := c.interruptLaneNative(actorProduct, actor.id, nativeThreadID, nativeTurnID); err != nil {
		return nil, err
	}
	cancel()
	return map[string]any{"type": "turn.interrupting", "thread_id": actor.id, "turn_id": actor.turnID}, nil
}

func (c *hostCoordinator) interruptLaneNative(product, laneID, nativeThreadID, nativeTurnID string) error {
	switch product {
	case "codex":
		native, nativeErr := c.codexNative()
		if nativeErr != nil {
			return nativeErr
		}
		adapter, adapterErr := newCodexLaneAdapter(native)
		if adapterErr != nil {
			return adapterErr
		}
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer interruptCancel()
		nativeErr = adapter.Interrupt(interruptCtx, nativeThreadID, nativeTurnID)
		return nativeErr
	case "claude":
		return c.claudeLanes.Interrupt(laneID)
	case "grok":
		return c.grokLanes.Interrupt(laneID)
	case "qwen":
		return c.qwenLanes.Interrupt(laneID)
	default:
		return fmt.Errorf("unsupported lane product %q", product)
	}
}

func (c *hostCoordinator) archiveLane(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if actor.state == "archived" {
		result := map[string]any{
			"type": "lane.archived", "product": product, "thread_id": actor.id,
			"session_id": actor.nativeID, "name": actor.name, "already_archived": true,
		}
		c.mu.Unlock()
		return result, nil
	}
	if actor.state == "running" {
		c.mu.Unlock()
		return nil, errors.New("refuse to archive a lane with an active turn")
	}
	actor.state = "archived"
	c.mu.Unlock()
	if err := c.commitLaneState(runtime, actor.id, "archived"); err != nil {
		return nil, err
	}
	if err := c.archiveNativeLane(actor); err != nil {
		return nil, err
	}
	if err := c.retireParentLanes(runtime, actor.id); err != nil {
		return nil, err
	}
	return map[string]any{"type": "lane.archived", "product": product, "thread_id": actor.id, "session_id": actor.nativeID, "name": actor.name}, nil
}

func (c *hostCoordinator) deliverLaneMessage(runtime *daemonpkg.Runtime, actor *laneActor, message string) error {
	message = laneInboundPrompt(actor.product, message)
	c.mu.Lock()
	switch actor.state {
	case "archived", "retiring":
		c.mu.Unlock()
		return errors.New("lane has no live message recipient")
	case "running", "preparing", "transitioning", "interrupting":
		c.mu.Unlock()
		return errors.New("lane product is busy and did not accept the message")
	}
	prepareLaneTurnLocked(actor)
	c.mu.Unlock()
	if err := c.commitResumeLane(runtime, actor, false); err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	return c.dispatchLaneTurn(runtime, actor, message, true, false)
}

// Claude Code treats a top-level <cross-session-message> argument as native
// asynchronous input. In one-shot --print mode that consumes the argument
// without opening a query and exits with "No messages returned from query".
// The legacy Claude lane used a long-lived stream-json worker, where the same
// frame arrived inside an ordinary user envelope. Preserve that established
// behavior while the unified daemon owns turn serialization by putting the
// peer frame inside an explicit user prompt before invoking Claude.
func laneInboundPrompt(product, message string) string {
	if product != "claude" || !strings.HasPrefix(strings.TrimSpace(message), "<cross-session-message ") {
		return message
	}
	return "The following Agent Sessions peer message is the current user turn. " +
		"Act on its enclosed content and preserve its sender metadata.\n\n" + message
}

func (c *hostCoordinator) reconcileOrphanedLanes(runtime *daemonpkg.Runtime) error {
	if err := c.ensureLaneActors(runtime); err != nil {
		return err
	}
	c.mu.Lock()
	candidates := make([]*laneActor, 0)
	for _, actor := range c.lanes {
		if !actor.persistent && actor.state != "archived" {
			candidates = append(candidates, actor)
		}
	}
	c.mu.Unlock()
	orphanedParents := map[string]bool{}
	for _, actor := range candidates {
		live, err := c.localParentIsLive(runtime, actor.parentID)
		if err != nil {
			return err
		}
		if live {
			continue
		}
		orphanedParents[actor.parentID] = true
	}
	for parentID := range orphanedParents {
		if err := c.retireParentLanes(runtime, parentID); err != nil {
			return err
		}
	}
	return nil
}

func (c *hostCoordinator) archiveIdleLanesForParent(runtime *daemonpkg.Runtime, parentID string) {
	_ = c.retireParentLanes(runtime, parentID)
}

//nolint:gocyclo // Parent retirement handles each durable lane lifecycle state explicitly.
func (c *hostCoordinator) retireParentLanes(runtime *daemonpkg.Runtime, parentID string) error {
	type transition struct {
		actor                        *laneActor
		state, product, thread, turn string
		cancel                       context.CancelFunc
	}
	c.mu.Lock()
	candidates := make([]transition, 0)
	for _, actor := range c.lanes {
		if actor.parentID != parentID || actor.persistent || actor.state == "archived" || actor.state == "retiring" {
			continue
		}
		state := "archived"
		var cancel context.CancelFunc
		if (actor.state == "running" || actor.state == "preparing" || actor.state == "interrupting") && actor.cancel != nil {
			state, cancel = "retiring", actor.cancel
			actor.interruptRequested = true
		}
		actor.state = state
		candidates = append(candidates, transition{
			actor: actor, state: state, product: actor.product,
			thread: actor.nativeID, turn: actor.nativeTurnID, cancel: cancel,
		})
	}
	c.mu.Unlock()
	for _, candidate := range candidates {
		if err := c.commitLaneState(runtime, candidate.actor.id, candidate.state); err != nil {
			return err
		}
		if candidate.state == "archived" {
			if err := c.archiveNativeLane(candidate.actor); err != nil {
				return err
			}
		}
		if candidate.cancel != nil {
			if candidate.product == "codex" {
				native, err := c.codexNative()
				if err != nil {
					return err
				}
				interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
				err = native.InterruptLaneTurn(interruptCtx, candidate.thread, candidate.turn)
				interruptCancel()
				if err != nil {
					return err
				}
			}
			candidate.cancel()
		}
	}
	return nil
}

func doctorLane(ctx context.Context, product, cwd string) (map[string]any, error) {
	path, err := laneExecutable(product)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cwd) == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve lane doctor cwd: %w", err)
		}
	}
	cwd, err = pathidentity.ExistingDirectory(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve lane doctor cwd: %w", err)
	}
	return inspectLaneProductReadiness(ctx, product, path, cwd), nil
}

func inspectLaneProductReadiness(ctx context.Context, product, path, cwd string) map[string]any {
	report := map[string]any{
		"type": "lane.doctor", "contract_version": 2, "authority": "daemon",
		"product": product, "ready": true, "native_path": path, "runtime_path": path,
		"daemon_reachable": true, "supervisor_reachable": true,
	}
	if product == "qwen" {
		return inspectQwenLaneProductReadiness(ctx, report, path, cwd)
	}
	version, versionErr := inspectLaneNativeVersion(ctx, product, path)
	report[product+"_available"] = versionErr == nil
	report[product+"_path"] = path
	report[product+"_version"] = version
	if versionErr != nil {
		report[product+"_error"] = versionErr.Error()
		report["ready"] = false
		report["readiness_error"] = versionErr.Error()
	}
	if product != "claude" {
		return report
	}
	status, err := bridge.InspectClaudeLaneReadiness(path)
	report["claude_logged_in"] = status.LoggedIn
	report["claude_auth_method"] = status.AuthMethod
	report["claude_api_provider"] = status.APIProvider
	switch {
	case err != nil:
		report["ready"] = false
		report["readiness_error"] = err.Error()
	case !status.LoggedIn:
		report["ready"] = false
		report["readiness_error"] = "Claude Code is not authenticated"
	}
	return report
}

func inspectLaneNativeVersion(ctx context.Context, product, path string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	arguments := []string{"--version"}
	if product == "grok" {
		arguments = []string{"--no-auto-update", "--version"}
	}
	body, err := exec.CommandContext(probeCtx, path, arguments...).CombinedOutput()
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s version check timed out", product)
	}
	if err != nil {
		return "", fmt.Errorf("%s version check failed: %w", product, err)
	}
	version := strings.TrimSpace(string(body))
	if version == "" {
		return "", fmt.Errorf("%s version check returned no identity", product)
	}
	return version, nil
}

func inspectQwenLaneProductReadiness(ctx context.Context, base map[string]any, path, cwd string) map[string]any {
	profile, err := qwenprofile.Current()
	if err != nil {
		base["ready"], base["qwen_available"], base["readiness_error"] = false, false, err.Error()
		return base
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := qwenreadiness.Check(probeCtx, qwenreadiness.Request{
		Executable: path, Workspace: cwd, Profile: profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(os.Environ()),
	})
	if err != nil {
		base["ready"], base["qwen_available"], base["readiness_error"] = false, false, err.Error()
		return base
	}
	for key, value := range qwenLaneDoctorProjection(result) {
		base[key] = value
	}
	return base
}

func qwenLaneDoctorProjection(report qwenreadiness.Report) map[string]any {
	interactive := qwenreadiness.StateReady
	for _, contract := range []qwenreadiness.ParserContract{
		qwenreadiness.ParserDualOutput, qwenreadiness.ParserNativeDefault, qwenreadiness.ParserDefault,
		qwenreadiness.ParserYolo, qwenreadiness.ParserPlan,
	} {
		if report.ParserContracts[contract] != qwenreadiness.StateReady {
			interactive = report.ParserContracts[contract]
			if interactive == "" {
				interactive = qwenreadiness.StateUnready
			}
			break
		}
	}
	projection := map[string]any{
		"ready": report.Ready, "ok": report.Ready, "qwen_available": report.ResolvedExecutable != "",
		"qwen_path": report.ResolvedExecutable, "qwen_version": report.Version,
		"minimum_version": report.MinimumVersion, "minimum_version_ok": report.MinimumVersionOK,
		"package_identity_ok": report.PackageIdentityOK, "profile": report.Profile,
		"integration": report.Integration, "integration_ready": report.IntegrationReady,
		"parser_contracts": report.ParserContracts, "auth_state": report.CredentialConfigurationState,
		"workspace_trust": report.WorkspaceTrust, "interactive_contract": interactive,
		"acp_contract": report.ACPContract, "archive_contract": report.ArchiveContract,
		"issues": report.Issues,
	}
	if !report.Ready {
		messages := make([]string, 0, len(report.Issues))
		for _, issue := range report.Issues {
			messages = append(messages, issue.Code+": "+issue.Message)
		}
		projection["readiness_error"] = strings.Join(messages, "; ")
	}
	return projection
}

//nolint:gocyclo // Dispatch keeps capability, journal, native execution, and failure transitions together.
func (c *hostCoordinator) dispatchLaneTurn(runtime *daemonpkg.Runtime, actor *laneActor, prompt string, resume, unarchive bool) error {
	capability, err := randomCapability()
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	actor.capability = capability
	c.mu.Unlock()
	if err := c.commitLaneAuthorization(runtime, actor, "preparing"); err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	actor.state = "preparing"
	c.mu.Unlock()
	if actor.product == "codex" {
		return c.dispatchCodexLaneTurn(runtime, actor, prompt, resume, unarchive)
	}
	if actor.product == "grok" || actor.product == "qwen" {
		return c.dispatchACPLaneTurn(runtime, actor, prompt)
	}
	command, err := laneNativeCommand(actor, prompt, resume)
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	turnCtx, cancel := laneTurnContext(c.ctx, actor.turnTimeout)
	command = exec.CommandContext(turnCtx, command.Path, command.Args[1:]...) //nolint:gosec // path and argv are constructed from the installed product inventory.
	command.Dir, command.Env = actor.cwd, laneWorkerEnvironment(os.Environ(), actor)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cappedLaneBuffer
	if actor.product == "codex" {
		stdout.onLine = func(line []byte) error {
			nativeID := parseCodexStartedThreadID(line)
			if nativeID == "" {
				return nil
			}
			return c.recordLaneNativeID(runtime, actor, nativeID)
		}
	}
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		cancel()
		return c.failLaneDispatch(runtime, actor, err)
	}
	if actor.product == "claude" {
		if err := c.claudeLanes.Register(actor.id, cancel); err != nil {
			cancel()
			_ = command.Wait()
			return c.failLaneDispatch(runtime, actor, err)
		}
	}
	c.mu.Lock()
	actor.cancel, actor.state, actor.startedAt = cancel, "running", time.Now().UnixMilli()
	if actor.turnTimeout > 0 {
		actor.deadlineAt = actor.startedAt + actor.turnTimeout.Milliseconds()
	}
	actor.interruptRequested = false
	c.mu.Unlock()
	if err := c.markLaneRunning(runtime, actor); err != nil {
		cancel()
		_ = command.Wait()
		if actor.product == "claude" {
			c.claudeLanes.Complete(actor.id)
		}
		return c.failLaneDispatch(runtime, actor, err)
	}
	go func() {
		c.mu.Lock()
		dispatchedTurnID, dispatchedDone := actor.turnID, actor.done
		c.mu.Unlock()
		err := command.Wait()
		if actor.product == "claude" {
			// A queued turn is dispatched synchronously by completeLaneTurn.
			// Release the exited per-turn worker before that dispatch registers
			// its successor, while retaining one-worker-at-a-time serialization.
			c.claudeLanes.Complete(actor.id)
		}
		outcome := "completed"
		if err != nil {
			outcome = "failed"
		}
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		result, nativeID := parseLaneNativeResult(actor.product, stdout.Bytes())
		if result == "" {
			result = strings.TrimSpace(stdout.String())
		}
		failure := ""
		if err != nil {
			failure = strings.TrimSpace(stderr.String())
		}
		cancel()
		c.completeLaneTurn(runtime, actor, dispatchedTurnID, dispatchedDone, laneTurnCompletion{
			outcome: outcome, result: result, failure: failure, nativeID: nativeID, failed: err != nil,
		})
	}()
	return nil
}

func (c *hostCoordinator) dispatchCodexLaneTurn(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	prompt string,
	resume, unarchive bool,
) error {
	native, err := c.codexNative()
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	return c.dispatchCodexLaneTurnWithNative(runtime, actor, prompt, resume, unarchive, native)
}

func (c *hostCoordinator) dispatchCodexLaneTurnWithNative(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	prompt string,
	resume, unarchive bool,
	native codexLaneNative,
) error {
	adapter, err := newCodexLaneAdapter(native)
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	thread, err := adapter.Prepare(c.ctx, daemonpkg.CodexLaneRequest{
		LaneID: actor.id, NativeSession: actor.nativeID, Cwd: actor.cwd, Name: actor.name,
		PermissionMode: actor.permission, ApprovalPolicy: actor.approvalPolicy, Sandbox: actor.sandbox,
		Resume: resume, Unarchive: unarchive,
	})
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	if err := c.recordLaneNativeID(runtime, actor, thread.ID); err != nil {
		if !resume {
			_ = adapter.Archive(context.Background(), thread.ID)
		}
		return c.failLaneDispatch(runtime, actor, err)
	}
	nativeTurnID, err := adapter.StartTurn(c.ctx, daemonpkg.CodexLanePrompt{
		ThreadID: thread.ID, Prompt: prompt, Effort: actor.effort,
		ApprovalPolicy: thread.ApprovalPolicy, Sandbox: thread.Sandbox,
		SchemaPath: actor.schema, Arguments: append([]string(nil), actor.arguments...),
	})
	if err != nil {
		if !resume {
			_ = adapter.Archive(context.Background(), thread.ID)
		}
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	actor.nativeTurnID = nativeTurnID
	c.mu.Unlock()
	turnCtx, cancel := laneTurnContext(c.ctx, actor.turnTimeout)
	if err := c.beginLaneExecution(runtime, actor, cancel); err != nil {
		cancel()
		if !resume {
			_ = adapter.Archive(context.Background(), thread.ID)
		}
		return c.failLaneDispatch(runtime, actor, err)
	}
	go c.watchCodexLaneTurn(runtime, actor, adapter, thread.ID, nativeTurnID, turnCtx, cancel)
	return nil
}

func (c *hostCoordinator) watchCodexLaneTurn(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	adapter *daemonpkg.CodexLaneAdapter,
	threadID, nativeTurnID string,
	turnCtx context.Context,
	cancel context.CancelFunc,
) {
	c.mu.Lock()
	dispatchedTurnID, dispatchedDone := actor.turnID, actor.done
	c.mu.Unlock()
	result, waitErr := adapter.Wait(turnCtx, threadID, nativeTurnID)
	if waitErr != nil && c.ctx.Err() != nil {
		// Service shutdown transfers ownership to the successor daemon. Do not
		// terminalize or interrupt the product-owned App Server turn here.
		cancel()
		return
	}
	outcome := result.Outcome
	if outcome == "" {
		outcome = "failed"
	}
	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		outcome = "timed_out"
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = adapter.Interrupt(interruptCtx, threadID, nativeTurnID)
		interruptCancel()
	}
	cancel()
	failure := ""
	if waitErr != nil && outcome != "interrupted" && outcome != "timed_out" {
		failure = waitErr.Error()
	}
	c.completeLaneTurn(runtime, actor, dispatchedTurnID, dispatchedDone, laneTurnCompletion{
		outcome: outcome, result: result.Result, failure: failure, failed: waitErr != nil, clearNativeTurn: true,
	})
}

func (c *hostCoordinator) dispatchACPLaneTurn(runtime *daemonpkg.Runtime, actor *laneActor, prompt string) error {
	executable, err := laneExecutable(actor.product)
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	hostExecutable, err := os.Executable()
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	product, cwd, laneID := actor.product, actor.cwd, actor.id
	permission, effort := actor.permission, actor.effort
	timeout := actor.turnTimeout
	c.mu.Unlock()
	lifecycleCtx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	capability := actor.capability
	nativeID := actor.nativeID
	c.mu.Unlock()
	type acpLaneEvent struct {
		ready  bool
		result bridge.NativeLaneACPResult
		err    error
	}
	events := make(chan acpLaneEvent, 2)
	runNative := func(runCtx context.Context, permissionMode, selectedNativeID string) (daemonpkg.NativeACPLaneResult, error) {
		result, runErr := bridge.RunNativeLaneACP(runCtx, bridge.NativeLaneACPConfig{
			Product: product, Executable: executable, HostExecutable: hostExecutable,
			Cwd: cwd, LaneID: laneID, Capability: capability,
			NativeSessionID: selectedNativeID, PermissionMode: permissionMode,
			Effort: effort, Prompt: prompt, Environment: laneWorkerEnvironment(os.Environ(), actor),
			ExecutionTimeout: timeout,
			SessionOpened:    func(opened string) error { return c.recordLaneNativeID(runtime, actor, opened) },
			ExecutionStarted: func() error {
				if err := c.beginLaneExecution(runtime, actor, cancel); err != nil {
					return err
				}
				events <- acpLaneEvent{ready: true}
				return nil
			},
		})
		return daemonpkg.NativeACPLaneResult{
			NativeSessionID: result.NativeSessionID, Output: result.Output, Mode: result.Mode,
		}, runErr
	}
	go func() {
		var (
			result daemonpkg.NativeACPLaneResult
			runErr error
		)
		if product == "grok" {
			result, runErr = c.grokLanes.Run(lifecycleCtx, daemonpkg.GrokACPLaneRequest{
				LaneID: laneID, NativeSession: nativeID, PermissionMode: permission, Prompt: prompt,
			}, func(runCtx context.Context, request daemonpkg.GrokACPLaneRequest) (daemonpkg.NativeACPLaneResult, error) {
				return runNative(runCtx, request.PermissionMode, request.NativeSession)
			})
		} else {
			result, runErr = c.qwenLanes.Run(lifecycleCtx, daemonpkg.QwenACPLaneRequest{
				LaneID: laneID, NativeSession: nativeID, PermissionMode: permission, Prompt: prompt,
			}, func(runCtx context.Context, request daemonpkg.QwenACPLaneRequest) (daemonpkg.NativeACPLaneResult, error) {
				return runNative(runCtx, request.PermissionMode, request.NativeSession)
			})
		}
		events <- acpLaneEvent{result: bridge.NativeLaneACPResult{
			NativeSessionID: result.NativeSessionID, Output: result.Output, Mode: result.Mode,
		}, err: runErr}
	}()
	first := <-events
	if !first.ready {
		cancel()
		if first.err == nil {
			first.err = errors.New("native ACP lane exited before its execution became ready")
		}
		return c.failLaneDispatch(runtime, actor, first.err)
	}
	go func() {
		terminal := <-events
		c.mu.Lock()
		dispatchedTurnID, dispatchedDone := actor.turnID, actor.done
		c.mu.Unlock()
		result, runErr := terminal.result, terminal.err
		outcome := "completed"
		if runErr != nil {
			outcome = "failed"
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		cancel()
		failure := ""
		if runErr != nil {
			failure = runErr.Error()
		}
		c.completeLaneTurn(runtime, actor, dispatchedTurnID, dispatchedDone, laneTurnCompletion{
			outcome: outcome, result: result.Output, failure: failure,
			nativeID: result.NativeSessionID, failed: runErr != nil,
		})
	}()
	return nil
}

func (c *hostCoordinator) beginLaneExecution(runtime *daemonpkg.Runtime, actor *laneActor, cancel context.CancelFunc) error {
	c.mu.Lock()
	actor.cancel, actor.state, actor.startedAt = cancel, "running", time.Now().UnixMilli()
	actor.deadlineAt = 0
	if actor.turnTimeout > 0 {
		actor.deadlineAt = actor.startedAt + actor.turnTimeout.Milliseconds()
	}
	actor.interruptRequested = false
	c.mu.Unlock()
	return c.markLaneRunning(runtime, actor)
}

func (c *hostCoordinator) recordLaneNativeID(runtime *daemonpkg.Runtime, actor *laneActor, nativeID string) error {
	if strings.TrimSpace(nativeID) == "" {
		return errors.New("native lane session identity is empty")
	}
	c.mu.Lock()
	if actor.nativeID != "" && actor.nativeID != nativeID {
		selected := actor.nativeID
		c.mu.Unlock()
		return fmt.Errorf("native lane identity changed from %s to %s", selected, nativeID)
	}
	actor.nativeID = nativeID
	c.mu.Unlock()
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	return engine.SetNativeSessionID(actor.id, nativeID)
}

func parseCodexStartedThreadID(line []byte) string {
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "thread.started" || !looksLikeUUID(event.ThreadID) {
		return ""
	}
	return event.ThreadID
}

func laneTurnContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

func (c *hostCoordinator) failLaneDispatch(runtime *daemonpkg.Runtime, actor *laneActor, cause error) error {
	c.mu.Lock()
	actor.state = "terminal"
	actor.outcome = "failed"
	actor.failure = cause.Error()
	actor.completedAt = time.Now().UnixMilli()
	actor.cancel = nil
	actor.capability = ""
	if err := c.markLaneTerminal(runtime, actor); err != nil {
		actor.failure = fmt.Sprintf("%s; persist terminal lane state: %v", actor.failure, err)
		terminal := cloneLaneActor(actor)
		close(actor.done)
		c.mu.Unlock()
		c.queueLaneTerminalNotice(runtime, &terminal)
		return fmt.Errorf("%w; persist failed lane dispatch: %w", cause, err)
	}
	terminal := cloneLaneActor(actor)
	close(actor.done)
	c.mu.Unlock()
	c.queueLaneTerminalNotice(runtime, &terminal)
	return cause
}

func prepareLaneTurnLocked(actor *laneActor) {
	actor.done = make(chan struct{})
	actor.turnID = commandRequestID()
	actor.outcome, actor.result, actor.failure = "", "", ""
	actor.startedAt, actor.completedAt, actor.deadlineAt = 0, 0, 0
	actor.cancel, actor.nativeTurnID = nil, ""
	actor.interruptRequested = false
	// Claim native dispatch while holding the coordinator lock. A concurrent
	// delivery is rejected truthfully while the product is busy.
	actor.state = "preparing"
}

func cloneLaneActor(actor *laneActor) laneActor {
	copyActor := *actor
	copyActor.groups = append([]string(nil), actor.groups...)
	copyActor.arguments = append([]string(nil), actor.arguments...)
	copyActor.explicitGroups = append([]string(nil), actor.explicitGroups...)
	return copyActor
}

type laneTurnCompletion struct {
	outcome, result, failure, nativeID string
	failed                             bool
	clearNativeTurn                    bool
}

func (c *hostCoordinator) completeLaneTurn(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	dispatchedTurnID string,
	dispatchedDone chan struct{},
	completion laneTurnCompletion,
) {
	c.mu.Lock()
	if actor.turnID != dispatchedTurnID || actor.done != dispatchedDone {
		c.mu.Unlock()
		return
	}
	if actor.interruptRequested && completion.failed {
		completion.outcome = "interrupted"
	}
	if completion.nativeID != "" && completion.nativeID != actor.nativeID {
		completion.failed, completion.outcome = true, "failed"
		identityFailure := "native lane completion identity did not match the opened product session"
		if actor.nativeID == "" {
			identityFailure = "native lane completion arrived before the product session opened"
		}
		if completion.failure == "" {
			completion.failure = identityFailure
		} else {
			completion.failure += "; " + identityFailure
		}
	}
	actor.state, actor.outcome, actor.result = "terminal", completion.outcome, completion.result
	actor.failure, actor.completedAt, actor.cancel = completion.failure, time.Now().UnixMilli(), nil
	if completion.clearNativeTurn {
		actor.nativeTurnID = ""
	}
	actor.capability = ""
	if persistErr := c.markLaneTerminal(runtime, actor); persistErr != nil {
		actor.outcome = "failed"
		actor.failure = fmt.Sprintf("persist terminal lane state: %v", persistErr)
	}
	terminal := cloneLaneActor(actor)
	close(dispatchedDone)
	c.mu.Unlock()
	c.queueLaneTerminalNotice(runtime, &terminal)
	c.archiveOrphanedCompletedLane(runtime, actor)
}

func (c *hostCoordinator) archiveOrphanedCompletedLane(runtime *daemonpkg.Runtime, actor *laneActor) bool {
	c.mu.Lock()
	persistent, parentID := actor.persistent, actor.parentID
	c.mu.Unlock()
	if persistent {
		return false
	}
	live, err := c.localParentIsLive(runtime, parentID)
	if err != nil || live {
		return false
	}
	c.mu.Lock()
	if actor.state == "running" || actor.state == "preparing" {
		c.mu.Unlock()
		return false
	}
	actor.state = "archived"
	c.mu.Unlock()
	return c.commitLaneState(runtime, actor.id, "archived") == nil && c.archiveNativeLane(actor) == nil
}

func (c *hostCoordinator) archiveNativeLane(actor *laneActor) error {
	if actor == nil {
		return nil
	}
	switch actor.product {
	case "codex":
		if !looksLikeUUID(actor.nativeID) {
			return nil
		}
		native, err := c.codexNative()
		if err != nil {
			return err
		}
		adapter, err := newCodexLaneAdapter(native)
		if err != nil {
			return err
		}
		return adapter.Archive(context.Background(), actor.nativeID)
	case "claude":
		return c.claudeLanes.Archive(actor.id)
	case "grok":
		return c.grokLanes.Archive(actor.id)
	case "qwen":
		return c.qwenLanes.Archive(actor.id)
	default:
		return errors.New("unsupported lane product")
	}
}

type cappedLaneBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	parsed int
	onLine func([]byte) error
}

func (b *cappedLaneBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.buffer.Len()+len(p) > laneOutputLimit {
		b.mu.Unlock()
		return 0, errors.New("native lane output exceeded 16 MiB")
	}
	n, err := b.buffer.Write(p)
	lines := make([][]byte, 0)
	if err == nil && b.onLine != nil {
		body := b.buffer.Bytes()
		for {
			relative := bytes.IndexByte(body[b.parsed:], '\n')
			if relative < 0 {
				break
			}
			end := b.parsed + relative
			lines = append(lines, append([]byte(nil), body[b.parsed:end]...))
			b.parsed = end + 1
		}
	}
	b.mu.Unlock()
	for _, line := range lines {
		if callbackErr := b.onLine(line); callbackErr != nil {
			return n, callbackErr
		}
	}
	return n, err
}

func (b *cappedLaneBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *cappedLaneBuffer) String() string {
	return string(b.Bytes())
}

//nolint:gocyclo // Product-specific argv construction preserves each native CLI's distinct contract.
func laneNativeCommand(actor *laneActor, prompt string, resume bool) (*exec.Cmd, error) {
	path, err := laneExecutable(actor.product)
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, 16+len(actor.arguments))
	switch actor.product {
	case "codex":
		if resume {
			args = append(args, "exec", "resume", actor.nativeID, "--json", "--skip-git-repo-check")
		} else {
			args = append(args, "exec", "--json", "--skip-git-repo-check", "-C", actor.cwd)
		}
		if actor.permission == "bypassPermissions" {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		if actor.approvalPolicy != "" && actor.permission != "bypassPermissions" {
			args = append(args, "-c", fmt.Sprintf("approval_policy=%q", actor.approvalPolicy))
		}
		if actor.sandbox != "" && actor.permission != "bypassPermissions" {
			if resume {
				args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", actor.sandbox))
			} else {
				args = append(args, "--sandbox", actor.sandbox)
			}
		}
		if actor.effort != "" {
			args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", actor.effort))
		}
		if actor.schema != "" {
			args = append(args, "--output-schema", actor.schema)
		}
		args = append(args, actor.arguments...)
		args = append(args, prompt)
	case "claude":
		command, commandErr := daemonpkg.NewClaudeLaneAdapter().Command(daemonpkg.ClaudeLaneRequest{
			LaneID: actor.id, NativeSession: actor.nativeID, Name: actor.name, Prompt: prompt,
			PermissionMode: actor.permission, Arguments: append([]string(nil), actor.arguments...), Resume: resume,
		})
		if commandErr != nil {
			return nil, commandErr
		}
		args = command.Arguments
	case "grok":
		args = append(args, "--no-auto-update", "--output-format", "json", "--cwd", actor.cwd)
		if resume {
			args = append(args, "--resume", actor.nativeID)
		} else {
			args = append(args, "--session-id", actor.id)
		}
		if actor.permission == "bypassPermissions" {
			args = append(args, "--always-approve")
		} else if actor.permission != "" {
			args = append(args, "--permission-mode", actor.permission)
		}
		args = append(args, actor.arguments...)
		args = append(args, "--single", prompt)
	case "qwen":
		if resume {
			args = append(args, "--resume", actor.nativeID)
		} else {
			args = append(args, "--session-id", actor.id)
		}
		if actor.permission == "bypassPermissions" || actor.permission == "yolo" {
			args = append(args, "--approval-mode", "yolo")
		}
		args = append(args, actor.arguments...)
		args = append(args, "--output-format", "json", "--prompt", prompt)
	default:
		return nil, errors.New("unsupported lane product")
	}
	return exec.Command(path, args...), nil //nolint:gosec // installed product and structured arguments.
}

func laneExecutable(product string) (string, error) {
	path, err := launcher.ResolveProductExecutable(product)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", product, err)
	}
	return resolved, nil
}

func cleanLaneEnvironment(input []string) []string {
	result := make([]string, 0, len(input))
	for _, entry := range input {
		if strings.HasPrefix(entry, "AGENT_SESSIONS_SESSION_ID=") || strings.HasPrefix(entry, "AGENT_SESSIONS_PRODUCT=") || strings.HasPrefix(entry, "AGENT_SESSIONS_QWEN_CAPABILITY=") || strings.HasPrefix(entry, "AGENT_SESSIONS_LANE_CAPABILITY=") || strings.HasPrefix(entry, "AGENT_SESSIONS_HOST_BINARY=") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func laneWorkerEnvironment(input []string, actor *laneActor) []string {
	result := cleanLaneEnvironment(input)
	if executable, err := os.Executable(); err == nil {
		result = append(result, "AGENT_SESSIONS_HOST_BINARY="+executable)
	}
	return append(result,
		"AGENT_SESSIONS_SESSION_ID="+actor.id,
		"AGENT_SESSIONS_PRODUCT="+actor.product,
		"AGENT_SESSIONS_LANE_CAPABILITY="+actor.capability,
	)
}

//nolint:gocyclo // Native result decoding accepts the documented result forms for all four products.
func parseLaneNativeResult(product string, body []byte) (string, string) {
	var documents []any
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var values []any
		if json.Unmarshal(trimmed, &values) == nil {
			documents = values
		}
	} else {
		for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
			var value any
			if json.Unmarshal(line, &value) == nil {
				documents = append(documents, value)
			}
		}
		if len(documents) == 0 {
			var value any
			if json.Unmarshal(trimmed, &value) == nil {
				documents = append(documents, value)
			}
		}
	}
	var session, result string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				text, _ := child.(string)
				switch key {
				case "session_id", "thread_id", "threadId":
					if session == "" && looksLikeUUID(text) {
						session = text
					}
				case "result":
					if text != "" {
						result = text
					}
				case "text":
					if text != "" && (product == "codex" || result == "") {
						result = text
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	for _, document := range documents {
		walk(document)
	}
	return result, session
}

func looksLikeUUID(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-'
}

func (c *hostCoordinator) resolveLaneActor(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product, target string, all bool) (*laneActor, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("lane selector is required")
	}
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var matches []*laneActor
	for _, actor := range c.lanes {
		if actor.product != product || actor.id != target && actor.name != target || !all && actor.state == "archived" || actor.parentID != parent.ID && !groupsIntersect(parentGroups, actor.groups) {
			continue
		}
		matches = append(matches, actor)
	}
	if len(matches) == 0 {
		return nil, errors.New("lane was not found")
	}
	if len(matches) > 1 {
		return nil, errors.New("lane name is ambiguous; use UUID")
	}
	return matches[0], nil
}

func (c *hostCoordinator) liveLaneNameLocked(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, name string) bool {
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return true
	}
	for _, a := range c.lanes {
		if a.name == name && a.state != "archived" && (a.parentID == parent.ID || groupsIntersect(a.groups, parentGroups)) {
			return true
		}
	}
	return false
}

func (c *hostCoordinator) anchorLaneGroups(runtime *daemonpkg.Runtime, groups []string, parentID, laneID string) ([]string, error) {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(snapshot.Catalog.Host.Host)
	if host == "" {
		return nil, errors.New("daemon host identity is unavailable")
	}
	parentAnchor := "session:" + host + "/" + parentID
	if strings.Contains(parentID, "/") {
		parentAnchor = "session:" + parentID
	}
	return uniqueStrings(append(groups,
		parentAnchor,
		"session:"+host+"/"+laneID,
	)), nil
}

func (c *hostCoordinator) attachmentVisibilityGroups(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) ([]string, error) {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(snapshot.Catalog.Host.Host)
	if host == "" {
		return nil, errors.New("daemon host identity is unavailable")
	}
	anchor := "session:" + host + "/" + attachment.ID
	if strings.Contains(attachment.ID, "/") {
		anchor = "session:" + attachment.ID
	}
	return uniqueStrings(append(append([]string(nil), attachment.Groups...), anchor)), nil
}

func (c *hostCoordinator) effectiveLaneGroups(runtime *daemonpkg.Runtime, actor *laneActor, parent daemonpkg.ManagedAttachment) ([]string, error) {
	groups := append([]string(nil), actor.explicitGroups...)
	if actor.inheritGroups {
		groups = append(groups, parent.Groups...)
	}
	return c.anchorLaneGroups(runtime, uniqueStrings(groups), parent.ID, actor.id)
}

func laneActorStatus(a *laneActor) map[string]any {
	return map[string]any{
		"type": "lane.status", "product": a.product, "thread_id": a.id, "session_id": a.nativeID,
		"name": a.name, "cwd": a.cwd, "groups": append([]string(nil), a.groups...), "permission_mode": a.permission,
		"state": a.state, "turn_id": a.turnID, "outcome": a.outcome, "exit": laneOutcomeExit(a.outcome),
		"owner_session_id": a.parentID, "persistent": a.persistent,
		"auto_archive": a.autoArchive, "auto_archive_after_seconds": a.autoArchiveDelay.Seconds(), "auto_archive_at": a.autoArchiveAt,
	}
}
func laneReadyResult(a *laneActor) map[string]any {
	value := laneActorStatus(a)
	value["type"] = "lane.ready"
	value["contract_version"] = 2
	return value
}
func laneActorResult(a *laneActor) map[string]any {
	return map[string]any{
		"type": "turn.completed", "product": a.product, "thread_id": a.id, "session_id": a.nativeID,
		"turn_id": a.turnID, "status": a.outcome, "outcome": a.outcome, "exit": laneOutcomeExit(a.outcome),
		"result": a.result, "diagnostic": a.failure,
	}
}

func laneOutcomeExit(outcome string) any {
	if outcome == "" {
		return nil
	}
	return laneOutcomeExitCode(outcome)
}

func laneOutcomeExitCode(outcome string) int {
	switch outcome {
	case "completed":
		return 0
	case "interrupted":
		return 130
	case "timed_out":
		return 124
	default:
		return 1
	}
}

func laneDefaultPermission(product, parent string) string {
	if parent == "bypassPermissions" {
		return parent
	}
	if product == "claude" {
		return "dontAsk"
	}
	return "default"
}

// validateGrokLanePermission prevents the generic option layer from silently
// widening a Grok lane. A new Grok lane requires an explicit request that
// canonicalizes to bypassPermissions; inheriting a bypass parent is not that
// request. Resume keeps the permission recorded by a previously accepted lane,
// but an explicitly supplied replacement must still be the supported mode.
func validateGrokLanePermission(product string, options parsedLaneCommand, start bool) error {
	if product != "grok" {
		return nil
	}
	if options.permissionExplicit && options.permission == "bypassPermissions" {
		return nil
	}
	if !start && !options.permissionExplicit {
		return nil
	}
	return errors.New("grok lanes require an explicit bypassPermissions permission selection")
}
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	sort.Strings(result)
	return result
}
func newLaneUUID() (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	h := hex.EncodeToString(body)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func (c *hostCoordinator) ensureLaneActors(_ *daemonpkg.Runtime) error {
	c.mu.Lock()
	c.lanesLoaded = true
	c.mu.Unlock()
	return nil
}
func (c *hostCoordinator) commitNewLane(r *daemonpkg.Runtime, a *laneActor) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	return engine.Create(durableLane(a, "idle"))
}
func (c *hostCoordinator) markLaneRunning(r *daemonpkg.Runtime, a *laneActor) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	lane := durableLane(a, "running")
	lane.CapabilityHash = daemonpkg.CapabilityDigest(a.capability)
	if err := engine.Update(lane); err != nil {
		return err
	}
	return engine.TransitionLane(a.id, "running", lane.CapabilityHash)
}
func (c *hostCoordinator) markLaneTerminal(r *daemonpkg.Runtime, a *laneActor) error {
	return c.commitLaneState(r, a.id, "terminal")
}
func (c *hostCoordinator) commitResumeLane(r *daemonpkg.Runtime, a *laneActor, _ bool) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	return engine.Update(durableLane(a, "idle"))
}
func (c *hostCoordinator) commitLaneState(r *daemonpkg.Runtime, id, state string) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	return engine.TransitionLane(id, state, "")
}

func durableLane(a *laneActor, state string) daemonpkg.Lane {
	lane := daemonpkg.Lane{ID: a.id, ParentAttachmentID: a.parentID, Product: a.product, Name: a.name, Cwd: a.cwd, State: state}
	copyLanePolicy(&lane, a)
	return lane
}

func copyLanePolicy(lane *daemonpkg.Lane, a *laneActor) {
	lane.NativeSessionID = a.nativeID
	lane.Groups = append([]string(nil), a.groups...)
	lane.ExplicitGroups = append([]string(nil), a.explicitGroups...)
	lane.InheritGroups = a.inheritGroups
	lane.PermissionMode = a.permission
	lane.ApprovalPolicy = a.approvalPolicy
	lane.Sandbox = a.sandbox
	lane.Effort = a.effort
	lane.Schema = a.schema
	lane.Arguments = append([]string(nil), a.arguments...)
	lane.Persistent = a.persistent
	lane.AutoArchive = a.autoArchive
	lane.AutoArchiveDelayMS = a.autoArchiveDelay.Milliseconds()
	lane.AutoArchiveAt = a.autoArchiveAt
}

func (c *hostCoordinator) armLaneAutoArchive(runtime *daemonpkg.Runtime, actor *laneActor) {
	c.mu.Lock()
	eligible := actor.autoArchive && actor.state == "idle"
	c.mu.Unlock()
	if !eligible {
		return
	}
	c.scheduleLaneAutoArchive(runtime, actor)
}

func (c *hostCoordinator) scheduleLaneAutoArchive(runtime *daemonpkg.Runtime, actor *laneActor) {
	c.mu.Lock()
	due := actor.autoArchiveAt
	eligible := actor.autoArchive && actor.state == "idle" && due > 0
	c.mu.Unlock()
	if !eligible {
		return
	}
	go func(expected int64) {
		delay := time.Until(time.UnixMilli(expected))
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-c.ctx.Done():
				return
			case <-timer.C:
			}
		}
		c.mu.Lock()
		if actor.state != "idle" || !actor.autoArchive || actor.autoArchiveAt != expected {
			c.mu.Unlock()
			return
		}
		actor.state, actor.autoArchiveAt = "archived", 0
		c.mu.Unlock()
		if c.commitLaneState(runtime, actor.id, "archived") == nil && c.archiveNativeLane(actor) == nil {
			_ = c.retireParentLanes(runtime, actor.id)
		}
	}(due)
}

func (c *hostCoordinator) commitLaneAuthorization(r *daemonpkg.Runtime, a *laneActor, state string) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	return engine.TransitionLane(a.id, state, daemonpkg.CapabilityDigest(a.capability))
}
