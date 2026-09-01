package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/envutil"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

type connectorCallEnvelope struct {
	Product string          `json:"product"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type connectorToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (c *hostCoordinator) handleConnector(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request daemonpkg.ControlRequest,
) (json.RawMessage, error) {
	var envelope connectorCallEnvelope
	if json.Unmarshal(request.Payload, &envelope) != nil || strings.TrimSpace(envelope.Product) == "" {
		return nil, errors.New("connector request is invalid")
	}
	switch request.Operation {
	case "connector.initialize":
		var input map[string]any
		_ = json.Unmarshal(envelope.Params, &input)
		protocol, _ := input["protocolVersion"].(string)
		if protocol == "" {
			protocol = "2025-06-18"
		}
		instructions, err := sessiontools.ProductMCPInstructions(envelope.Product)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-sessions", "version": version},
			"instructions":    instructions,
		})
	case "connector.tools":
		tools, err := sessiontools.ProductMCPTools(envelope.Product)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"tools": tools})
	case "connector.call":
		var call connectorToolCall
		if json.Unmarshal(envelope.Params, &call) != nil || strings.TrimSpace(call.Name) == "" {
			return nil, errors.New("connector tool call is invalid")
		}
		operationID := strings.TrimSpace(request.IdempotencyKey)
		if operationID == "" {
			operationID = strings.TrimSpace(request.ID)
		}
		result, err := c.callLocalToolWithID(ctx, runtime, request.AttachmentID, call.Name, call.Arguments, operationID)
		if err != nil {
			return json.Marshal(map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true,
			})
		}
		return json.Marshal(map[string]any{
			"content":           []map[string]any{{"type": "text", "text": result.Text}},
			"structuredContent": result.Data,
		})
	default:
		return nil, errors.New("connector operation is unsupported")
	}
}

type localToolResult struct {
	Text string
	Data any
}

type localPeerTarget struct {
	attachment *daemonpkg.ManagedAttachment
	lane       *laneActor
	name       string
}

type messagePeerTarget struct {
	local  *localPeerTarget
	remote *federationpkg.Peer
}

//nolint:gocyclo // Tool dispatch keeps authorization, group visibility, and operation-specific validation together.
func (c *hostCoordinator) callLocalTool(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	sourceID, name string,
	args map[string]any,
) (localToolResult, error) {
	return c.callLocalToolWithID(ctx, runtime, sourceID, name, args, "")
}

// callLocalToolWithID preserves the connector/MCP operation identity through
// lane admission. The wrapper above remains for direct internal callers whose
// operations are not retry-addressed by an upstream protocol.
func (c *hostCoordinator) callLocalToolWithID(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	sourceID, name string,
	args map[string]any,
	operationID string,
) (localToolResult, error) {
	source, ok, err := c.activeLocalParent(runtime, sourceID)
	if err != nil {
		return localToolResult{}, daemonpkg.InactiveControlError()
	}
	if !ok {
		return localToolResult{}, daemonpkg.InactiveControlError()
	}
	switch name {
	case "list_peers":
		peers, err := c.visibleMessageTargets(runtime, source)
		if err != nil {
			return localToolResult{}, err
		}
		lines := make([]string, 0, len(peers))
		for _, peer := range peers {
			row := publicMessageTarget(peer)
			lines = append(lines, fmt.Sprintf("%s — %s, %s, cwd %s, session %s, groups %s",
				row["name"], row["product"], row["status"], row["cwd"], row["session_id"], strings.Join(stringSlice(row["groups"]), ",")))
		}
		text := "No live peers share a group with this session."
		if len(lines) != 0 {
			text = strings.Join(lines, "\n")
		}
		return localToolResult{Text: text, Data: map[string]any{"peers": publicMessageTargets(peers)}}, nil
	case "identity":
		displayName := c.attachmentDisplayName(runtime, source)
		return localToolResult{Text: displayName + " — session:" + source.ID, Data: publicAttachment(runtime, source)}, nil
	case "rename_session":
		if strings.TrimSpace(mapString(args, "name")) == "" {
			return localToolResult{}, errors.New("name is required")
		}
		if _, active, renameErr := runtime.Attachments().ActiveAttachment(source.ID); renameErr != nil {
			return localToolResult{}, renameErr
		} else if !active {
			return localToolResult{}, errors.New("lane names are managed by the lane lifecycle")
		}
		return localToolResult{}, errors.New("native rename driver is not composed")
	case "send_message":
		message := strings.TrimSpace(mapString(args, "message"))
		if message == "" {
			return localToolResult{}, errors.New("message is required")
		}
		targets, err := requestedLocalTargets(args)
		if err != nil {
			return localToolResult{}, err
		}
		peers, err := c.visibleMessageTargets(runtime, source)
		if err != nil {
			return localToolResult{}, err
		}
		result, err := c.routeMessageFrame(ctx, runtime, source, peers, federationpkg.AgentFrame{
			Version: federationpkg.AgentFrameVersion, Type: "send", MessageID: commandRequestID(),
			Targets: targets, Content: message,
		})
		if err != nil {
			return localToolResult{}, err
		}
		if err := requireAcceptedDeliveries(result.Deliveries); err != nil {
			return localToolResult{}, err
		}
		return localToolResult{
			Text: fmt.Sprintf("Message accepted by %d peer(s).", len(result.Deliveries)),
			Data: map[string]any{"message_id": result.MessageID, "deliveries": result.Deliveries},
		}, nil
	case "broadcast":
		group := strings.TrimSpace(mapString(args, "group"))
		message := strings.TrimSpace(mapString(args, "message"))
		if group == "" || message == "" || !containsString(source.Groups, group) {
			return localToolResult{}, errors.New("broadcast requires one of the sender's groups and a message")
		}
		peers, err := c.visibleMessageTargets(runtime, source)
		if err != nil {
			return localToolResult{}, err
		}
		result, err := c.routeMessageFrame(ctx, runtime, source, peers, federationpkg.AgentFrame{
			Version: federationpkg.AgentFrameVersion, Type: "broadcast", MessageID: commandRequestID(),
			Group: group, Content: message,
		})
		if err != nil {
			return localToolResult{}, err
		}
		if err := requireAcceptedDeliveries(result.Deliveries); err != nil {
			return localToolResult{}, err
		}
		return localToolResult{
			Text: fmt.Sprintf("Broadcast accepted by %d peer(s).", len(result.Deliveries)),
			Data: map[string]any{"message_id": result.MessageID, "deliveries": result.Deliveries},
		}, nil
	case "lane":
		payload, err := json.Marshal(laneCommandEnvelope{
			Product: mapString(args, "product"), Command: mapString(args, "command"),
			Arguments: mapStringSlice(args, "arguments"), Input: mapString(args, "input"),
			Cwd: source.Cwd, Host: mapString(args, "host"), SourceAttachmentID: source.ID,
		})
		if err != nil {
			return localToolResult{}, err
		}
		raw, err := c.handleLaneCommand(ctx, runtime, daemonpkg.ControlRequest{
			ID: operationID, IdempotencyKey: operationID, AttachmentID: source.ID, Payload: payload,
		})
		if err != nil {
			return localToolResult{}, err
		}
		var data map[string]any
		if json.Unmarshal(raw, &data) != nil {
			return localToolResult{}, errors.New("lane command returned invalid data")
		}
		return localToolResult{Text: string(raw), Data: data}, nil
	default:
		return localToolResult{}, fmt.Errorf("tool %s is not yet available through the unified daemon", name)
	}
}

func (c *hostCoordinator) routeMessageFrame(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	source daemonpkg.ManagedAttachment,
	targets []messagePeerTarget,
	frame federationpkg.AgentFrame,
) (federationpkg.AgentFrameResult, error) {
	sourceGroups, err := c.attachmentVisibilityGroups(runtime, source)
	if err != nil {
		return federationpkg.AgentFrameResult{}, err
	}
	sourcePeer := federationpkg.Peer{
		ID: source.ID, SessionID: source.NativeSessionID, Name: c.attachmentDisplayName(runtime, source),
		Product: source.Product, Cwd: source.Cwd, PermissionMode: source.PermissionMode,
		Groups: sourceGroups,
	}
	peers := make([]federationpkg.Peer, 0, len(targets))
	byID := make(map[string]messagePeerTarget, len(targets))
	for _, target := range targets {
		peer := routingPeer(target)
		if strings.TrimSpace(peer.ID) == "" {
			return federationpkg.AgentFrameResult{}, errors.New("message target has no stable identity")
		}
		peers = append(peers, peer)
		byID[peer.ID] = target
	}
	return daemonpkg.RouteDelivery(ctx, frame, sourcePeer, peers, func(
		callCtx context.Context,
		_ federationpkg.Peer,
		target federationpkg.Peer,
		deliveryID string,
		delivered federationpkg.AgentFrame,
	) error {
		selected, ok := byID[target.ID]
		if !ok {
			return errors.New("admitted message target disappeared")
		}
		_, err := c.deliverMessageTarget(callCtx, runtime, source, selected, deliveryID, delivered.Content, delivered.Group)
		return err
	})
}

func routingPeer(target messagePeerTarget) federationpkg.Peer {
	row := publicMessageTarget(target)
	peer := federationpkg.Peer{
		ID: fmt.Sprint(row["id"]), SessionID: fmt.Sprint(row["session_id"]),
		Name: fmt.Sprint(row["name"]), Product: fmt.Sprint(row["product"]),
		Status: fmt.Sprint(row["status"]), Cwd: fmt.Sprint(row["cwd"]),
		PermissionMode: fmt.Sprint(row["permission_mode"]), Groups: stringSlice(row["groups"]),
	}
	if target.remote != nil {
		peer.GlobalID = target.remote.GlobalID
		peer.DisplayName = target.remote.DisplayName
		peer.HostID = target.remote.HostID
	}
	return peer
}

func requireAcceptedDeliveries(deliveries []federationpkg.DeliveryResult) error {
	for _, delivery := range deliveries {
		if delivery.Status != "accepted" {
			return fmt.Errorf("destination %s did not accept the delivery", delivery.Target)
		}
	}
	return nil
}

func (c *hostCoordinator) visibleMessageTargets(runtime *daemonpkg.Runtime, source daemonpkg.ManagedAttachment) ([]messagePeerTarget, error) {
	locals, err := c.visibleTargets(runtime, source)
	if err != nil {
		return nil, err
	}
	result := make([]messagePeerTarget, 0, len(locals))
	for index := range locals {
		local := locals[index]
		result = append(result, messagePeerTarget{local: &local})
	}
	c.mu.Lock()
	host := c.federation
	c.mu.Unlock()
	if host != nil {
		sourcePeer, sourceErr := c.localFederationPeer(runtime, source)
		if sourceErr != nil {
			return nil, sourceErr
		}
		for _, remote := range host.RemotePeers() {
			if groupsIntersect(sourcePeer.Groups, remote.Groups) {
				peer := remote
				result = append(result, messagePeerTarget{remote: &peer})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := publicMessageTarget(result[i]), publicMessageTarget(result[j])
		if fmt.Sprint(left["name"]) != fmt.Sprint(right["name"]) {
			return fmt.Sprint(left["name"]) < fmt.Sprint(right["name"])
		}
		return fmt.Sprint(left["id"]) < fmt.Sprint(right["id"])
	})
	return result, nil
}

func (c *hostCoordinator) deliverMessageTarget(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	source daemonpkg.ManagedAttachment,
	target messagePeerTarget,
	messageID, body, group string,
) ([]byte, error) {
	if target.local != nil {
		return nil, c.deliverUnified(ctx, runtime, source, *target.local, messageID, body)
	}
	if target.remote == nil {
		return nil, errors.New("target disappeared")
	}
	c.mu.Lock()
	host := c.federation
	c.mu.Unlock()
	if host == nil {
		return nil, errors.New("daemon federation component is unavailable")
	}
	sourcePeer, err := c.localFederationPeer(runtime, source)
	if err != nil {
		return nil, err
	}
	return nil, host.Send(ctx, sourcePeer, *target.remote, messageID, body, group)
}

func publicMessageTargets(targets []messagePeerTarget) []map[string]any {
	result := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		result = append(result, publicMessageTarget(target))
	}
	return result
}

func publicMessageTarget(target messagePeerTarget) map[string]any {
	if target.local != nil {
		return publicLocalTarget(*target.local)
	}
	if target.remote == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id": target.remote.GlobalID, "session_id": target.remote.SessionID,
		"name": target.remote.DisplayName, "product": target.remote.Entrypoint,
		"status": target.remote.Status, "cwd": target.remote.Cwd,
		"groups":          append([]string(nil), target.remote.Groups...),
		"permission_mode": target.remote.PermissionMode, "host_id": target.remote.HostID,
		"kind": "remote-peer",
	}
}

func (c *hostCoordinator) activeLocalParent(
	runtime *daemonpkg.Runtime,
	id string,
) (daemonpkg.ManagedAttachment, bool, error) {
	attachment, ok, err := runtime.Attachments().ActiveAttachment(id)
	if err != nil || ok {
		return attachment, ok, err
	}
	attachment, ok = c.activeLaneAttachment(id)
	return attachment, ok, nil
}

func (c *hostCoordinator) localParentIsLive(runtime *daemonpkg.Runtime, id string) (bool, error) {
	_, live, err := runtime.Attachments().ActiveAttachment(id)
	if err != nil || live {
		return live, err
	}
	c.mu.Lock()
	federationHost := c.federation
	actor := c.lanes[id]
	laneLive := actor != nil && actor.state != "archived" && actor.state != "retiring" && actor.state != "cleanup-debt"
	c.mu.Unlock()
	if laneLive {
		return true, nil
	}
	if federationHost != nil {
		for _, peer := range federationHost.RemotePeers() {
			if peer.ID == id {
				return true, nil
			}
		}
	}
	return false, nil
}

func (c *hostCoordinator) activeLaneAttachment(id string) (daemonpkg.ManagedAttachment, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	actor := c.lanes[id]
	if actor == nil || (actor.state != "preparing" && actor.state != "running") {
		return daemonpkg.ManagedAttachment{}, false
	}
	return daemonpkg.ManagedAttachment{
		ID: actor.id, Product: actor.product, NativeSessionID: actor.nativeID,
		Cwd: actor.cwd, Groups: append([]string(nil), actor.groups...),
		PermissionMode: actor.permission, State: "attached",
	}, true
}

func (c *hostCoordinator) visibleTargets(runtime *daemonpkg.Runtime, source daemonpkg.ManagedAttachment) ([]localPeerTarget, error) {
	if err := c.reconcileOrphanedLanes(runtime); err != nil {
		return nil, err
	}
	sourceGroups, err := c.attachmentVisibilityGroups(runtime, source)
	if err != nil {
		return nil, err
	}
	attachments, err := c.visiblePeers(runtime, source)
	if err != nil {
		return nil, err
	}
	snapshot, err := runtime.State().Read()
	if err != nil {
		return nil, err
	}
	staged := stagedUnacknowledgedLaneInputs(snapshot.Catalog)
	targets := make([]localPeerTarget, 0, len(attachments)+len(c.lanes))
	for index := range attachments {
		attachment := attachments[index]
		targets = append(targets, localPeerTarget{
			attachment: &attachment, name: c.attachmentDisplayName(runtime, attachment),
		})
	}
	c.mu.Lock()
	for _, lane := range c.lanes {
		durableState := snapshot.Catalog.Lanes[lane.id].State
		if !staged[lane.id] && durableState != "archived" && durableState != "retiring" && durableState != "cleanup-debt" && groupsIntersect(sourceGroups, lane.groups) {
			targets = append(targets, localPeerTarget{lane: lane})
		}
	}
	c.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool {
		left, right := publicLocalTarget(targets[i]), publicLocalTarget(targets[j])
		if fmt.Sprint(left["name"]) != fmt.Sprint(right["name"]) {
			return fmt.Sprint(left["name"]) < fmt.Sprint(right["name"])
		}
		return fmt.Sprint(left["id"]) < fmt.Sprint(right["id"])
	})
	return targets, nil
}

func (c *hostCoordinator) deliverUnified(ctx context.Context, runtime *daemonpkg.Runtime, source daemonpkg.ManagedAttachment, target localPeerTarget, messageID, body string) error {
	if target.attachment != nil {
		return c.deliverLocal(ctx, runtime, source, *target.attachment, messageID, body)
	}
	if target.lane == nil {
		return errors.New("target disappeared")
	}
	mode := "prompting"
	if source.PermissionMode == "bypassPermissions" {
		mode = "bypass"
	}
	message, err := sessiontools.WrapPeerMessage(
		source.Product, "session:"+source.ID, source.NativeSessionID,
		c.attachmentDisplayName(runtime, source), mode,
		messageID, time.Now().UTC().Format(time.RFC3339Nano), body,
	)
	if err != nil {
		return err
	}
	_, err = c.deliverLaneMessageWithID(runtime, target.lane, messageID, message)
	return err
}

func publicLocalTarget(target localPeerTarget) map[string]any {
	if target.attachment != nil {
		return map[string]any{
			"id": target.attachment.ID, "session_id": target.attachment.NativeSessionID,
			"name": target.name, "product": target.attachment.Product, "status": "live",
			"cwd": target.attachment.Cwd, "groups": append([]string(nil), target.attachment.Groups...),
			"permission_mode": target.attachment.PermissionMode,
		}
	}
	if target.lane == nil {
		return map[string]any{}
	}
	targetStatus := "idle"
	if target.lane.state == "running" || target.lane.state == "preparing" || target.lane.state == "interrupting" {
		targetStatus = "busy"
	}
	return map[string]any{
		"id": target.lane.id, "session_id": target.lane.nativeID,
		"name": target.lane.name, "product": target.lane.product, "status": targetStatus,
		"cwd": target.lane.cwd, "groups": append([]string(nil), target.lane.groups...),
		"permission_mode": target.lane.permission, "kind": "lane",
	}
}

func stringSlice(value any) []string {
	values, _ := value.([]string)
	return values
}

func mapStringSlice(values map[string]any, key string) []string {
	raw := values[key]
	switch typed := raw.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if value, ok := item.(string); ok {
				result = append(result, value)
			}
		}
		return result
	default:
		return nil
	}
}

func (c *hostCoordinator) visiblePeers(
	runtime *daemonpkg.Runtime,
	source daemonpkg.ManagedAttachment,
) ([]daemonpkg.ManagedAttachment, error) {
	sourceGroups, err := c.attachmentVisibilityGroups(runtime, source)
	if err != nil {
		return nil, err
	}
	all, err := runtime.Attachments().ListActive()
	if err != nil {
		return nil, err
	}
	visible := make([]daemonpkg.ManagedAttachment, 0, len(all))
	for _, candidate := range all {
		candidateGroups, groupErr := c.attachmentVisibilityGroups(runtime, candidate)
		if groupErr != nil {
			return nil, groupErr
		}
		if candidate.ID != source.ID && groupsIntersect(sourceGroups, candidateGroups) {
			visible = append(visible, candidate)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	return visible, nil
}

func (c *hostCoordinator) deliverLocal(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	source, target daemonpkg.ManagedAttachment,
	messageID, body string,
) error {
	mode := "prompting"
	if source.PermissionMode == "bypassPermissions" {
		mode = "bypass"
	}
	message, err := sessiontools.WrapPeerMessage(
		source.Product, "session:"+source.ID, source.NativeSessionID,
		c.attachmentDisplayName(runtime, source), mode,
		messageID, time.Now().UTC().Format(time.RFC3339Nano), body,
	)
	if err != nil {
		return err
	}
	return c.deliverPreparedMessage(ctx, target, messageID, "session:"+source.ID, message)
}

func (c *hostCoordinator) deliverPreparedMessage(
	ctx context.Context,
	target daemonpkg.ManagedAttachment,
	messageID, from, message string,
) error {
	switch target.Product {
	case "codex":
		native, err := c.codexNative()
		if err != nil {
			return err
		}
		_, err = native.SendMessage(ctx, target.NativeSessionID, message)
		return err
	case "claude":
		return sendClaudeNativeMessage(target.Evidence.SocketPath, messageID, from, message)
	case "qwen":
		return c.submitQwenMessage(target, message)
	case "grok":
		return c.interjectGrokMessage(ctx, target, messageID, message)
	default:
		return errors.New("target product is unsupported")
	}
}

func (c *hostCoordinator) submitQwenMessage(target daemonpkg.ManagedAttachment, message string) error {
	c.mu.Lock()
	pending := c.qwenPending[target.ID]
	if pending == nil {
		c.mu.Unlock()
		// Native input ownership is durable, while an open append descriptor is
		// intentionally process-local. A successor daemon must re-open the exact
		// re-attested artifact instead of making a surviving Qwen peer inert.
		refreshed, err := refreshDurableQwenAttachment(c.stateRoot, target)
		if err != nil {
			return fmt.Errorf("qwen native input is unavailable: %w", err)
		}
		writer, err := bridge.OpenQwenNativeInput(refreshed.ArtifactPath)
		if err != nil {
			return err
		}
		defer func() { _ = writer.Close() }()
		return writer.Submit(message)
	}
	if pending.writer == nil {
		writer, err := bridge.OpenQwenNativeInput(pending.input.Path)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		pending.writer = writer
	}
	writer := pending.writer
	c.mu.Unlock()
	return writer.Submit(message)
}

func (c *hostCoordinator) interjectGrokMessage(
	ctx context.Context,
	target daemonpkg.ManagedAttachment,
	messageID, message string,
) error {
	c.mu.Lock()
	pending := c.grokPending[target.ID]
	if pending == nil {
		c.mu.Unlock()
		// The ACP observer is only a daemon-generation handle. The TUI, private
		// leader, socket, and exact process identities survive service restart and
		// are durably attested, so reconnect to them before interjecting.
		evidence, err := refreshDurableGrokAttachment(target)
		if err != nil {
			return fmt.Errorf("grok native observer is unavailable: %w", err)
		}
		environment := envutil.Set(os.Environ(), "AGENT_SESSIONS_PRODUCT", "grok")
		environment = envutil.Set(environment, "AGENT_SESSIONS_SESSION_ID", target.NativeSessionID)
		observer, err := bridge.OpenGrokNativeObserver(
			ctx, evidence.Executable, target.Cwd, evidence.SocketPath,
			target.NativeSessionID, environment, nil,
		)
		if err != nil {
			return err
		}
		defer observer.Close()
		return observer.Interject(ctx, messageID, message)
	}
	observer := pending.observer
	c.mu.Unlock()
	if observer == nil {
		environment := envutil.Set(os.Environ(), "AGENT_SESSIONS_PRODUCT", "grok")
		environment = envutil.Set(environment, "AGENT_SESSIONS_SESSION_ID", pending.request.SessionID)
		created, err := bridge.OpenGrokNativeObserver(
			c.ctx, pending.request.GrokBin, pending.request.Cwd, pending.leaderSocket,
			pending.request.SessionID, environment, pending.diagnostics,
		)
		if err != nil {
			return err
		}
		c.mu.Lock()
		if current := c.grokPending[target.ID]; current != nil && current.observer == nil {
			current.observer = created
			observer = created
		} else {
			created.Close()
			if current != nil {
				observer = current.observer
			}
		}
		c.mu.Unlock()
	}
	if observer == nil {
		return errors.New("grok native observer disappeared")
	}
	return observer.Interject(ctx, messageID, message)
}

func sendClaudeNativeMessage(socket, messageID, from, message string) error {
	if strings.TrimSpace(socket) == "" {
		return errors.New("claude messaging socket is unavailable")
	}
	body, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": messageID, "type": "user", "priority": "next",
		"from": from, "message": map[string]any{"role": "user", "content": message},
	})
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(append(body, '\n'))
	return err
}

func requestedLocalTargets(args map[string]any) ([]string, error) {
	single := strings.TrimSpace(mapString(args, "target"))
	var targets []string
	if raw, ok := args["targets"].([]any); ok {
		for _, value := range raw {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				targets = append(targets, strings.TrimSpace(text))
			}
		}
	}
	if single != "" {
		if len(targets) != 0 {
			return nil, errors.New("use either target or targets")
		}
		targets = []string{single}
	}
	if len(targets) == 0 {
		return nil, errors.New("target or targets is required")
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target] {
			return nil, errors.New("targets contain duplicates")
		}
		seen[target] = true
	}
	return targets, nil
}

func publicAttachment(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) map[string]any {
	name := attachment.ID
	if runtime != nil {
		if title, observed, err := runtime.Attachments().LiveNativeTitle(attachment.ID); err == nil && observed && title != "" {
			name = title
		}
	}
	return map[string]any{
		"id": attachment.ID, "session_id": attachment.NativeSessionID,
		"name": name, "product": attachment.Product, "status": "live",
		"cwd": attachment.Cwd, "groups": append([]string(nil), attachment.Groups...),
		"permission_mode": attachment.PermissionMode,
	}
}

func (c *hostCoordinator) attachmentDisplayName(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) string {
	if runtime != nil {
		if title, observed, err := runtime.Attachments().LiveNativeTitle(attachment.ID); err == nil && observed && title != "" {
			return title
		}
	}
	return attachment.ID
}

func groupsIntersect(left, right []string) bool {
	for _, value := range left {
		if containsString(right, value) {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func mapString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}
