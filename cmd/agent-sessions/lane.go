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
	"sort"
	"strings"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/launcher"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

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
	projected, err := launcher.ProjectNativeLaneArguments(envelope.Product, envelope.Arguments)
	if err != nil {
		return nil, err
	}
	parsed, err := parseUnifiedLaneCommand(projected)
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
	if strings.TrimSpace(envelope.Cwd) != "" {
		parent.Cwd = envelope.Cwd
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
	for _, candidate := range []string{"-m", "--model", "--agent", "--effort", "--reasoning-effort", "--thinking", "--sandbox", "--approval-policy", "--config", "-c", "--schema", "--max-budget-usd", "--tools", "--allowed-tools", "--disallowed-tools", "--qwen-home"} {
		if argument == candidate {
			return true
		}
	}
	return false
}

func laneInvocationCwd(parent, requested string) (string, error) {
	cwd := requested
	if cwd == "" {
		cwd = parent
	}
	if !filepath.IsAbs(cwd) {
		if parent == "" {
			return "", errors.New("lane cwd is unavailable")
		}
		cwd = filepath.Join(parent, cwd)
	}
	resolved, err := pathidentity.ExistingDirectory(filepath.Clean(cwd))
	if err != nil {
		return "", errors.New("lane cwd is unavailable")
	}
	return resolved, nil
}

//nolint:gocyclo // Start is one durable authorization, native dispatch, publication, and rollback transaction.
func (c *hostCoordinator) startLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand, input string, wait bool) (map[string]any, error) {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(options.name) == "" {
		return nil, classifyLiveError(errLiveInvalidParams, errors.New("lane start/run requires --name and non-empty input"))
	}
	cwd, err := laneInvocationCwd(parent.Cwd, options.cwd)
	if err != nil {
		return nil, classifyLiveError(errLiveInvalidParams, err)
	}
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	if err := validateLaneGroupNames(options.groups, parentGroups); err != nil {
		return nil, classifyLiveError(errLiveInvalidParams, err)
	}
	nativePath, err := laneExecutable(product)
	if err != nil {
		return nil, classifyLiveError(errLiveProductUnavailable, err)
	}
	readiness := inspectLaneProductReadiness(ctx, product, nativePath, cwd)
	if ready, _ := readiness["ready"].(bool); !ready {
		return nil, classifyLiveError(errLiveProductUnavailable, fmt.Errorf("%s lane readiness is not established: %v", product, readiness["readiness_error"]))
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
		return nil, classifyLiveError(errLiveBusy, fmt.Errorf("visible lane name %q is already live", options.name))
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
	if err := c.dispatchLaneTurn(runtime, actor, input); err != nil {
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
		return nil, classifyLiveError(errLiveInvalidParams, errors.New("lane resume requires one selector and non-empty input"))
	}
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	if len(options.groups) > 0 {
		parentGroups, groupErr := c.attachmentVisibilityGroups(runtime, parent)
		if groupErr != nil {
			return nil, groupErr
		}
		if groupErr := validateLaneGroupNames(options.groups, parentGroups); groupErr != nil {
			return nil, classifyLiveError(errLiveInvalidParams, groupErr)
		}
	}
	cwd, err := laneInvocationCwd(parent.Cwd, options.cwd)
	if err != nil {
		return nil, classifyLiveError(errLiveInvalidParams, err)
	}
	c.mu.Lock()
	if actor.parentID != "" && actor.parentID != parent.ID && actor.state != "archived" {
		owner := actor.parentID
		c.mu.Unlock()
		return nil, classifyLiveError(errLiveNotPermitted, fmt.Errorf("lane is live under %s", owner))
	}
	if actor.state == "running" {
		c.mu.Unlock()
		return nil, classifyLiveError(errLiveBusy, errors.New("collect or interrupt the active lane turn before resume"))
	}
	prepareLaneTurnLocked(actor)
	actor.parentID = parent.ID
	actor.cwd = cwd
	if len(options.groups) > 0 {
		actor.explicitGroups = uniqueStrings(options.groups)
	}
	if options.inheritGroups {
		actor.inheritGroups = true
	}
	if options.noInheritGroups {
		actor.inheritGroups = false
	}
	actor.approvalPolicy = options.approvalPolicy
	actor.sandbox = options.sandbox
	actor.effort = options.effort
	actor.schema = options.schema
	actor.arguments = append([]string(nil), options.native...)
	actor.permission = options.permission
	if actor.permission == "" {
		actor.permission = laneDefaultPermission(parent.PermissionMode)
	}
	if options.persistentSet {
		actor.persistent = options.persistent
	}
	actor.autoArchive, actor.autoArchiveDelay = true, defaultUnifiedLaneAutoArchiveDelay
	actor.autoArchiveAt = 0
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
	if err := c.dispatchLaneTurn(runtime, actor, input); err != nil {
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
		return nil, classifyLiveError(errLiveBusy, errors.New("lane already has an active collector"))
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
		return nil, classifyLiveError(errLiveBusy, errors.New("lane has no live turn result"))
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
	running := actor.state == "running" && actor.cancel != nil
	if running {
		actor.state = "interrupting"
	}
	c.mu.Unlock()
	if !running {
		return nil, classifyLiveError(errLiveBusy, errors.New("lane has no active turn"))
	}
	if err := c.interruptLaneNative(actor); err != nil {
		return nil, err
	}
	return map[string]any{"type": "turn.interrupting", "session_id": actor.id, "turn_id": actor.turnID}, nil
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
		return nil, classifyLiveError(errLiveNoRunningTurn, errors.New("lane has no active native turn"))
	}
	turn := productruntime.NativeTurnRef{
		NativeSessionRef: productruntime.NativeSessionRef{
			LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
		},
		NativeTurnID: actor.nativeTurnID,
	}
	sessionID, turnID := actor.id, actor.turnID
	permission, arguments := actor.permission, append([]string(nil), actor.arguments...)
	approvalPolicy, sandbox, effort, schema := actor.approvalPolicy, actor.sandbox, actor.effort, actor.schema
	c.mu.Unlock()
	driver, ok := c.laneDrivers.ByProduct(product)
	if !ok || !driver.Capabilities().Steer {
		return nil, fmt.Errorf("%w: %s lanes do not support steer", productruntime.ErrUnsupportedSteer, product)
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
		"type": "turn.steered", "product": product, "session_id": sessionID,
		"turn_id": turnID, "native_message_id": accepted.NativeMessageID,
	}, nil
}

func (c *hostCoordinator) interruptLaneNative(actor *laneActor) error {
	c.mu.Lock()
	product := actor.product
	session := productruntime.NativeSessionRef{
		LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
	}
	turn := productruntime.NativeTurnRef{NativeSessionRef: session, NativeTurnID: actor.nativeTurnID}
	c.mu.Unlock()
	driver, ok := c.laneDrivers.ByProduct(product)
	if !ok {
		return fmt.Errorf("unsupported lane product %q", product)
	}
	interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer interruptCancel()
	return driver.Interrupt(interruptCtx, turn)
}

func (c *hostCoordinator) archiveLane(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if actor.state == "archived" {
		result := map[string]any{
			"type": "lane.archived", "product": product, "session_id": actor.id,
			"name": actor.name, "already_archived": true,
		}
		c.mu.Unlock()
		return result, nil
	}
	if actor.state == "running" {
		c.mu.Unlock()
		return nil, classifyLiveError(errLiveBusy, errors.New("refuse to archive a lane with an active turn"))
	}
	actor.state = "archived"
	c.mu.Unlock()
	if err := c.archiveNativeLane(actor); err != nil {
		return nil, err
	}
	if err := c.retireParentLanes(runtime, actor.id); err != nil {
		return nil, err
	}
	return map[string]any{"type": "lane.archived", "product": product, "session_id": actor.id, "name": actor.name}, nil
}

func (c *hostCoordinator) deliverLaneMessage(ctx context.Context, actor *laneActor, message productruntime.NativeMessage) error {
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
	return c.deliverPreparedMessage(ctx, daemonpkg.ManagedAttachment{ID: session.NativeSessionID}, message)
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
		if actor.parentID != parentID || actor.state == "archived" || actor.state == "retiring" {
			continue
		}
		if actor.persistent {
			actor.parentID = ""
			continue
		}
		state := "archived"
		var cancel context.CancelFunc
		if (actor.state == "running" || actor.state == "preparing" || actor.state == "interrupting") && actor.cancel != nil {
			state, cancel = "retiring", actor.cancel
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
				continue
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
	body, err := exec.CommandContext(probeCtx, path, "--version").CombinedOutput()
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

func (c *hostCoordinator) dispatchLaneTurn(runtime *daemonpkg.Runtime, actor *laneActor, prompt string) error {
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
	driver, ok := c.laneDrivers.ByProduct(actor.product)
	if !ok {
		return c.failLaneDispatch(runtime, actor, fmt.Errorf("unsupported lane product %q", actor.product))
	}
	return c.dispatchProductLaneTurn(runtime, actor, prompt, driver)
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
	environment, err := laneWorkerEnvironment(runtime, os.Environ(), actor, driver.Capabilities())
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	resumeNativeID := actor.nativeID
	session, err := driver.Open(c.ctx, productruntime.LaneOpenRequest{
		ProductID: actor.product, LaneID: actor.id, Name: actor.name, Groups: append([]string(nil), actor.groups...), ResumeNativeID: resumeNativeID,
		Cwd: actor.cwd, PermissionMode: mode, Arguments: append([]string(nil), actor.arguments...),
		Environment: environment, ApprovalPolicy: actor.approvalPolicy,
		Sandbox: actor.sandbox, Capability: actor.capability, Effort: actor.effort,
	})
	if err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	if err := c.recordLaneNativeID(runtime, actor, session, resumeNativeID == ""); err != nil {
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

func (c *hostCoordinator) beginLaneExecution(runtime *daemonpkg.Runtime, actor *laneActor, cancel context.CancelFunc) error {
	c.mu.Lock()
	actor.cancel, actor.state, actor.startedAt = cancel, "running", time.Now().UnixMilli()
	actor.deadlineAt = 0
	if actor.turnTimeout > 0 {
		actor.deadlineAt = actor.startedAt + actor.turnTimeout.Milliseconds()
	}
	c.mu.Unlock()
	return c.markLaneRunning(runtime, actor)
}

func (c *hostCoordinator) recordLaneNativeID(runtime *daemonpkg.Runtime, actor *laneActor, session productruntime.NativeSessionRef, fresh bool) error {
	nativeID := session.NativeSessionID
	if strings.TrimSpace(nativeID) == "" {
		return errors.New("native lane session identity is empty")
	}
	if session.LaneID != nativeID {
		return fmt.Errorf("%w: lane driver returned lane %q for native session %q", productruntime.ErrProtocol, session.LaneID, nativeID)
	}
	c.mu.Lock()
	if actor.nativeID != "" {
		if actor.nativeID != nativeID {
			selected := actor.nativeID
			c.mu.Unlock()
			return fmt.Errorf("native lane identity changed from %s to %s", selected, nativeID)
		}
	}
	primary := "session:" + runtime.HostID() + "/" + actor.parentID
	temporary := primary + "/" + actor.id
	stable := primary + "/" + nativeID
	for index, group := range actor.groups {
		if group == temporary {
			actor.groups[index] = stable
		}
	}
	actor.groups = uniqueStrings(actor.groups)
	if actor.id != nativeID {
		if current := c.lanes[actor.id]; current != actor {
			c.mu.Unlock()
			return fmt.Errorf("%w: provisional lane identity is stale", productruntime.ErrStale)
		}
		if current := c.lanes[nativeID]; current != nil && current != actor {
			c.mu.Unlock()
			return fmt.Errorf("%w: native lane %q already has an actor", productruntime.ErrAmbiguousSession, nativeID)
		}
		oldID := actor.id
		delete(c.lanes, oldID)
		c.lanes[nativeID] = actor
		for reportedID, actorID := range c.reportedLanes {
			if actorID == oldID {
				c.reportedLanes[reportedID] = nativeID
			}
		}
		actor.id = nativeID
	}
	actor.nativeID = nativeID
	c.mu.Unlock()
	if fresh {
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
	driver, ok := c.laneDrivers.ByProduct(actor.product)
	if !ok {
		return fmt.Errorf("unsupported lane product %q", actor.product)
	}
	if actor.nativeID == "" {
		return nil
	}
	return driver.Archive(context.Background(), productruntime.NativeSessionRef{
		LaneID: actor.id, NativeSessionID: actor.nativeID, Generation: actor.nativeGeneration,
	})
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

func laneWorkerEnvironment(runtime *daemonpkg.Runtime, input []string, actor *laneActor, capabilities productruntime.LaneCapabilitySet) ([]string, error) {
	result := cleanLaneEnvironment(input)
	groups := append([]string(nil), actor.groups...)
	sessionID := actor.nativeID
	if sessionID == "" && capabilities.CallerSuppliedSessionID {
		sessionID = actor.id
	}
	if sessionID == "" {
		primary, err := parentPrivateGroup(runtime, daemonpkg.ManagedAttachment{ID: actor.parentID, Groups: actor.groups})
		if err != nil {
			return nil, err
		}
		provisionalAnchor := primary + "/" + actor.id
		filtered := groups[:0]
		for _, group := range groups {
			if group != provisionalAnchor {
				filtered = append(filtered, group)
			}
		}
		groups = filtered
	}
	encodedGroups, _ := json.Marshal(groups)
	if executable, err := os.Executable(); err == nil {
		result = append(result, "AGENT_SESSIONS_HOST_BINARY="+executable)
	}
	if sessionID != "" {
		result = append(result, "AGENT_SESSIONS_SESSION_ID="+sessionID)
	}
	return append(result,
		"AGENT_SESSIONS_PRODUCT="+actor.product,
		"AGENT_SESSIONS_SESSION_NAME="+actor.name,
		"AGENT_SESSIONS_GROUPS="+string(encodedGroups),
		"AGENT_SESSIONS_LANE_CAPABILITY="+actor.capability,
	), nil
}

func (c *hostCoordinator) resolveLaneActor(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product, target string, all bool) (*laneActor, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, classifyLiveError(errLiveInvalidParams, errors.New("lane selector is required"))
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
		if actor.product != product || actor.id != target && actor.name != target || !all && actor.state == "archived" || actor.parentID != parent.ID && !groupsIntersect(parentGroups, actor.groups) {
			continue
		}
		matches = append(matches, actor)
	}
	if len(matches) == 0 {
		return nil, classifyLiveError(errLiveUnknown, errors.New("lane was not found"))
	}
	if len(matches) > 1 {
		return nil, classifyLiveError(errLiveUnknown, errors.New("lane name is ambiguous; use UUID"))
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
	groups := append([]string(nil), actor.explicitGroups...)
	if actor.inheritGroups {
		groups = append(groups, parent.Groups...)
	}
	return c.anchorLaneGroups(runtime, uniqueStrings(groups), parent, laneGroupIdentity(actor))
}

func laneGroupIdentity(actor *laneActor) string {
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
		"type": "lane.status", "product": a.product, "session_id": a.id,
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
		"type": "turn.completed", "product": a.product, "session_id": a.id,
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
		UUID: actor.nativeID, Name: actor.name, Product: actor.product,
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
