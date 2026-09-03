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
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const laneOutputLimit = 16 << 20
const defaultUnifiedLaneAutoArchiveDelay = time.Minute

type laneActor struct {
	id, product, name, cwd, parentID, nativeID string
	nativeTurnID                               string
	nativeGeneration                           uint64
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
	case "steer":
		result, err = c.steerLane(ctx, runtime, parent, product, parsed, input)
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
	if (result.command == "run" || result.command == "start") && result.target != "" {
		return parsedLaneCommand{}, fmt.Errorf("lane %s reads its prompt from stdin; positional prompts are not accepted", result.command)
	}
	// An explicit native approval policy takes precedence over an inherited
	// parent mode and any generic permission flag, independent of option order.
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
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	if err := validateLaneGroupNames(options.groups, parentGroups); err != nil {
		return nil, err
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
		permission = laneDefaultPermission(parent.PermissionMode)
	}
	id, err := newLaneUUID()
	if err != nil {
		return nil, err
	}
	groups, err = c.anchorLaneGroups(runtime, groups, parent, id)
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
	if err := c.dispatchLaneTurn(runtime, actor, input, false); err != nil {
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
	actor.permission = laneResumePermission(actor.permission, options.permission, parent.PermissionMode)
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
	if err := c.dispatchLaneTurn(runtime, actor, input, true); err != nil {
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
	if options.all {
		if err := c.ensureActiveLaneNames(c.ctx, runtime, parent, product); err != nil {
			return nil, err
		}
	}
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	lanes := make([]map[string]any, 0)
	liveNativeIDs := map[string]bool{}
	for _, actor := range c.lanes {
		if actor.product != product || options.mine && actor.parentID != parent.ID || !options.all && actor.state == "archived" || !groupsIntersect(parentGroups, actor.groups) && actor.parentID != parent.ID {
			continue
		}
		lanes = append(lanes, laneActorStatus(actor))
		liveNativeIDs[actor.nativeID] = true
	}
	if options.all {
		for _, entry := range c.laneNames[parent.ID] {
			if entry.Product != product || liveNativeIDs[entry.UUID] {
				continue
			}
			lanes = append(lanes, laneActorStatus(&laneActor{
				id: entry.UUID, nativeID: entry.UUID, product: entry.Product,
				name: entry.Name, cwd: entry.Cwd, parentID: entry.Parent,
				groups: append([]string(nil), entry.Groups...), state: "archived",
			}))
		}
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
	running := actor.state == "running" && actor.cancel != nil
	if running {
		actor.interruptRequested = true
		actor.state = "interrupting"
	}
	c.mu.Unlock()
	if !running {
		return nil, errors.New("lane has no active turn")
	}
	if err := c.interruptLaneNative(actor); err != nil {
		return nil, err
	}
	return map[string]any{"type": "turn.interrupting", "thread_id": actor.id, "turn_id": actor.turnID}, nil
}

func (c *hostCoordinator) steerLane(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	product string,
	options parsedLaneCommand,
	input string,
) (map[string]any, error) {
	if strings.TrimSpace(input) == "" {
		return nil, errors.New("lane steer requires non-empty input")
	}
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if actor.state != "running" || actor.nativeTurnID == "" {
		c.mu.Unlock()
		return nil, errors.New("lane has no active native turn")
	}
	turn := productruntime.NativeTurnRef{
		NativeSessionRef: productruntime.NativeSessionRef{
			LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
		},
		NativeTurnID: actor.nativeTurnID,
	}
	threadID, turnID := actor.id, actor.turnID
	permission, arguments := actor.permission, append([]string(nil), actor.arguments...)
	approvalPolicy, sandbox, effort, schema := actor.approvalPolicy, actor.sandbox, actor.effort, actor.schema
	c.mu.Unlock()
	driver, ok := c.laneDrivers.ByProduct(product)
	if !ok || !driver.Capabilities().Steer {
		return nil, fmt.Errorf("%s lanes do not support steer", product)
	}
	mode, err := permissionmode.Parse(permission)
	if err != nil {
		return nil, err
	}
	accepted, err := driver.Steer(ctx, turn, productruntime.TurnStartRequest{
		Prompt: input, PermissionMode: mode, Arguments: arguments,
		ApprovalPolicy: approvalPolicy, Sandbox: sandbox, Effort: effort, SchemaPath: schema,
	})
	if err != nil {
		return nil, err
	}
	if accepted.NativeSessionID != turn.NativeSessionID {
		return nil, fmt.Errorf("%w: lane steer changed native session from %q to %q", productruntime.ErrAmbiguousSession, turn.NativeSessionID, accepted.NativeSessionID)
	}
	return map[string]any{
		"type": "turn.steered", "product": product, "thread_id": threadID,
		"session_id": turn.NativeSessionID, "turn_id": turnID, "native_message_id": accepted.NativeMessageID,
	}, nil
}

func (c *hostCoordinator) interruptLaneNative(actor *laneActor) error {
	c.mu.Lock()
	product, laneID := actor.product, actor.id
	session := productruntime.NativeSessionRef{
		LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
	}
	turn := productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: actor.nativeTurnID}
	c.mu.Unlock()
	if driver, ok := c.laneDrivers.ByProduct(product); ok {
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer interruptCancel()
		return driver.Interrupt(interruptCtx, turn)
	}
	switch product {
	case "grok":
		return c.grokLanes.Interrupt(laneID)
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
	if err := c.archiveNativeLane(actor); err != nil {
		return nil, err
	}
	if err := c.retireParentLanes(runtime, actor.id); err != nil {
		return nil, err
	}
	return map[string]any{"type": "lane.archived", "product": product, "thread_id": actor.id, "session_id": actor.nativeID, "name": actor.name}, nil
}

func (c *hostCoordinator) deliverLaneMessage(ctx context.Context, actor *laneActor, messageID, message string) error {
	c.mu.Lock()
	product := actor.product
	session := productruntime.NativeSessionRef{
		LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
	}
	driver, managed := c.laneDrivers.ByProduct(product)
	c.mu.Unlock()
	if !managed {
		return fmt.Errorf("%s lane has no native inbound path", product)
	}
	if messenger, ok := driver.(productruntime.LaneMessageDriver); ok {
		return messenger.SendMessage(ctx, session, message)
	}
	return c.deliverPreparedMessage(ctx, daemonpkg.ManagedAttachment{ID: session.NativeSessionID}, messageID, message)
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
		actor  *laneActor
		state  string
		cancel context.CancelFunc
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
		candidates = append(candidates, transition{actor: actor, state: state, cancel: cancel})
	}
	c.mu.Unlock()
	for _, candidate := range candidates {
		if candidate.state == "archived" {
			if err := c.archiveNativeLane(candidate.actor); err != nil {
				return err
			}
		}
		if candidate.cancel != nil {
			if _, managed := c.laneDrivers.ByProduct(candidate.actor.product); managed {
				if err := c.interruptLaneNative(candidate.actor); err != nil {
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
	version, versionErr := inspectLaneNativeVersion(ctx, product, path)
	report[product+"_available"] = versionErr == nil
	report[product+"_path"] = path
	report[product+"_version"] = version
	if versionErr != nil {
		report[product+"_error"] = versionErr.Error()
		report["ready"] = false
		report["readiness_error"] = versionErr.Error()
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

//nolint:gocyclo // Dispatch keeps capability, journal, native execution, and failure transitions together.
func (c *hostCoordinator) dispatchLaneTurn(runtime *daemonpkg.Runtime, actor *laneActor, prompt string, resume bool) error {
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
	if driver, ok := c.laneDrivers.ByProduct(actor.product); ok {
		return c.dispatchProductLaneTurn(runtime, actor, prompt, driver)
	}
	if actor.product == "grok" {
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
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		cancel()
		return c.failLaneDispatch(runtime, actor, err)
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
		return c.failLaneDispatch(runtime, actor, err)
	}
	go func() {
		c.mu.Lock()
		dispatchedTurnID, dispatchedDone := actor.turnID, actor.done
		c.mu.Unlock()
		err := command.Wait()
		outcome := "completed"
		if err != nil {
			outcome = "failed"
		}
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		result, nativeID := parseLaneNativeResult(stdout.Bytes())
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

func (c *hostCoordinator) dispatchProductLaneTurn(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	prompt string,
	driver productruntime.LaneDriver,
) error {
	mode, err := permissionmode.Parse(actor.permission)
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	session, err := driver.Open(c.ctx, productruntime.LaneOpenRequest{
		ProductID: actor.product, LaneID: actor.id, Name: actor.name, Groups: append([]string(nil), actor.groups...), ResumeNativeID: actor.nativeID,
		Cwd: actor.cwd, PermissionMode: mode, Arguments: append([]string(nil), actor.arguments...),
		ApprovalPolicy: actor.approvalPolicy, Sandbox: actor.sandbox,
	})
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	if err := c.recordLaneNativeID(runtime, actor, session.NativeSessionID); err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	actor.nativeGeneration = session.Generation
	c.mu.Unlock()
	turn, err := driver.StartTurn(c.ctx, session, productruntime.TurnStartRequest{
		Prompt: prompt, PermissionMode: mode, Arguments: append([]string(nil), actor.arguments...),
		ApprovalPolicy: actor.approvalPolicy, Sandbox: actor.sandbox, Effort: actor.effort, SchemaPath: actor.schema,
	})
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.mu.Lock()
	actor.nativeTurnID = turn.NativeTurnID
	c.mu.Unlock()
	turnCtx, cancel := laneTurnContext(c.ctx, actor.turnTimeout)
	if err := c.beginLaneExecution(runtime, actor, cancel); err != nil {
		cancel()
		return c.failLaneDispatch(runtime, actor, err)
	}
	go c.watchProductLaneTurn(runtime, actor, driver, turn, turnCtx, cancel)
	return nil
}

func (c *hostCoordinator) watchProductLaneTurn(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	driver productruntime.LaneDriver,
	turn productruntime.NativeTurnRef,
	turnCtx context.Context,
	cancel context.CancelFunc,
) {
	c.mu.Lock()
	dispatchedTurnID, dispatchedDone := actor.turnID, actor.done
	c.mu.Unlock()
	result, waitErr := driver.WaitTurn(turnCtx, turn)
	outcome := string(result.Outcome)
	if result.Outcome == productruntime.TurnTimedOut {
		outcome = "timed_out"
	}
	if outcome == "" {
		outcome = "failed"
	}
	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		outcome = "timed_out"
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = driver.Interrupt(interruptCtx, turn)
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
		result, runErr = c.grokLanes.Run(lifecycleCtx, daemonpkg.GrokACPLaneRequest{
			LaneID: laneID, NativeSession: nativeID, PermissionMode: permission, Prompt: prompt,
		}, func(runCtx context.Context, request daemonpkg.GrokACPLaneRequest) (daemonpkg.NativeACPLaneResult, error) {
			return runNative(runCtx, request.PermissionMode, request.NativeSession)
		})
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
	if actor.nativeID == "" {
		primary := "session:" + runtime.HostID() + "/" + actor.parentID
		temporary := primary + "/" + actor.id
		stable := primary + "/" + nativeID
		for index, group := range actor.groups {
			if group == temporary {
				actor.groups[index] = stable
			}
		}
		actor.groups = uniqueStrings(actor.groups)
	}
	actor.nativeID = nativeID
	c.mu.Unlock()
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	candidate, ok := durableLaneCandidate(runtime, actor)
	if !ok {
		return nil
	}
	if err := engine.Remember(candidate); err != nil {
		return err
	}
	c.rememberActiveLaneName(actor)
	return nil
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
	return c.archiveNativeLane(actor) == nil
}

func (c *hostCoordinator) archiveNativeLane(actor *laneActor) error {
	if actor == nil {
		return nil
	}
	if driver, ok := c.laneDrivers.ByProduct(actor.product); ok {
		if actor.nativeID == "" {
			return nil
		}
		return driver.Archive(context.Background(), productruntime.NativeSessionRef{
			LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
		})
	}
	switch actor.product {
	case "grok":
		return c.grokLanes.Archive(actor.id)
	default:
		return errors.New("unsupported lane product")
	}
}

type cappedLaneBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *cappedLaneBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.buffer.Len()+len(p) > laneOutputLimit {
		b.mu.Unlock()
		return 0, errors.New("native lane output exceeded 16 MiB")
	}
	n, err := b.buffer.Write(p)
	b.mu.Unlock()
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
		if strings.HasPrefix(entry, "AGENT_SESSIONS_SESSION_ID=") || strings.HasPrefix(entry, "AGENT_SESSIONS_PRODUCT=") || strings.HasPrefix(entry, "AGENT_SESSIONS_SESSION_NAME=") || strings.HasPrefix(entry, "AGENT_SESSIONS_GROUPS=") || strings.HasPrefix(entry, "AGENT_SESSIONS_LANE_CAPABILITY=") || strings.HasPrefix(entry, "AGENT_SESSIONS_HOST_BINARY=") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func laneWorkerEnvironment(input []string, actor *laneActor) []string {
	result := cleanLaneEnvironment(input)
	groups, _ := json.Marshal(actor.groups)
	if executable, err := os.Executable(); err == nil {
		result = append(result, "AGENT_SESSIONS_HOST_BINARY="+executable)
	}
	return append(result,
		"AGENT_SESSIONS_SESSION_ID="+actor.id,
		"AGENT_SESSIONS_PRODUCT="+actor.product,
		"AGENT_SESSIONS_SESSION_NAME="+actor.name,
		"AGENT_SESSIONS_GROUPS="+string(groups),
		"AGENT_SESSIONS_LANE_CAPABILITY="+actor.capability,
	)
}

//nolint:gocyclo // Native result decoding accepts the documented result forms for all four products.
func parseLaneNativeResult(body []byte) (string, string) {
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
					if text != "" && result == "" {
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
	if all {
		if err := c.ensureActiveLaneNames(c.ctx, runtime, parent, product); err != nil {
			return nil, err
		}
	}
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var matches []*laneActor
	for _, actor := range c.lanes {
		if actor.product != product || actor.id != target && actor.nativeID != target && actor.name != target || !all && actor.state == "archived" || actor.parentID != parent.ID && !groupsIntersect(parentGroups, actor.groups) {
			continue
		}
		matches = append(matches, actor)
	}
	if len(matches) == 0 && all {
		for _, entry := range c.laneNames[parent.ID] {
			if entry.Product != product || entry.UUID != target && entry.Name != target {
				continue
			}
			actor := &laneActor{
				id: entry.UUID, nativeID: entry.UUID, product: entry.Product,
				name: entry.Name, cwd: entry.Cwd, parentID: entry.Parent,
				groups:         append([]string(nil), entry.Groups...),
				explicitGroups: append([]string(nil), entry.SecondaryGroups...),
				state:          "archived", done: make(chan struct{}),
			}
			matches = append(matches, actor)
		}
		if len(matches) == 1 {
			c.lanes[matches[0].id] = matches[0]
		}
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

func (c *hostCoordinator) anchorLaneGroups(runtime *daemonpkg.Runtime, groups []string, parent daemonpkg.ManagedAttachment, laneID string) ([]string, error) {
	parentAnchor, err := parentPrivateGroup(runtime, parent)
	if err != nil {
		return nil, err
	}
	return uniqueStrings(append(groups, parentAnchor, parentAnchor+"/"+laneID)), nil
}

func parentPrivateGroup(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment) (string, error) {
	host := strings.TrimSpace(runtime.HostID())
	if host == "" {
		return "", errors.New("daemon host identity is unavailable")
	}
	fallback := "session:" + host + "/" + parent.ID
	if strings.Contains(parent.ID, "/") {
		fallback = "session:" + parent.ID
	}
	for _, group := range parent.Groups {
		if group == fallback || strings.HasPrefix(group, "session:") && strings.HasSuffix(group, "/"+parent.ID) {
			return group, nil
		}
	}
	return fallback, nil
}

func (c *hostCoordinator) attachmentVisibilityGroups(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) ([]string, error) {
	anchor, err := parentPrivateGroup(runtime, attachment)
	if err != nil {
		return nil, err
	}
	return uniqueStrings(append(append([]string(nil), attachment.Groups...), anchor)), nil
}

func (c *hostCoordinator) effectiveLaneGroups(runtime *daemonpkg.Runtime, actor *laneActor, parent daemonpkg.ManagedAttachment) ([]string, error) {
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	if err := validateLaneGroupNames(actor.explicitGroups, parentGroups); err != nil {
		return nil, err
	}
	groups := append([]string(nil), actor.explicitGroups...)
	if actor.inheritGroups {
		groups = append(groups, parent.Groups...)
	}
	return c.anchorLaneGroups(runtime, uniqueStrings(groups), parent, laneGroupIdentity(actor))
}

func laneGroupIdentity(actor *laneActor) string {
	if actor.nativeID != "" {
		return actor.nativeID
	}
	return actor.id
}

func validateLaneGroupNames(requested, parentGroups []string) error {
	parents := uniqueStrings(parentGroups)
	for _, group := range requested {
		allowed := false
		for _, parent := range parents {
			if group == parent || strings.HasPrefix(group, parent+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			prefixes := make([]string, 0, len(parents))
			for _, parent := range parents {
				prefixes = append(prefixes, parent+"/")
			}
			sort.Strings(prefixes)
			return fmt.Errorf("lane group %q must equal a parent group or start with one of: %s", group, strings.Join(prefixes, ", "))
		}
	}
	return nil
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

func laneDefaultPermission(parent string) string {
	if parent == "bypassPermissions" {
		return parent
	}
	return "default"
}

func laneResumePermission(existing, requested, parent string) string {
	if requested != "" {
		return requested
	}
	if existing != "" {
		return existing
	}
	return laneDefaultPermission(parent)
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
	if a.nativeID != "" {
		candidate, ok := durableLaneCandidate(r, a)
		if !ok {
			return nil
		}
		engine, err := daemonpkg.NewLaneEngine(r.State())
		if err != nil {
			return err
		}
		if err := engine.Remember(candidate); err != nil {
			return err
		}
		c.rememberActiveLaneName(a)
	}
	return nil
}

func (c *hostCoordinator) rememberActiveLaneName(actor *laneActor) {
	if actor == nil || actor.parentID == "" || actor.nativeID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, active := c.liveReports[actor.parentID]; !active {
		return
	}
	if c.laneNames[actor.parentID] == nil {
		c.laneNames[actor.parentID] = map[string]laneNameEntry{}
	}
	c.laneNames[actor.parentID][actor.nativeID] = laneNameEntry{
		UUID: actor.nativeID, Name: actor.name, Product: actor.product, Parent: actor.parentID,
		Groups: append([]string(nil), actor.groups...),
	}
}
func (c *hostCoordinator) markLaneRunning(*daemonpkg.Runtime, *laneActor) error        { return nil }
func (c *hostCoordinator) markLaneTerminal(*daemonpkg.Runtime, *laneActor) error       { return nil }
func (c *hostCoordinator) commitResumeLane(*daemonpkg.Runtime, *laneActor, bool) error { return nil }

func durableLaneCandidate(runtime *daemonpkg.Runtime, actor *laneActor) (daemonpkg.LaneCandidate, bool) {
	if actor == nil || strings.TrimSpace(actor.nativeID) == "" || strings.Contains(actor.parentID, "/") {
		return daemonpkg.LaneCandidate{}, false
	}
	primary := "session:" + runtime.HostID() + "/" + actor.parentID
	laneGroup := primary + "/" + actor.nativeID
	secondary := make([]string, 0, len(actor.groups))
	for _, group := range actor.groups {
		if group != primary && group != laneGroup {
			secondary = append(secondary, group)
		}
	}
	return daemonpkg.LaneCandidate{
		NativeSessionID: actor.nativeID, Product: actor.product, Parent: actor.parentID,
		PrimaryGroup: primary, SecondaryGroups: uniqueStrings(secondary),
	}, true
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
		if c.archiveNativeLane(actor) == nil {
			_ = c.retireParentLanes(runtime, actor.id)
		}
	}(due)
}

func (c *hostCoordinator) commitLaneAuthorization(*daemonpkg.Runtime, *laneActor, string) error {
	return nil
}
