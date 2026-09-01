package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	inputSequence                              uint64
	cancel                                     context.CancelFunc
	done                                       chan struct{}
	inputPump                                  bool
	activeReceiptID                            string
	namePromoting                              bool
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
		return c.handleRemoteLaneCommand(ctx, runtime, parent, envelope, laneCommandInputID(request))
	}
	return c.dispatchLaneCommand(ctx, runtime, parent, envelope.Product, parsed, envelope.Input, laneCommandInputID(request))
}

func laneCommandInputID(request daemonpkg.ControlRequest) string {
	key := strings.TrimSpace(request.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(request.ID)
	}
	if key == "" {
		key = commandRequestID()
	}
	digest := sha256.Sum256([]byte("lane-input\x00" + key))
	return daemonpkg.LaneCommandReceiptPrefix + hex.EncodeToString(digest[:16])
}

func laneIDForInitialReceipt(receiptID string) string {
	const operationReceiptLength = len(daemonpkg.LaneCommandReceiptPrefix) + 32
	if strings.HasPrefix(receiptID, daemonpkg.LaneCommandReceiptPrefix) && len(receiptID) >= operationReceiptLength {
		receiptID = receiptID[:operationReceiptLength]
	}
	digest := sha256.Sum256([]byte("lane-start\x00" + receiptID))
	encoded := hex.EncodeToString(digest[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func initialLaneReceiptID(operationID string, actor *laneActor, input string, wait bool) string {
	identity := struct {
		OperationID        string   `json:"operation_id"`
		ParentAttachmentID string   `json:"parent_attachment_id"`
		Product            string   `json:"product"`
		Name               string   `json:"name"`
		Cwd                string   `json:"cwd"`
		Groups             []string `json:"groups"`
		ExplicitGroups     []string `json:"explicit_groups"`
		InheritGroups      bool     `json:"inherit_groups"`
		PermissionMode     string   `json:"permission_mode"`
		ApprovalPolicy     string   `json:"approval_policy"`
		Sandbox            string   `json:"sandbox"`
		Effort             string   `json:"effort"`
		Schema             string   `json:"schema"`
		Arguments          []string `json:"arguments"`
		Persistent         bool     `json:"persistent"`
		AutoArchive        bool     `json:"auto_archive"`
		AutoArchiveDelayMS int64    `json:"auto_archive_delay_ms"`
		TurnTimeoutNS      int64    `json:"turn_timeout_ns"`
		Wait               bool     `json:"wait"`
		InputDigest        string   `json:"input_digest"`
	}{
		OperationID: operationID, ParentAttachmentID: actor.parentID, Product: actor.product,
		Name: actor.name, Cwd: actor.cwd, Groups: append([]string(nil), actor.groups...),
		ExplicitGroups: append([]string(nil), actor.explicitGroups...), InheritGroups: actor.inheritGroups,
		PermissionMode: actor.permission, ApprovalPolicy: actor.approvalPolicy, Sandbox: actor.sandbox,
		Effort: actor.effort, Schema: actor.schema, Arguments: append([]string(nil), actor.arguments...),
		Persistent: actor.persistent, AutoArchive: actor.autoArchive,
		AutoArchiveDelayMS: actor.autoArchiveDelay.Milliseconds(), TurnTimeoutNS: actor.turnTimeout.Nanoseconds(),
		Wait: wait, InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(input))),
	}
	body, _ := json.Marshal(identity)
	digest := sha256.Sum256(body)
	return operationID + "-" + hex.EncodeToString(digest[:16])
}

func (c *hostCoordinator) dispatchLaneCommand(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	product string,
	parsed parsedLaneCommand,
	input string,
	inputID string,
) (json.RawMessage, error) {
	var err error
	var result map[string]any
	switch parsed.command {
	case "run", "start":
		result, err = c.startLane(ctx, runtime, parent, product, parsed, input, parsed.command == "run", inputID)
	case "resume":
		result, err = c.resumeLane(ctx, runtime, parent, product, parsed, input, inputID)
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
func (c *hostCoordinator) startLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand, input string, wait bool, inputID string) (map[string]any, error) {
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
	operationInputID := inputID
	id := laneIDForInitialReceipt(operationInputID)
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
		arguments: append([]string(nil), options.native...), state: "idle",
	}
	actor.done = make(chan struct{})
	close(actor.done)
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
	inputID = initialLaneReceiptID(operationInputID, actor, input, wait)
	inputEngine, err := c.laneInputEngine(runtime)
	if err != nil {
		return nil, err
	}
	priorSnapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	priorReceipt, replay := priorSnapshot.Catalog.LaneInputs[inputID]
	for _, candidate := range priorSnapshot.Catalog.LaneInputs {
		if candidate.LaneID == id && candidate.ReceiptID != inputID &&
			(candidate.ReceiptID == operationInputID || strings.HasPrefix(candidate.ReceiptID, operationInputID+"-")) {
			return nil, daemonpkg.ErrLaneInputConflict
		}
	}
	if replay && priorReceipt.LaneID != id {
		return nil, daemonpkg.ErrLaneInputConflict
	}
	if replay && !exactLaneStartReplay(priorSnapshot.Catalog.Lanes[id], durableLane(actor, "idle")) {
		return nil, daemonpkg.ErrLaneInputConflict
	}
	queuedReplay := replay && priorReceipt.State == daemonpkg.ReceiptQueued
	stagedReplay := queuedReplay && priorReceipt.Revision == 1
	requestedTurnTimeout := actor.turnTimeout
	c.mu.Lock()
	existingActor := c.lanes[id]
	if stagedReplay && c.liveLaneNameExceptLocked(runtime, parent, options.name, id) {
		c.mu.Unlock()
		return nil, daemonpkg.ErrLaneInputConflict
	}
	if !stagedReplay && existingActor == nil && c.liveLaneNameLocked(runtime, parent, options.name) {
		c.mu.Unlock()
		return nil, fmt.Errorf("visible lane name %q is already live", options.name)
	}
	reserved := existingActor == nil
	if reserved {
		if !replay {
			prepareLaneTurnLocked(actor)
			actor.inputPump, actor.activeReceiptID = true, inputID
		}
		if stagedReplay {
			actor.namePromoting = true
		}
		c.lanes[id] = actor
	} else {
		actor = existingActor
		if queuedReplay {
			actor.turnTimeout = requestedTurnTimeout
		}
		if stagedReplay {
			actor.namePromoting = true
		}
	}
	c.mu.Unlock()
	if stagedReplay {
		defer func() {
			c.mu.Lock()
			actor.namePromoting = false
			c.mu.Unlock()
		}()
	}
	receipt, err := inputEngine.CreateLaneAdmitAndMarkDispatching(
		inputID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), commandRequestID(), []byte(input),
	)
	if err != nil {
		recoverableQueuedError := receipt.ReceiptID != "" && receipt.State == daemonpkg.ReceiptQueued
		c.mu.Lock()
		if recoverableQueuedError {
			resetStagedLaneTurnLocked(actor, "idle")
			queuedReplay = true
		} else if reserved && c.lanes[id] == actor {
			delete(c.lanes, id)
		}
		c.mu.Unlock()
		if !recoverableQueuedError {
			return nil, err
		}
	}
	c.mu.Lock()
	if receipt.Sequence > actor.inputSequence {
		actor.inputSequence = receipt.Sequence
	}
	c.mu.Unlock()
	if receipt.State == daemonpkg.ReceiptQueued {
		_ = c.kickLaneInputCommandReplay(runtime, actor)
		acceptedSnapshot, readErr := runtime.State().Read()
		if readErr != nil {
			return nil, readErr
		}
		receipt = acceptedSnapshot.Catalog.LaneInputs[inputID]
		if receipt.Revision <= 1 {
			return nil, errors.New("lane input did not cross the accepted turn boundary")
		}
	} else if replay {
		_ = c.kickLaneInput(runtime, actor)
	} else {
		_ = c.dispatchCommittedLaneInput(runtime, actor, inputEngine, receipt, false, false)
	}
	if !wait {
		return laneResultWithReceipt(laneReadyResult(actor), receipt), nil
	}
	result, currentReceipt, err := c.waitLaneReceipt(ctx, runtime, actor, receipt.ReceiptID)
	if err != nil {
		return nil, err
	}
	return laneResultWithReceipt(result, currentReceipt), nil
}

//nolint:gocyclo // Resume revalidates ownership, native identity, permissions, and dispatch atomically.
func (c *hostCoordinator) resumeLane(ctx context.Context, runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand, input string, inputID string) (map[string]any, error) {
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
	inputEngine, err := c.laneInputEngine(runtime)
	if err != nil {
		return nil, err
	}
	before, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	if existing, replay := before.Catalog.LaneInputs[inputID]; replay {
		if existing.LaneID != actor.id {
			return nil, daemonpkg.ErrLaneInputConflict
		}
		receipt, admitErr := inputEngine.AdmitWithID(inputID, actor.id, []byte(input))
		if admitErr != nil {
			return nil, admitErr
		}
		if receipt.State == daemonpkg.ReceiptQueued {
			_ = c.kickLaneInputCommandReplay(runtime, actor)
			acceptedSnapshot, readErr := runtime.State().Read()
			if readErr != nil {
				return nil, readErr
			}
			receipt = acceptedSnapshot.Catalog.LaneInputs[inputID]
			if receipt.Revision <= 1 {
				return nil, errors.New("lane input did not cross the accepted turn boundary")
			}
		} else {
			_ = c.kickLaneInput(runtime, actor)
		}
		result, currentReceipt, waitErr := c.waitLaneReceipt(ctx, runtime, actor, receipt.ReceiptID)
		if waitErr != nil {
			return nil, waitErr
		}
		return laneResultWithReceipt(result, currentReceipt), nil
	}
	if err := c.resolveLegacyLaneInputRetryDebt(runtime, actor); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if debt, ok, readErr := oldestUncollectedLaneTurn(runtime, actor.id); readErr != nil {
		c.mu.Unlock()
		return nil, readErr
	} else if ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("collect outstanding %s lane turn %s before resume", product, debt.ID)
	}
	if actor.state == "running" {
		c.mu.Unlock()
		return nil, errors.New("collect or interrupt the active lane turn before resume")
	}
	priorActor := cloneLaneActor(actor)
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
			*actor = priorActor
			c.mu.Unlock()
			return nil, err
		}
	}
	actor.groups, err = c.effectiveLaneGroups(runtime, actor, parent)
	if err != nil {
		*actor = priorActor
		c.mu.Unlock()
		return nil, err
	}
	unarchive := priorActor.state == "archived"
	actor.state, actor.autoArchiveAt = "idle", 0
	prepareLaneTurnLocked(actor)
	actor.inputPump, actor.activeReceiptID = true, inputID
	c.mu.Unlock()
	receipt, err := inputEngine.UpdateLaneAdmitAndMarkDispatching(
		inputID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), commandRequestID(), []byte(input),
	)
	if err != nil {
		c.mu.Lock()
		if receipt.ReceiptID != "" && receipt.State == daemonpkg.ReceiptQueued {
			resetStagedLaneTurnLocked(actor, priorActor.state)
		} else {
			*actor = priorActor
		}
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	actor.inputSequence = receipt.Sequence
	c.mu.Unlock()
	_ = c.dispatchCommittedLaneInput(runtime, actor, inputEngine, receipt, true, unarchive)
	result, currentReceipt, err := c.waitLaneReceipt(ctx, runtime, actor, receipt.ReceiptID)
	if err != nil {
		return nil, err
	}
	return laneResultWithReceipt(result, currentReceipt), nil
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

	var turn daemonpkg.Turn
waitForCollectable:
	for {
		var (
			ok  bool
			err error
		)
		turn, ok, err = oldestUncollectedLaneTurn(runtime, actor.id)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		c.mu.Lock()
		state := actor.state
		c.mu.Unlock()
		switch state {
		case "idle":
			// Vendor MCP clients may serialize calls to one stdio server. An
			// empty wait must therefore fail immediately rather than occupying
			// the sole transport indefinitely and making every other Agent
			// Sessions tool appear dead.
			return nil, errors.New("idle lane has no collectable turn")
		case "archived":
			// Completion persists the terminal turn before it retires an
			// orphaned lane, but this loop may have read the catalog just before
			// that commit and then observe the newer process-local archived
			// state. Re-read after the state observation so an already-published
			// terminal turn cannot be mistaken for an empty archived lane.
			turn, err = archivedLaneCollectableTurn(runtime, actor.id)
			if err != nil {
				return nil, err
			}
			break waitForCollectable
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	result := durableLaneTurnResult(actor, turn)
	remainingDebt, err := c.collectLane(runtime, actor.id, turn.ID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if !remainingDebt && actor.state == "terminal" && actor.turnID == turn.ID {
		actor.state = "idle"
	}
	c.mu.Unlock()
	if !remainingDebt {
		c.armLaneAutoArchive(runtime, actor)
	}
	return result, nil
}

// waitLaneReceipt follows the receipt's current TargetTurnID instead of the
// lane collection cursor. Recovery may leave an older synthetic terminal audit
// while requeueing the same receipt onto a fresh turn; that audit must never be
// mistaken for the command caller's result.
func (c *hostCoordinator) waitLaneReceipt(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	receiptID string,
) (map[string]any, daemonpkg.LaneInputReceipt, error) {
	c.mu.Lock()
	if actor.collecting {
		c.mu.Unlock()
		return nil, daemonpkg.LaneInputReceipt{}, errors.New("lane already has an active collector")
	}
	actor.collecting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		actor.collecting = false
		c.mu.Unlock()
	}()

	for {
		snapshot, err := runtime.State().Read()
		if err != nil {
			return nil, daemonpkg.LaneInputReceipt{}, err
		}
		receipt, ok := snapshot.Catalog.LaneInputs[receiptID]
		if !ok || receipt.LaneID != actor.id {
			return nil, daemonpkg.LaneInputReceipt{}, errors.New("lane input receipt is missing or changed lane")
		}
		if receipt.TargetTurnID != "" {
			turn, turnOK := snapshot.Catalog.Turns[receipt.TargetTurnID]
			if !turnOK || turn.LaneID != actor.id {
				return nil, receipt, errors.New("lane input target turn is missing or changed lane")
			}
			switch turn.State {
			case "terminal":
				result := durableLaneTurnResult(actor, turn)
				remainingDebt, collectErr := c.collectLane(runtime, actor.id, turn.ID)
				if collectErr != nil {
					return nil, receipt, collectErr
				}
				c.mu.Lock()
				if !remainingDebt && actor.state == "terminal" && actor.turnID == turn.ID {
					actor.state = "idle"
				}
				c.mu.Unlock()
				if !remainingDebt {
					c.armLaneAutoArchive(runtime, actor)
				}
				return result, receipt, nil
			case "collected":
				return durableLaneTurnResult(actor, turn), receipt, nil
			}
		} else if receipt.State == daemonpkg.ReceiptRetired || receipt.State == daemonpkg.ReceiptAmbiguous {
			return nil, receipt, fmt.Errorf("lane input receipt became %s before an exact result was available", receipt.State)
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, receipt, ctx.Err()
		case <-timer.C:
		}
	}
}

func archivedLaneCollectableTurn(runtime *daemonpkg.Runtime, laneID string) (daemonpkg.Turn, error) {
	turn, ok, err := oldestUncollectedLaneTurn(runtime, laneID)
	if err != nil {
		return daemonpkg.Turn{}, err
	}
	if !ok {
		return daemonpkg.Turn{}, errors.New("archived lane has no collectable turn")
	}
	return turn, nil
}

func (c *hostCoordinator) statusLane(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	actor, err := c.resolveLaneActor(runtime, parent, product, options.target, options.all)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	result := laneActorStatus(actor)
	c.mu.Unlock()
	debt, _, err := oldestUncollectedLaneTurn(runtime, actor.id)
	if err != nil {
		return nil, err
	}
	result["collection_debt"] = debt.ID != ""
	return result, nil
}

func (c *hostCoordinator) listLanes(runtime *daemonpkg.Runtime, parent daemonpkg.ManagedAttachment, product string, options parsedLaneCommand) (map[string]any, error) {
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return nil, err
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	staged := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	c.mu.Lock()
	lanes := make([]map[string]any, 0)
	for _, actor := range c.lanes {
		durableState := snapshot.Catalog.Lanes[actor.id].State
		if staged[actor.id] || actor.product != product || options.mine && actor.parentID != parent.ID || !options.all && durableState == "archived" ||
			!groupsIntersect(parentGroups, actor.groups) && actor.parentID != parent.ID {
			continue
		}
		lanes = append(lanes, laneActorStatus(actor))
	}
	c.mu.Unlock()
	for _, lane := range lanes {
		id, _ := lane["thread_id"].(string)
		lane["collection_debt"] = catalogHasUncollectedLaneTurn(snapshot.Catalog, id)
	}
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
		cleanupErr := c.retireLaneInputs(runtime, actor.id, true)
		nativeErr := c.archiveNativeLaneTracked(runtime, actor)
		parentErr := c.retireParentLanes(runtime, actor.id)
		return result, errors.Join(cleanupErr, nativeErr, parentErr)
	}
	if actor.state == "cleanup-debt" {
		// Addressability was already withdrawn when the debt was recorded. Retry
		// every remaining cleanup operation without first overwriting the durable
		// cleanup-debt state that ResolveCleanupDebt must reconcile.
		result := map[string]any{
			"type": "lane.archived", "product": product, "thread_id": actor.id,
			"session_id": actor.nativeID, "name": actor.name,
		}
		c.mu.Unlock()
		cleanupErr := c.retireLaneInputs(runtime, actor.id, true)
		nativeErr := c.archiveNativeLaneTracked(runtime, actor)
		parentErr := c.retireParentLanes(runtime, actor.id)
		stateErr := requireLaneArchivedAfterCleanup(runtime, actor.id)
		return result, errors.Join(cleanupErr, nativeErr, parentErr, stateErr)
	}
	if actor.state == "running" || actor.state == "preparing" || actor.state == "interrupting" || actor.inputPump {
		c.mu.Unlock()
		return nil, errors.New("refuse to archive a lane with an active turn")
	}
	c.mu.Unlock()
	if err := c.commitLaneState(runtime, actor.id, "archived"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	actor.state = "archived"
	c.mu.Unlock()
	cleanupErr := c.retireLaneInputs(runtime, actor.id, true)
	nativeErr := c.archiveNativeLaneTracked(runtime, actor)
	parentErr := c.retireParentLanes(runtime, actor.id)
	return map[string]any{"type": "lane.archived", "product": product, "thread_id": actor.id, "session_id": actor.nativeID, "name": actor.name}, errors.Join(cleanupErr, nativeErr, parentErr)
}

// retireLaneInputs gives archive/retirement an explicit receipt disposition.
// Dispatching is made ambiguous because archive cannot prove whether native
// I/O occurred. Ambiguous receipts retain that evidence until an explicit
// ambiguity-resolution operation proves injection or abandons them; an
// automatic lane archive must never collapse unproven native I/O to Retired.
// Queued and injected receipts can retire directly. Cleanup failure remains
// durable debt and is returned to the caller.
func (c *hostCoordinator) retireLaneInputs(runtime *daemonpkg.Runtime, laneID string, includeDispatching bool) error {
	engine, err := c.laneInputEngine(runtime)
	if err != nil {
		return err
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	receipts := make([]daemonpkg.LaneInputReceipt, 0)
	for _, receipt := range snapshot.Catalog.LaneInputs {
		if receipt.LaneID == laneID {
			receipts = append(receipts, receipt)
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].Sequence < receipts[j].Sequence })
	var retirementErr error
	for _, receipt := range receipts {
		switch receipt.State {
		case daemonpkg.ReceiptDispatching:
			if !includeDispatching {
				continue
			}
			if _, err := engine.MarkAmbiguous(receipt.ReceiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven); err != nil {
				retirementErr = errors.Join(retirementErr, err)
			}
			continue
		case daemonpkg.ReceiptAmbiguous:
			continue
		case daemonpkg.ReceiptQueued, daemonpkg.ReceiptInjected:
			_, err = engine.Retire(receipt.ReceiptID)
		default:
			continue
		}
		retirementErr = errors.Join(retirementErr, err)
	}
	if includeDispatching {
		retirementErr = errors.Join(retirementErr, c.resolveRetiredLaneInputRetryDebt(runtime, laneID))
	}
	return retirementErr
}

func (c *hostCoordinator) deliverLaneMessageWithID(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	deliveryID, message string,
) (daemonpkg.LaneInputReceipt, error) {
	message = laneInboundPrompt(actor.product, message)
	engine, err := c.laneInputEngine(runtime)
	if err != nil {
		return daemonpkg.LaneInputReceipt{}, err
	}
	before, err := runtime.State().Read()
	if err != nil {
		return daemonpkg.LaneInputReceipt{}, err
	}
	if stagedUnacknowledgedLaneInputs(before.Catalog)[actor.id] {
		return daemonpkg.LaneInputReceipt{}, daemonpkg.ErrLaneInputUnavailable
	}
	_, replay := before.Catalog.LaneInputs[deliveryID]
	// Serialize receipt admission with lane lifecycle commits so a durable lane
	// projection can never regress the frozen InputSequence authority.
	c.laneInputCommitMu.Lock()
	receipt, err := engine.AdmitWithID(deliveryID, actor.id, []byte(message))
	c.laneInputCommitMu.Unlock()
	if err != nil {
		return daemonpkg.LaneInputReceipt{}, err
	}
	c.mu.Lock()
	if receipt.Sequence > actor.inputSequence {
		actor.inputSequence = receipt.Sequence
	}
	c.mu.Unlock()
	// Durable admission is the caller-visible acceptance boundary. Dispatch is
	// deliberately best-effort after this point: a failure is represented by the
	// receipt lifecycle and must not turn an accepted delivery into a retry that
	// creates competing native I/O.
	if replay {
		_ = c.kickLaneInput(runtime, actor)
	} else {
		_ = c.kickLaneInputExplicit(runtime, actor)
	}
	return receipt, nil
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

func (c *hostCoordinator) laneInputEngine(runtime *daemonpkg.Runtime) (*daemonpkg.LaneInputEngine, error) {
	c.mu.Lock()
	engine := c.laneInputs
	c.mu.Unlock()
	if engine != nil {
		return engine, nil
	}
	created, err := daemonpkg.NewLaneInputEngine(
		runtime.State(), filepath.Join(c.stateRoot, "lane-input-spool"), daemonpkg.DefaultLaneInputLimits(),
	)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.laneInputs == nil {
		c.laneInputs = created
	}
	engine = c.laneInputs
	c.mu.Unlock()
	return engine, nil
}

type lanePreNativeDispatchError struct{ cause error }

func (e lanePreNativeDispatchError) Error() string { return e.cause.Error() }
func (e lanePreNativeDispatchError) Unwrap() error { return e.cause }

func preNativeLaneDispatch(cause error) error {
	if cause == nil {
		return nil
	}
	return lanePreNativeDispatchError{cause: cause}
}

func isPreNativeLaneDispatch(cause error) bool {
	var target lanePreNativeDispatchError
	return errors.As(cause, &target)
}

// kickLaneInput claims one process-local pump for a lane. Durable receipt state,
// not this boolean, remains the acceptance and recovery authority.
func (c *hostCoordinator) kickLaneInput(runtime *daemonpkg.Runtime, actor *laneActor) error {
	return c.kickLaneInputMode(runtime, actor, false, false)
}

func (c *hostCoordinator) kickLaneInputExplicit(runtime *daemonpkg.Runtime, actor *laneActor) error {
	return c.kickLaneInputMode(runtime, actor, true, false)
}

func (c *hostCoordinator) kickLaneInputCommandReplay(runtime *daemonpkg.Runtime, actor *laneActor) error {
	return c.kickLaneInputMode(runtime, actor, true, true)
}

func (c *hostCoordinator) kickLaneInputMode(runtime *daemonpkg.Runtime, actor *laneActor, allowRetryCeiling, allowStaged bool) error {
	c.mu.Lock()
	if actor.inputPump {
		c.mu.Unlock()
		return nil
	}
	switch actor.state {
	case "running", "preparing", "interrupting", "retiring", "archived", "cleanup-debt":
		c.mu.Unlock()
		return nil
	}
	actor.inputPump = true
	c.mu.Unlock()

	engine, err := c.laneInputEngine(runtime)
	if err != nil {
		c.releaseLaneInputPump(actor, "")
		return err
	}
	stagedSnapshot, err := runtime.State().Read()
	if err != nil {
		c.releaseLaneInputPump(actor, "")
		return err
	}
	staged := stagedUnacknowledgedLaneInputs(stagedSnapshot.Catalog)[actor.id]
	if staged && !allowStaged {
		c.releaseLaneInputPump(actor, "")
		return nil
	}
	receipt, ok, err := engine.EarliestQueued(actor.id)
	if err != nil || !ok {
		c.releaseLaneInputPump(actor, "")
		return err
	}
	if laneInputDispatchAttempts(receipt) >= maxAutomaticLaneInputDispatchAttempts && !allowRetryCeiling {
		c.releaseLaneInputPump(actor, "")
		return nil
	}
	dispatchSnapshot, err := runtime.State().Read()
	if err != nil {
		c.releaseLaneInputPump(actor, "")
		return err
	}
	resumeNative, ledgerCreatedLane := false, false
	for _, candidate := range dispatchSnapshot.Catalog.LaneInputs {
		if candidate.LaneID != actor.id {
			continue
		}
		if candidate.Sequence == 1 && laneIDForInitialReceipt(candidate.ReceiptID) == actor.id {
			ledgerCreatedLane = true
		}
		if candidate.NativeAcceptance != nil {
			resumeNative = true
			break
		}
	}
	if !resumeNative && !ledgerCreatedLane {
		for _, turn := range dispatchSnapshot.Catalog.Turns {
			if turn.LaneID == actor.id {
				resumeNative = true
				break
			}
		}
	}
	unarchiveNative := dispatchSnapshot.Catalog.Lanes[actor.id].ArchiveRevision > 0

	c.mu.Lock()
	if actor.state == "running" || actor.state == "preparing" || actor.state == "interrupting" ||
		actor.state == "retiring" || actor.state == "archived" || actor.state == "cleanup-debt" {
		actor.inputPump = false
		c.mu.Unlock()
		return nil
	}
	priorState := actor.state
	prepareLaneTurnLocked(actor)
	actor.activeReceiptID = receipt.ReceiptID
	c.mu.Unlock()

	// The turn and its receipt dispatch intent share one StateStore commit.
	// Queued input intentionally bypasses older terminal collection debt;
	// collection and input retirement are independent cursors.
	attemptID := commandRequestID()
	c.laneInputCommitMu.Lock()
	var dispatching daemonpkg.LaneInputReceipt
	if staged {
		dispatching, err = engine.AcceptStagedTurnAndMarkDispatching(
			receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), attemptID,
		)
	} else {
		dispatching, err = engine.AcceptTurnAndMarkDispatching(
			receipt.ReceiptID, durableLane(actor, "idle"), durableTurn(actor, 0, "accepted"), attemptID,
		)
	}
	c.laneInputCommitMu.Unlock()
	if err != nil {
		c.mu.Lock()
		actor.state, actor.activeReceiptID, actor.inputPump = priorState, "", false
		c.mu.Unlock()
		return err
	}
	return c.dispatchCommittedLaneInput(runtime, actor, engine, dispatching, resumeNative, unarchiveNative)
}

func (c *hostCoordinator) dispatchCommittedLaneInput(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	engine *daemonpkg.LaneInputEngine,
	receipt daemonpkg.LaneInputReceipt,
	resumeNative, unarchiveNative bool,
) error {
	reader, metadata, err := engine.OpenVerified(receipt.ReceiptID)
	if err != nil {
		return c.failVerifiedLaneInput(runtime, actor, engine, receipt.ReceiptID, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, metadata.Bytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) != metadata.Bytes {
		return c.failVerifiedLaneInput(runtime, actor, engine, receipt.ReceiptID, errors.Join(readErr, closeErr, errors.New("verified lane input length changed")))
	}
	if err := c.dispatchLaneTurn(runtime, actor, string(body), resumeNative, unarchiveNative); err != nil {
		if snapshot, readErr := runtime.State().Read(); readErr == nil &&
			snapshot.Catalog.LaneInputs[receipt.ReceiptID].State == daemonpkg.ReceiptInjected {
			_, _ = engine.Retire(receipt.ReceiptID)
			c.releaseLaneInputPump(actor, receipt.ReceiptID)
			_ = c.kickLaneInput(runtime, actor)
			return err
		}
		if isPreNativeLaneDispatch(err) {
			requeued, requeueErr := engine.RequeueProvenNotInjected(receipt.ReceiptID)
			c.releaseLaneInputPump(actor, receipt.ReceiptID)
			if requeueErr == nil {
				if laneInputDispatchAttempts(requeued) < maxAutomaticLaneInputDispatchAttempts {
					c.scheduleLaneInputRetry(runtime, actor, requeued)
				}
			}
		} else {
			_, _ = engine.MarkAmbiguous(receipt.ReceiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven)
			c.releaseLaneInputPump(actor, receipt.ReceiptID)
			_ = c.kickLaneInput(runtime, actor)
		}
		return err
	}
	return nil
}

// scheduleLaneInputRetry gives proven-pre-I/O failures a deterministic wake
// without converting a permanent local configuration error into a busy loop.
// The receipt revision is durable, so a daemon restart cannot reset the bound.
func (c *hostCoordinator) scheduleLaneInputRetry(runtime *daemonpkg.Runtime, actor *laneActor, receipt daemonpkg.LaneInputReceipt) {
	attempts := laneInputDispatchAttempts(receipt)
	if attempts >= maxAutomaticLaneInputDispatchAttempts {
		return
	}
	delay := time.Duration(attempts+1) * 100 * time.Millisecond
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			_ = c.kickLaneInput(runtime, actor)
		}
	}()
}

const maxAutomaticLaneInputDispatchAttempts = uint64(3)

func laneInputDispatchAttempts(receipt daemonpkg.LaneInputReceipt) uint64 {
	if receipt.Revision <= 1 {
		return 0
	}
	return (receipt.Revision - 1) / 2
}

func (c *hostCoordinator) resolveLegacyLaneInputRetryDebt(runtime *daemonpkg.Runtime, actor *laneActor) error {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	if snapshot.Catalog.Lanes[actor.id].State != "cleanup-debt" {
		return nil
	}
	for receiptID, receipt := range snapshot.Catalog.LaneInputs {
		debtID := "lane-input-retry-" + receiptID
		if receipt.LaneID != actor.id || receipt.State != daemonpkg.ReceiptQueued {
			continue
		}
		if _, ok := snapshot.Catalog.CleanupDebts[debtID]; !ok {
			continue
		}
		engine, engineErr := daemonpkg.NewLaneEngine(runtime.State())
		if engineErr != nil {
			return engineErr
		}
		if engineErr = engine.ResolveCleanupDebt(actor.id, debtID, "terminal"); engineErr != nil {
			return engineErr
		}
		c.mu.Lock()
		actor.state = "terminal"
		c.mu.Unlock()
		return nil
	}
	return errors.New("lane cleanup debt is not an exact retry-ceiling receipt")
}

func (c *hostCoordinator) resolveRetiredLaneInputRetryDebt(runtime *daemonpkg.Runtime, laneID string) error {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	if snapshot.Catalog.Lanes[laneID].State != "cleanup-debt" {
		return nil
	}
	for receiptID, receipt := range snapshot.Catalog.LaneInputs {
		debtID := "lane-input-retry-" + receiptID
		if receipt.LaneID != laneID || receipt.State != daemonpkg.ReceiptRetired {
			continue
		}
		if _, ok := snapshot.Catalog.CleanupDebts[debtID]; !ok {
			continue
		}
		engine, engineErr := daemonpkg.NewLaneEngine(runtime.State())
		if engineErr != nil {
			return engineErr
		}
		return engine.ResolveCleanupDebt(laneID, debtID, "archived")
	}
	return nil
}

func (c *hostCoordinator) failVerifiedLaneInput(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	engine *daemonpkg.LaneInputEngine,
	receiptID string,
	cause error,
) error {
	_, _ = engine.RequeueProvenNotInjected(receiptID)
	// Recovery records exact-object cleanup debt for a changed body. It never
	// opens or removes a replacement object by name alone.
	_, _ = engine.Recover()
	_ = c.failLaneDispatch(runtime, actor, cause)
	c.releaseLaneInputPump(actor, receiptID)
	return cause
}

func (c *hostCoordinator) releaseLaneInputPump(actor *laneActor, receiptID string) {
	c.mu.Lock()
	if receiptID == "" || actor.activeReceiptID == receiptID {
		actor.activeReceiptID = ""
		actor.inputPump = false
	}
	c.mu.Unlock()
}

func (c *hostCoordinator) reconcileOrphanedLanes(runtime *daemonpkg.Runtime) error {
	if err := c.ensureLaneActors(runtime); err != nil {
		return err
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	stagedInputs := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	c.mu.Lock()
	candidates := make([]*laneActor, 0)
	for _, actor := range c.lanes {
		if !actor.persistent && (actor.state != "archived" || stagedInputs[actor.id]) {
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
		stagedInput                  bool
		alreadyArchived              bool
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	stagedInputs := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	c.mu.Lock()
	candidates := make([]transition, 0)
	for _, actor := range c.lanes {
		stagedInput := stagedInputs[actor.id]
		if actor.parentID != parentID || actor.persistent || actor.state == "archived" && !stagedInput {
			continue
		}
		alreadyArchived := actor.state == "archived"
		state := "archived"
		var cancel context.CancelFunc
		if stagedInput && actor.state == "preparing" {
			state = "retiring"
		} else if (actor.state == "running" || actor.state == "preparing" || actor.state == "interrupting" ||
			actor.state == "retiring" || actor.state == "cleanup-debt") && actor.cancel != nil {
			state, cancel = "retiring", actor.cancel
			actor.interruptRequested = true
		}
		actor.state = state
		candidates = append(candidates, transition{
			actor: actor, state: state, product: actor.product,
			thread: actor.nativeID, turn: actor.nativeTurnID, cancel: cancel, stagedInput: stagedInput,
			alreadyArchived: alreadyArchived,
		})
	}
	c.mu.Unlock()
	var retirementErr error
	for _, candidate := range candidates {
		if candidate.stagedInput {
			inputEngine, engineErr := c.laneInputEngine(runtime)
			if engineErr != nil {
				retirementErr = errors.Join(retirementErr, engineErr)
				continue
			}
			stagedSnapshot, readErr := runtime.State().Read()
			if readErr != nil {
				retirementErr = errors.Join(retirementErr, readErr)
				continue
			}
			retired := false
			for _, receipt := range stagedSnapshot.Catalog.LaneInputs {
				if receipt.LaneID != candidate.actor.id || receipt.State != daemonpkg.ReceiptQueued ||
					receipt.Revision != 1 || !strings.HasPrefix(receipt.ReceiptID, daemonpkg.LaneCommandReceiptPrefix) {
					continue
				}
				if _, retireErr := inputEngine.RetireStagedLane(receipt.ReceiptID); retireErr != nil {
					retirementErr = errors.Join(retirementErr, retireErr)
				} else {
					retired = true
				}
				break
			}
			if retired {
				c.mu.Lock()
				candidate.actor.state, candidate.actor.inputPump, candidate.actor.activeReceiptID = "archived", false, ""
				c.mu.Unlock()
			}
			continue
		}
		if !candidate.alreadyArchived {
			if err := c.commitLaneState(runtime, candidate.actor.id, candidate.state); err != nil {
				retirementErr = errors.Join(retirementErr, err)
				continue
			}
		}
		if err := c.retireLaneInputs(runtime, candidate.actor.id, candidate.state == "archived" || candidate.stagedInput); err != nil {
			retirementErr = errors.Join(retirementErr, err)
			continue
		}
		if candidate.state == "archived" {
			if err := c.archiveNativeLaneTracked(runtime, candidate.actor); err != nil {
				retirementErr = errors.Join(retirementErr, err)
			}
		}
		if candidate.cancel != nil {
			if candidate.product == "codex" {
				native, err := c.codexNative()
				if err != nil {
					retirementErr = errors.Join(retirementErr, err)
				} else {
					interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 10*time.Second)
					err = native.InterruptLaneTurn(interruptCtx, candidate.thread, candidate.turn)
					interruptCancel()
					retirementErr = errors.Join(retirementErr, err)
					if err != nil {
						retirementErr = errors.Join(retirementErr, c.recordNativeLaneCleanupDebt(runtime, candidate.actor))
					}
				}
			}
			candidate.cancel()
		}
	}
	return retirementErr
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
	}
	c.mu.Lock()
	actor.capability = capability
	c.mu.Unlock()
	if err := c.commitLaneAuthorization(runtime, actor, "preparing"); err != nil {
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
	}
	thread, err := adapter.Prepare(c.ctx, daemonpkg.CodexLaneRequest{
		LaneID: actor.id, NativeSession: actor.nativeID, Cwd: actor.cwd, Name: actor.name,
		PermissionMode: actor.permission, ApprovalPolicy: actor.approvalPolicy, Sandbox: actor.sandbox,
		Resume: resume, Unarchive: unarchive,
	})
	if err != nil {
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
	}
	if err := c.recordLaneNativeID(runtime, actor, thread.ID); err != nil {
		if !resume {
			_ = adapter.Archive(context.Background(), thread.ID)
		}
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
	// turn/start returned an exact thread+turn identity. This is the native
	// acceptance boundary, independent of whether the turn later succeeds.
	if err := c.markActiveLaneInputInjected(runtime, actor, thread.ID, nativeTurnID); err != nil {
		return c.failLaneDispatch(runtime, actor, err)
	}
	turnCtx, cancel := laneTurnContext(c.ctx, actor.turnTimeout)
	if err := c.beginLaneExecution(runtime, actor, cancel); err != nil {
		cancel()
		if !resume {
			_ = adapter.Archive(context.Background(), thread.ID)
		}
		return c.failLaneDispatch(runtime, actor, err)
	}
	c.persistCodexNativeTurnID(runtime, actor)
	go c.watchCodexLaneTurn(runtime, actor, adapter, thread.ID, nativeTurnID, turnCtx, cancel)
	return nil
}

func (c *hostCoordinator) persistCodexNativeTurnID(runtime *daemonpkg.Runtime, actor *laneActor) {
	c.mu.Lock()
	turnID, nativeTurnID, receiptID := actor.turnID, actor.nativeTurnID, actor.activeReceiptID
	c.mu.Unlock()
	if receiptID != "" {
		// Receipt-backed Codex dispatch committed this anchor atomically with
		// NativeAcceptanceRef at the exact StartTurn acknowledgement.
		return
	}
	engine, engineErr := daemonpkg.NewLaneEngine(runtime.State())
	if engineErr != nil {
		return
	}
	if err := engine.SetNativeDispatchID(turnID, nativeTurnID); err != nil {
		// The App Server already accepted the exact turn. Recovery can still bind
		// the newest native turn through the durable thread ID after a restart.
		c.mu.Lock()
		if actor.failure == "" {
			actor.failure = fmt.Sprintf("persist native Codex turn id: %v", err)
		}
		c.mu.Unlock()
	}
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
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
	}
	hostExecutable, err := os.Executable()
	if err != nil {
		return c.failLaneDispatch(runtime, actor, preNativeLaneDispatch(err))
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
			nativeID: result.NativeSessionID, replaceNativeID: true, failed: runErr != nil,
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
	// Claim native dispatch while still holding the coordinator lock. Every
	// concurrent delivery now queues instead of starting a second writer in the
	// gap between durable acceptance and product dispatch.
	actor.state = "preparing"
}

func resetStagedLaneTurnLocked(actor *laneActor, state string) {
	if state == "" {
		state = "preparing"
	}
	select {
	case <-actor.done:
	default:
		close(actor.done)
	}
	actor.state, actor.turnID, actor.nativeTurnID = state, "", ""
	actor.outcome, actor.result, actor.failure = "", "", ""
	actor.startedAt, actor.completedAt, actor.deadlineAt = 0, 0, 0
	actor.cancel, actor.interruptRequested = nil, false
	actor.inputPump, actor.activeReceiptID = false, ""
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
	replaceNativeID                    bool
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
	if completion.nativeID != "" && (actor.nativeID == "" || completion.replaceNativeID) {
		actor.nativeID = completion.nativeID
	}
	nativeMessageID := actor.nativeTurnID
	actor.state, actor.outcome, actor.result = "terminal", completion.outcome, completion.result
	actor.failure, actor.completedAt, actor.cancel = completion.failure, time.Now().UnixMilli(), nil
	if completion.clearNativeTurn {
		actor.nativeTurnID = ""
	}
	receiptID := actor.activeReceiptID
	nativeSessionID := actor.nativeID
	actor.capability = ""
	if receiptID != "" {
		if diagnostic := c.finalizeLaneInput(runtime, receiptID, nativeSessionID, nativeMessageID, actor.outcome); diagnostic != "" {
			actor.outcome = "failed"
			if actor.failure == "" {
				actor.failure = diagnostic
			} else {
				actor.failure += "; " + diagnostic
			}
		}
	}
	if persistErr := c.markLaneTerminal(runtime, actor); persistErr != nil {
		actor.outcome = "failed"
		actor.failure = fmt.Sprintf("persist terminal lane state: %v", persistErr)
	}
	terminal := cloneLaneActor(actor)
	close(dispatchedDone)
	c.mu.Unlock()
	c.releaseLaneInputPump(actor, receiptID)
	c.queueLaneTerminalNotice(runtime, &terminal)
	archived := c.archiveOrphanedCompletedLane(runtime, actor)
	if archived {
		return
	}
	_ = c.kickLaneInput(runtime, actor)
}

func (c *hostCoordinator) finalizeLaneInput(
	runtime *daemonpkg.Runtime,
	receiptID, nativeSessionID, nativeMessageID, outcome string,
) string {
	// Completion calls this while holding c.mu so the diagnostic is included in
	// the same terminal/notice projection. Receipt-backed dispatch always
	// initialized this engine at admission.
	engine := c.laneInputs
	if engine == nil {
		return "finalize lane input receipt: durable engine unavailable"
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return "finalize lane input receipt: durable state unavailable"
	}
	receipt, ok := snapshot.Catalog.LaneInputs[receiptID]
	if !ok || receipt.State == daemonpkg.ReceiptRetired || receipt.State == daemonpkg.ReceiptAmbiguous {
		return ""
	}
	if receipt.State == daemonpkg.ReceiptInjected {
		if _, err := engine.Retire(receiptID); err != nil {
			return "retire verified lane input: cleanup debt"
		}
		return ""
	}
	// Successful terminal evidence proves that a non-Codex adapter accepted
	// this prompt. Every failed/timed-out/interrupted path remains ambiguous;
	// a native session identifier alone is never proof of prompt acceptance.
	if receipt.State != daemonpkg.ReceiptDispatching || outcome != "completed" || nativeSessionID == "" {
		if receipt.State == daemonpkg.ReceiptDispatching {
			_, _ = engine.MarkAmbiguous(receiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven)
		}
		return ""
	}
	acceptedAt := time.Now().Unix()
	if acceptedAt <= 0 {
		acceptedAt = 1
	}
	if _, err := engine.MarkInjected(receiptID, daemonpkg.NativeAcceptanceRef{
		NativeSessionID: nativeSessionID, NativeMessageID: nativeMessageID, AcceptedAt: acceptedAt,
	}); err != nil {
		if _, ambiguityErr := engine.MarkAmbiguous(receiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven); ambiguityErr != nil {
			return "lane input native acceptance commit failed; ambiguity evidence also requires reconciliation"
		}
		return "lane input native acceptance commit failed; receipt retained as ambiguous"
	}
	if _, err := engine.Retire(receiptID); err != nil {
		return "retire verified lane input: cleanup debt"
	}
	return ""
}

func (c *hostCoordinator) markActiveLaneInputInjected(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	nativeSessionID, nativeMessageID string,
) error {
	c.mu.Lock()
	receiptID := actor.activeReceiptID
	c.mu.Unlock()
	if receiptID == "" {
		return nil
	}
	engine, err := c.laneInputEngine(runtime)
	if err != nil {
		return err
	}
	acceptedAt := time.Now().Unix()
	if acceptedAt <= 0 {
		acceptedAt = 1
	}
	_, err = engine.MarkInjectedAndSetNativeDispatch(receiptID, daemonpkg.NativeAcceptanceRef{
		NativeSessionID: nativeSessionID, NativeMessageID: nativeMessageID, AcceptedAt: acceptedAt,
	})
	return err
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
	if c.commitLaneState(runtime, actor.id, "archived") != nil {
		return false
	}
	cleanupErr := c.retireLaneInputs(runtime, actor.id, true)
	nativeErr := c.archiveNativeLaneTracked(runtime, actor)
	return cleanupErr == nil && nativeErr == nil
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

func (c *hostCoordinator) archiveNativeLaneTracked(runtime *daemonpkg.Runtime, actor *laneActor) error {
	archiveErr := c.archiveNativeLane(actor)
	if archiveErr == nil {
		debtID := "lane-native-archive-" + actor.id
		snapshot, readErr := runtime.State().Read()
		if readErr != nil {
			return readErr
		}
		if _, exists := snapshot.Catalog.CleanupDebts[debtID]; !exists {
			return nil
		}
		engine, engineErr := daemonpkg.NewLaneEngine(runtime.State())
		if engineErr != nil {
			return engineErr
		}
		// The archive acknowledgement is the live absence confirmation that
		// permits this exact debt to resolve. Reassert cleanup-debt first because
		// a concurrent terminal projection may have advanced the lane state while
		// preserving the still-unresolved debt record.
		if engineErr = engine.RecordCleanupDebt(actor.id, snapshot.Catalog.CleanupDebts[debtID]); engineErr != nil {
			return engineErr
		}
		if engineErr = engine.ResolveCleanupDebt(actor.id, debtID, "archived"); engineErr != nil {
			return engineErr
		}
		c.mu.Lock()
		actor.state = "archived"
		c.mu.Unlock()
		return nil
	}
	engineErr := c.recordNativeLaneCleanupDebt(runtime, actor)
	return errors.Join(archiveErr, engineErr)
}

func (c *hostCoordinator) recordNativeLaneCleanupDebt(runtime *daemonpkg.Runtime, actor *laneActor) error {
	debtID := "lane-native-archive-" + actor.id
	retryRevision := uint64(1)
	if snapshot, readErr := runtime.State().Read(); readErr == nil {
		retryRevision = snapshot.Catalog.CleanupDebts[debtID].RetryRevision + 1
	}
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	err = engine.RecordCleanupDebt(actor.id, daemonpkg.CleanupDebt{
		ID: debtID, Resource: actor.product + "-native-session:" + actor.nativeID,
		BaselineIdentity: actor.nativeID, IntendedState: "archived-and-absent",
		LastVerifiedState: "native-absence-unconfirmed", Cause: "native-cleanup-unconfirmed",
		RetryRevision: retryRevision, Operation: "archive-native-lane",
	})
	if err == nil {
		c.mu.Lock()
		actor.state = "cleanup-debt"
		c.mu.Unlock()
	}
	return err
}

func requireLaneArchivedAfterCleanup(runtime *daemonpkg.Runtime, laneID string) error {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	lane, ok := snapshot.Catalog.Lanes[laneID]
	if !ok {
		return errors.New("lane state is missing after cleanup retry")
	}
	if lane.State != "archived" {
		return errors.New("lane cleanup debt remains unresolved")
	}
	return nil
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
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	staged := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	c.mu.Lock()
	defer c.mu.Unlock()
	var matches []*laneActor
	for _, actor := range c.lanes {
		durableState := snapshot.Catalog.Lanes[actor.id].State
		if staged[actor.id] || actor.product != product || actor.id != target && actor.name != target || !all && durableState == "archived" ||
			actor.parentID != parent.ID && !groupsIntersect(parentGroups, actor.groups) {
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
	return c.liveLaneNameExceptLocked(runtime, parent, name, "")
}

// liveLaneNameExceptLocked revalidates a staged lane immediately before CAS2.
// The exact staged lane is excluded, while every accepted durable lane and
// every concurrent process-local reservation remains name authority.
func (c *hostCoordinator) liveLaneNameExceptLocked(
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	name, excludeLaneID string,
) bool {
	parentGroups, err := c.attachmentVisibilityGroups(runtime, parent)
	if err != nil {
		return true
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return true
	}
	staged := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	for id, lane := range snapshot.Catalog.Lanes {
		if id == excludeLaneID || lane.State == "archived" {
			continue
		}
		promoting := c.lanes[id] != nil && c.lanes[id].namePromoting
		if staged[id] && !promoting {
			continue
		}
		if lane.Name == name && (lane.ParentAttachmentID == parent.ID || groupsIntersect(lane.Groups, parentGroups)) {
			return true
		}
	}
	for _, a := range c.lanes {
		if a.id == excludeLaneID {
			continue
		}
		if _, durable := snapshot.Catalog.Lanes[a.id]; durable {
			continue
		}
		if (!staged[a.id] || a.namePromoting) &&
			a.name == name && (a.parentID == parent.ID || groupsIntersect(a.groups, parentGroups)) {
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
		"owner_session_id": a.parentID, "persistent": a.persistent, "collection_debt": a.state == "terminal",
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

func durableLaneTurnResult(a *laneActor, turn daemonpkg.Turn) map[string]any {
	return map[string]any{
		"type": "turn.completed", "product": a.product, "thread_id": a.id, "session_id": a.nativeID,
		"turn_id": turn.ID, "status": turn.Outcome, "outcome": turn.Outcome, "exit": laneOutcomeExit(turn.Outcome),
		"result": turn.Result, "diagnostic": turn.Diagnostic,
	}
}

func laneResultWithReceipt(result map[string]any, receipt daemonpkg.LaneInputReceipt) map[string]any {
	result["receipt_id"] = receipt.ReceiptID
	result["receipt_sequence"] = receipt.Sequence
	return result
}

func durableReceiptTurnResult(
	runtime *daemonpkg.Runtime,
	actor *laneActor,
	receipt daemonpkg.LaneInputReceipt,
) (map[string]any, bool, error) {
	if receipt.TargetTurnID == "" {
		return nil, false, nil
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, false, err
	}
	turn, ok := snapshot.Catalog.Turns[receipt.TargetTurnID]
	if !ok || turn.LaneID != receipt.LaneID || turn.State != "terminal" && turn.State != "collected" {
		return nil, false, nil
	}
	return durableLaneTurnResult(actor, turn), true, nil
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

// stagedUnacknowledgedLaneInputs identifies the internal first commit of a
// start/resume command. Command receipt IDs occupy a coordinator-owned opaque
// namespace, so the frozen receipt schema needs no new discriminator. Revision
// one is the queued, not-yet-acknowledgeable phase; a requeue after a proven
// pre-native failure has a later revision and remains ordinary accepted work.
func stagedUnacknowledgedLaneInputs(catalog daemonpkg.Catalog) map[string]bool {
	staged := make(map[string]bool)
	for _, receipt := range catalog.LaneInputs {
		if receipt.State == daemonpkg.ReceiptQueued && receipt.Revision == 1 && strings.HasPrefix(receipt.ReceiptID, daemonpkg.LaneCommandReceiptPrefix) {
			staged[receipt.LaneID] = true
		}
	}
	return staged
}

//nolint:gocyclo // Recovery classifies and reconstructs every durable lane state explicitly.
func (c *hostCoordinator) ensureLaneActors(runtime *daemonpkg.Runtime) error {
	c.mu.Lock()
	if c.lanesLoaded {
		c.mu.Unlock()
		return c.startRecoveredLaneWork(runtime)
	}
	c.mu.Unlock()

	inputEngine, err := c.laneInputEngine(runtime)
	if err != nil {
		return err
	}
	// Receipt/object recovery is the first readiness gate. In particular, no
	// native watcher may resume while a dispatching receipt is still unclassified.
	_, err = inputEngine.Recover()
	if err != nil {
		return err
	}
	before, err := runtime.State().Read()
	if err != nil {
		return err
	}
	// Recover intentionally omits unverified dispatching objects from its live
	// report. Classify from durable receipt metadata so missing/changed objects
	// can never remain silently detached in Dispatching.
	dispatching := make([]daemonpkg.LaneInputReceipt, 0)
	dispatchingCount := make(map[string]int)
	activeReceiptCount := make(map[string]int)
	latestTurn := make(map[string]daemonpkg.Turn)
	for _, turn := range before.Catalog.Turns {
		if current, ok := latestTurn[turn.LaneID]; !ok || turn.Sequence > current.Sequence {
			latestTurn[turn.LaneID] = turn
		}
	}
	for _, receipt := range before.Catalog.LaneInputs {
		if receipt.State == daemonpkg.ReceiptDispatching {
			dispatching = append(dispatching, receipt)
			dispatchingCount[receipt.LaneID]++
		}
		if receipt.State == daemonpkg.ReceiptDispatching || receipt.State == daemonpkg.ReceiptInjected {
			activeReceiptCount[receipt.LaneID]++
		}
	}
	stagedInputs := stagedUnacknowledgedLaneInputs(before.Catalog)
	sort.Slice(dispatching, func(i, j int) bool {
		if dispatching[i].LaneID == dispatching[j].LaneID {
			return dispatching[i].Sequence < dispatching[j].Sequence
		}
		return dispatching[i].LaneID < dispatching[j].LaneID
	})
	activeReceiptByLane := make(map[string]daemonpkg.LaneInputReceipt)
	engine, err := daemonpkg.NewLaneEngine(runtime.State())
	if err != nil {
		return err
	}
	remainingDispatching := dispatching[:0]
	for _, receipt := range dispatching {
		lane := before.Catalog.Lanes[receipt.LaneID]
		turn := latestTurn[receipt.LaneID]
		provenPreNative := dispatchingCount[receipt.LaneID] == 1 && activeReceiptCount[receipt.LaneID] == 1 &&
			receipt.TargetTurnID == turn.ID && turn.LaneID == receipt.LaneID &&
			lane.State == "idle" && turn.State == "accepted"
		if !provenPreNative {
			remainingDispatching = append(remainingDispatching, receipt)
			continue
		}
		if _, recoverErr := inputEngine.RecoverAcceptedTurnAndRequeue(
			receipt.ReceiptID, "Agent Sessions daemon restarted before native lane I/O",
		); recoverErr != nil {
			return recoverErr
		}
		activeReceiptCount[receipt.LaneID]--
	}
	dispatching = remainingDispatching
	if err := engine.ReconcileRestart(func(lane daemonpkg.Lane, turn daemonpkg.Turn) bool {
		if stagedInputs[lane.ID] {
			return true
		}
		// A Codex watcher resumes only an exact durable native turn. Lanes that
		// originated outside the receipt path retain the same strict criterion.
		return laneRestartIsRecoverable(lane, turn)
	}, "Agent Sessions daemon restarted during the accepted turn"); err != nil {
		return err
	}
	for _, receipt := range dispatching {
		lane := before.Catalog.Lanes[receipt.LaneID]
		turn := latestTurn[receipt.LaneID]
		exactRecoverable := dispatchingCount[receipt.LaneID] == 1 && activeReceiptCount[receipt.LaneID] == 1 &&
			receipt.TargetTurnID == turn.ID &&
			turn.LaneID == receipt.LaneID && laneRestartIsRecoverable(lane, turn)
		if exactRecoverable {
			activeReceiptByLane[receipt.LaneID] = receipt
			continue
		}
		if _, markErr := inputEngine.MarkAmbiguous(receipt.ReceiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven); markErr != nil {
			return markErr
		}
	}
	for _, receipt := range before.Catalog.LaneInputs {
		if receipt.State != daemonpkg.ReceiptInjected || activeReceiptCount[receipt.LaneID] != 1 {
			continue
		}
		lane := before.Catalog.Lanes[receipt.LaneID]
		turn := latestTurn[receipt.LaneID]
		if receipt.TargetTurnID == turn.ID && turn.LaneID == receipt.LaneID && laneRestartIsRecoverable(lane, turn) {
			activeReceiptByLane[receipt.LaneID] = receipt
		}
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	hydrated := make(map[string]*laneActor, len(snapshot.Catalog.Lanes))
	for id, lane := range snapshot.Catalog.Lanes {
		done := make(chan struct{})
		name := lane.Name
		if name == "" {
			name = id
		}
		latest := daemonpkg.Turn{}
		for _, turn := range snapshot.Catalog.Turns {
			if turn.LaneID == id && turn.Sequence >= latest.Sequence {
				latest = turn
			}
		}
		delay := time.Duration(lane.AutoArchiveDelayMS) * time.Millisecond
		if delay <= 0 {
			delay = defaultUnifiedLaneAutoArchiveDelay
		}
		active := lane.Product == "codex" && (lane.State == "preparing" || lane.State == "running") &&
			(latest.State == "accepted" || latest.State == "dispatched")
		if !active {
			close(done)
		}
		actor := &laneActor{
			id: id, product: lane.Product, name: name, cwd: lane.Cwd,
			parentID: lane.ParentAttachmentID, nativeID: lane.NativeSessionID,
			groups: append([]string(nil), lane.Groups...), explicitGroups: append([]string(nil), lane.ExplicitGroups...), inheritGroups: lane.InheritGroups,
			permission: lane.PermissionMode, approvalPolicy: lane.ApprovalPolicy, sandbox: lane.Sandbox, effort: lane.Effort, schema: lane.Schema,
			arguments: append([]string(nil), lane.Arguments...), persistent: lane.Persistent, autoArchive: lane.AutoArchive,
			autoArchiveDelay: delay, autoArchiveAt: lane.AutoArchiveAt, state: lane.State, done: done,
			turnID: latest.ID, nativeTurnID: latest.NativeDispatchID,
			outcome: latest.Outcome, result: latest.Result, failure: latest.Diagnostic,
			startedAt: latest.StartedAt, completedAt: latest.CompletedAt, deadlineAt: latest.DeadlineAt, inputSequence: lane.InputSequence,
		}
		if receipt, ok := activeReceiptByLane[id]; ok && receipt.TargetTurnID == latest.ID {
			actor.inputPump, actor.activeReceiptID = true, receipt.ReceiptID
		}
		hydrated[id] = actor
	}
	c.mu.Lock()
	if !c.lanesLoaded {
		for id, actor := range hydrated {
			if _, exists := c.lanes[id]; !exists {
				c.lanes[id] = actor
			}
		}
		c.lanesLoaded = true
	}
	c.mu.Unlock()
	return c.startRecoveredLaneWork(runtime)
}

func (c *hostCoordinator) startRecoveredLaneWork(runtime *daemonpkg.Runtime) error {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return err
	}
	staged := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	c.mu.Lock()
	if c.ownerReconciling || c.laneRecoveryStarted {
		c.mu.Unlock()
		return nil
	}
	c.laneRecoveryStarted = true
	actors := make([]*laneActor, 0, len(c.lanes))
	for _, actor := range c.lanes {
		actors = append(actors, actor)
	}
	c.mu.Unlock()
	for _, actor := range actors {
		if staged[actor.id] {
			c.scheduleStagedLaneRetirement(runtime, actor)
			continue
		}
		if actor.product == "codex" && (actor.state == "preparing" || actor.state == "running") {
			c.recoverCodexLaneTurn(runtime, actor)
		} else {
			_ = c.kickLaneInput(runtime, actor)
			c.scheduleLaneAutoArchive(runtime, actor)
		}
	}
	return nil
}

func (c *hostCoordinator) scheduleStagedLaneRetirement(runtime *daemonpkg.Runtime, actor *laneActor) {
	snapshot, err := runtime.State().Read()
	if err != nil {
		return
	}
	var staged daemonpkg.LaneInputReceipt
	for _, receipt := range snapshot.Catalog.LaneInputs {
		if receipt.LaneID == actor.id && receipt.State == daemonpkg.ReceiptQueued && receipt.Revision == 1 &&
			strings.HasPrefix(receipt.ReceiptID, daemonpkg.LaneCommandReceiptPrefix) {
			staged = receipt
			break
		}
	}
	if staged.ReceiptID == "" {
		return
	}
	c.mu.Lock()
	now, timeout := c.now, c.laneInputStagingTimeout
	c.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	due := time.Unix(staged.AcceptedAt, 0).Add(timeout)
	delay := due.Sub(now())
	if delay < 0 {
		delay = 0
	}
	go func(receiptID string) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
		}
		engine, engineErr := c.laneInputEngine(runtime)
		if engineErr != nil {
			return
		}
		if _, retireErr := engine.RetireStagedLane(receiptID); retireErr != nil {
			return
		}
		c.mu.Lock()
		actor.state, actor.inputPump, actor.activeReceiptID = "archived", false, ""
		c.mu.Unlock()
	}(staged.ReceiptID)
}

func laneRestartIsRecoverable(lane daemonpkg.Lane, turn daemonpkg.Turn) bool {
	return lane.Product == "codex" && lane.NativeSessionID != "" &&
		turn.State == "dispatched" && turn.NativeDispatchID != ""
}

func (c *hostCoordinator) recoverCodexLaneTurn(runtime *daemonpkg.Runtime, actor *laneActor) {
	c.mu.Lock()
	threadID, cwd := actor.nativeID, actor.cwd
	preferredTurnID, deadlineAt := actor.nativeTurnID, actor.deadlineAt
	c.mu.Unlock()
	native, err := c.codexNative()
	if err == nil {
		var reattached bridge.CodexNativeThread
		reattached, err = native.ReattachThread(c.ctx, threadID)
		if err == nil {
			err = validateCodexLaneReattachment(threadID, cwd, reattached)
		}
	}
	nativeTurnID := ""
	if err == nil {
		nativeTurnID, err = native.ResolveLaneTurnID(c.ctx, threadID, preferredTurnID)
	}
	if err != nil {
		c.finishUnrecoverableCodexLane(runtime, actor, err)
		return
	}
	if nativeTurnID != preferredTurnID {
		c.finishUnrecoverableCodexLane(runtime, actor, errors.New("active Codex turn changed from durable recovery anchor"))
		return
	}
	if err := c.markActiveLaneInputInjected(runtime, actor, threadID, nativeTurnID); err != nil {
		c.finishUnrecoverableCodexLane(runtime, actor, err)
		return
	}
	adapter, err := newCodexLaneAdapter(native)
	if err != nil {
		c.finishUnrecoverableCodexLane(runtime, actor, err)
		return
	}
	turnCtx, cancel := context.WithCancel(c.ctx)
	if deadlineAt > 0 {
		cancel()
		turnCtx, cancel = context.WithDeadline(c.ctx, time.UnixMilli(deadlineAt))
	}
	c.mu.Lock()
	actor.cancel, actor.state, actor.nativeTurnID = cancel, "running", nativeTurnID
	c.mu.Unlock()
	if err := c.markLaneRunning(runtime, actor); err != nil {
		cancel()
		c.finishUnrecoverableCodexLane(runtime, actor, err)
		return
	}
	go c.watchCodexLaneTurn(runtime, actor, adapter, threadID, nativeTurnID, turnCtx, cancel)
}

func validateCodexLaneReattachment(storedThreadID, storedCwd string, live bridge.CodexNativeThread) error {
	if live.ID != storedThreadID {
		return errors.New("native session gone: Codex returned a different thread identity")
	}
	expectedCwd, err := pathidentity.ExistingDirectory(storedCwd)
	if err != nil {
		return fmt.Errorf("native session gone: durable Codex cwd: %w", err)
	}
	liveCwd, err := pathidentity.ExistingDirectory(live.Cwd)
	if err != nil || liveCwd != expectedCwd {
		return errors.New("native session gone: Codex thread cwd/provenance changed")
	}
	return nil
}

func (c *hostCoordinator) finishUnrecoverableCodexLane(runtime *daemonpkg.Runtime, actor *laneActor, cause error) {
	c.mu.Lock()
	receiptID := actor.activeReceiptID
	actor.state, actor.outcome = "terminal", "interrupted"
	actor.failure = fmt.Sprintf("recover Codex lane after daemon restart: %v", cause)
	actor.completedAt, actor.cancel, actor.nativeTurnID, actor.capability = time.Now().UnixMilli(), nil, "", ""
	_ = c.markLaneTerminal(runtime, actor)
	terminal := cloneLaneActor(actor)
	close(actor.done)
	c.mu.Unlock()
	if receiptID != "" {
		if engine, err := c.laneInputEngine(runtime); err == nil {
			if snapshot, readErr := runtime.State().Read(); readErr == nil &&
				snapshot.Catalog.LaneInputs[receiptID].State == daemonpkg.ReceiptInjected {
				_, _ = engine.Retire(receiptID)
			} else {
				_, _ = engine.MarkAmbiguous(receiptID, daemonpkg.AmbiguityNativeAcceptanceUnproven)
			}
		}
	}
	c.releaseLaneInputPump(actor, receiptID)
	c.queueLaneTerminalNotice(runtime, &terminal)
	_ = c.kickLaneInput(runtime, actor)
}
func (c *hostCoordinator) markLaneRunning(r *daemonpkg.Runtime, a *laneActor) error {
	c.laneInputCommitMu.Lock()
	defer c.laneInputCommitMu.Unlock()
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	lane, err := c.durableLaneWithSequence(r, a, "running")
	if err != nil {
		return err
	}
	lane.CapabilityHash = daemonpkg.CapabilityDigest(a.capability)
	turn := durableTurn(a, 0, "dispatched")
	turn.NativeDispatchID, turn.StartedAt, turn.DeadlineAt = a.nativeTurnID, a.startedAt, a.deadlineAt
	return engine.Dispatch(lane, turn)
}
func (c *hostCoordinator) markLaneTerminal(r *daemonpkg.Runtime, a *laneActor) error {
	c.laneInputCommitMu.Lock()
	defer c.laneInputCommitMu.Unlock()
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	lane, err := c.durableLaneWithSequence(r, a, "terminal")
	if err != nil {
		return err
	}
	turn := durableTurn(a, 0, "terminal")
	turn.NativeDispatchID, turn.StartedAt, turn.DeadlineAt = a.nativeTurnID, a.startedAt, a.deadlineAt
	turn.Outcome, turn.Result, turn.Diagnostic = a.outcome, a.result, a.failure
	turn.ExitCode, turn.CompletedAt = laneOutcomeExitCode(a.outcome), a.completedAt
	_, err = engine.Complete(lane, turn)
	return err
}
func (c *hostCoordinator) collectLane(r *daemonpkg.Runtime, id, turnID string) (bool, error) {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return false, err
	}
	collection, err := engine.Collect(id, turnID, defaultUnifiedLaneAutoArchiveDelay)
	if err == nil && collection.AutoArchiveAt > 0 {
		c.mu.Lock()
		if actor := c.lanes[id]; actor != nil {
			actor.autoArchiveAt = collection.AutoArchiveAt
		}
		c.mu.Unlock()
	}
	if err != nil {
		return false, err
	}
	return collection.RemainingDebt, nil
}

func oldestUncollectedLaneTurn(r *daemonpkg.Runtime, laneID string) (daemonpkg.Turn, bool, error) {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return daemonpkg.Turn{}, false, err
	}
	return engine.OldestCollectable(laneID)
}

func catalogHasUncollectedLaneTurn(catalog daemonpkg.Catalog, laneID string) bool {
	for _, turn := range catalog.Turns {
		if turn.LaneID == laneID && turn.State == "terminal" {
			return true
		}
	}
	return false
}
func (c *hostCoordinator) durableLaneWithSequence(r *daemonpkg.Runtime, a *laneActor, state string) (daemonpkg.Lane, error) {
	lane := durableLane(a, state)
	snapshot, err := r.State().Read()
	if err != nil {
		return daemonpkg.Lane{}, err
	}
	if current, ok := snapshot.Catalog.Lanes[a.id]; ok {
		lane.InputSequence = current.InputSequence
	}
	return lane, nil
}
func (c *hostCoordinator) commitLaneState(r *daemonpkg.Runtime, id, state string) error {
	engine, err := daemonpkg.NewLaneEngine(r.State())
	if err != nil {
		return err
	}
	return engine.TransitionLane(id, state, "")
}

func durableLane(a *laneActor, state string) daemonpkg.Lane {
	lane := daemonpkg.Lane{ID: a.id, ParentAttachmentID: a.parentID, Product: a.product, Name: a.name, Cwd: a.cwd, State: state, InputSequence: a.inputSequence}
	copyLanePolicy(&lane, a)
	return lane
}

// exactLaneStartReplay binds a stable command key to its complete immutable
// authority/request tuple. Native anchors and lifecycle counters are mutable
// projections and deliberately are not part of this comparison.
func exactLaneStartReplay(stored, requested daemonpkg.Lane) bool {
	return stored.ID != "" &&
		stored.ID == requested.ID &&
		stored.ParentAttachmentID == requested.ParentAttachmentID &&
		stored.Product == requested.Product &&
		stored.Name == requested.Name &&
		stored.Cwd == requested.Cwd &&
		slices.Equal(stored.Groups, requested.Groups) &&
		slices.Equal(stored.ExplicitGroups, requested.ExplicitGroups) &&
		stored.InheritGroups == requested.InheritGroups &&
		stored.PermissionMode == requested.PermissionMode &&
		stored.ApprovalPolicy == requested.ApprovalPolicy &&
		stored.Sandbox == requested.Sandbox &&
		stored.Effort == requested.Effort &&
		stored.Schema == requested.Schema &&
		slices.Equal(stored.Arguments, requested.Arguments) &&
		stored.Persistent == requested.Persistent &&
		stored.AutoArchive == requested.AutoArchive &&
		stored.AutoArchiveDelayMS == requested.AutoArchiveDelayMS
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

func durableTurn(a *laneActor, sequence uint64, state string) daemonpkg.Turn {
	return daemonpkg.Turn{ID: a.turnID, LaneID: a.id, Sequence: sequence, State: state}
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
		if c.commitLaneState(runtime, actor.id, "archived") != nil {
			return
		}
		cleanupErr := c.retireLaneInputs(runtime, actor.id, true)
		nativeErr := c.archiveNativeLaneTracked(runtime, actor)
		if cleanupErr == nil && nativeErr == nil {
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
