package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type connectorToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type connectorToolEnvelope struct {
	SourceID  string         `json:"source_id"`
	RequestID string         `json:"request_id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (c *hostCoordinator) handleLiveSessionCall(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	report livepresence.Report,
	requestID string,
	method string,
	params json.RawMessage,
) (json.RawMessage, error) {
	spec, known := livepresence.LookupMethod(method)
	if !known || spec.Direction != livepresence.ClientToDaemon || method == "session.hello" || method == "session.update" {
		return nil, livepresence.NewError(livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": method})
	}
	switch method {
	case "peers.list":
		if err := livepresence.DecodeStrict(params, &struct{}{}); err != nil {
			return nil, livepresence.NewError(livepresence.InvalidParams, "Invalid params", map[string]any{"method": method})
		}
		return c.callLiveTool(ctx, runtime, report.UUID, requestID, "list_peers", map[string]any{}, nil)
	case "message.send":
		arguments, err := decodeLiveMessageSend(params)
		if err != nil {
			return nil, livepresence.NewError(livepresence.InvalidParams, "Invalid params", map[string]any{"method": method})
		}
		return c.callLiveTool(ctx, runtime, report.UUID, requestID, "send_message", arguments, nil)
	case "lane.doctor", "lane.list", "lane.start", "lane.run", "lane.resume", "lane.steer", "lane.wait", "lane.status", "lane.interrupt", "lane.archive":
		arguments, invocationCwd, err := decodeLiveLaneCall(method, params)
		if err != nil {
			return nil, livepresence.NewError(livepresence.InvalidParams, "Invalid params", map[string]any{"method": method})
		}
		return c.callLiveTool(ctx, runtime, report.UUID, requestID, "lane", arguments, invocationCwd)
	default:
		return nil, livepresence.NewError(livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": method})
	}
}

func (c *hostCoordinator) callLiveTool(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	sourceID, requestID, name string,
	arguments map[string]any, invocationCwd *string,
) (json.RawMessage, error) {
	result, err := c.callLocalToolWithID(ctx, runtime, sourceID, name, arguments, "presence:"+sourceID+":"+requestID, invocationCwd)
	if err != nil {
		return nil, err
	}
	if result.Data == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(result.Data)
}

func decodeLiveMessageSend(raw json.RawMessage) (map[string]any, error) {
	var params struct {
		Target  *string   `json:"target"`
		Targets *[]string `json:"targets"`
		Group   *string   `json:"group"`
		Message *string   `json:"message"`
	}
	if err := livepresence.DecodeStrict(raw, &params); err != nil || params.Message == nil || strings.TrimSpace(*params.Message) == "" ||
		boolCount(params.Target != nil, params.Targets != nil, params.Group != nil) != 1 {
		return nil, errors.New("message send params are invalid")
	}
	arguments := map[string]any{"message": *params.Message}
	if params.Target != nil {
		if strings.TrimSpace(*params.Target) == "" {
			return nil, errors.New("message target is invalid")
		}
		arguments["target"] = *params.Target
		return arguments, nil
	}
	if params.Group != nil {
		if strings.TrimSpace(*params.Group) == "" {
			return nil, errors.New("message group is invalid")
		}
		arguments["group"] = *params.Group
		return arguments, nil
	}
	if len(*params.Targets) == 0 {
		return nil, errors.New("message targets are invalid")
	}
	seen := map[string]bool{}
	values := make([]any, 0, len(*params.Targets))
	for _, target := range *params.Targets {
		if strings.TrimSpace(target) == "" || seen[target] {
			return nil, errors.New("message targets are invalid")
		}
		seen[target] = true
		values = append(values, target)
	}
	arguments["targets"] = values
	return arguments, nil
}

func decodeLiveLaneCall(method string, raw json.RawMessage) (map[string]any, *string, error) {
	var params struct {
		Product   *string   `json:"product"`
		Arguments *[]string `json:"arguments"`
		Input     *string   `json:"input"`
		Host      *string   `json:"host"`
		Cwd       *string   `json:"cwd"`
	}
	if err := livepresence.DecodeStrict(raw, &params); err != nil || params.Product == nil || strings.TrimSpace(*params.Product) == "" || params.Arguments == nil {
		return nil, nil, errors.New("lane params are invalid")
	}
	spec, known := livepresence.LookupMethod(method)
	if !known || !spec.Lane {
		return nil, nil, errors.New("lane method is invalid")
	}
	needsInput := spec.NeedsInput
	if needsInput != (params.Input != nil) || needsInput && strings.TrimSpace(*params.Input) == "" || params.Host != nil && strings.TrimSpace(*params.Host) == "" {
		return nil, nil, errors.New("lane params are invalid")
	}
	var invocationCwd *string
	if params.Cwd != nil {
		if strings.TrimSpace(*params.Cwd) == "" || !filepath.IsAbs(*params.Cwd) {
			return nil, nil, errors.New("lane cwd is invalid")
		}
		cwd := *params.Cwd
		invocationCwd = &cwd
	}
	for _, argument := range *params.Arguments {
		if strings.ContainsAny(argument, "\x00\r\n") {
			return nil, nil, errors.New("lane argument is invalid")
		}
	}
	operation := strings.TrimPrefix(method, "lane.")
	if _, err := parseUnifiedLaneCommand(append([]string{operation}, (*params.Arguments)...)); err != nil {
		return nil, nil, err
	}
	arguments := map[string]any{"product": *params.Product, "command": operation, "arguments": append([]string(nil), (*params.Arguments)...)}
	if params.Input != nil {
		arguments["input"] = *params.Input
	}
	if params.Host != nil {
		arguments["host"] = *params.Host
	}
	return arguments, invocationCwd, nil
}

func (c *hostCoordinator) handleConnectorTool(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request daemonpkg.ControlRequest,
) (json.RawMessage, error) {
	var call connectorToolEnvelope
	if json.Unmarshal(request.Payload, &call) != nil || strings.TrimSpace(call.RequestID) == "" || strings.TrimSpace(call.Name) == "" {
		return nil, errors.New("connector tool call is invalid")
	}
	result, err := c.callLocalToolWithID(ctx, runtime, call.SourceID, call.Name, call.Arguments, call.RequestID, nil)
	if err != nil {
		return nil, livepresence.FailureFromError(nil, "connector.tool", err).Error
	}
	return marshalConnectorToolResult(result)
}
func marshalConnectorToolResult(result localToolResult) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": result.Text}},
		"structuredContent": result.Data,
	})
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
	return c.callLocalToolWithID(ctx, runtime, sourceID, name, args, "", nil)
}

// callLocalToolWithID preserves the connector/MCP operation identity through
// lane admission. The wrapper above remains for direct internal callers whose
// operations are not retry-addressed by an upstream protocol.
func (c *hostCoordinator) callLocalToolWithID(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	sourceID, name string,
	args map[string]any,
	operationID string, invocationCwd *string,
) (localToolResult, error) {
	source, ok, err := c.activeLocalParent(runtime, sourceID)
	if err != nil {
		return localToolResult{}, daemonpkg.InactiveControlError()
	}
	if !ok {
		return localToolResult{}, daemonpkg.InactiveControlError()
	}
	if invocationCwd != nil {
		source.Cwd = *invocationCwd
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
		return localToolResult{Text: displayName + " — session:" + source.ID, Data: publicAttachment(source, displayName)}, nil
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
		targets, group, err := requestedLocalSelector(args)
		if err != nil {
			return localToolResult{}, err
		}
		peers, err := c.visibleMessageTargets(runtime, source)
		if err != nil {
			return localToolResult{}, err
		}
		result, err := c.routeMessageFrame(ctx, runtime, source, peers, federationpkg.AgentFrame{
			Version: federationpkg.AgentFrameVersion, Type: "send", MessageID: commandRequestID(),
			Targets: targets, Group: group, Content: message,
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
	source.Groups = sourceGroups
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
			if delivery.Cause != nil {
				return delivery.Cause
			}
			return errors.New(delivery.Error)
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
		"kind": "remote-peer", "info": map[string]string{},
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
	laneLive := actor != nil && actor.state != "archived" && actor.state != "retiring"
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
	displayNames := c.attachmentDisplayNames(runtime, attachments)
	targets := make([]localPeerTarget, 0, len(attachments)+len(c.lanes))
	for index := range attachments {
		attachment := attachments[index]
		targets = append(targets, localPeerTarget{
			attachment: &attachment, name: displayNames[attachment.ID],
		})
	}
	c.mu.Lock()
	for _, lane := range c.lanes {
		if lane.state != "archived" && lane.state != "retiring" && groupsIntersect(sourceGroups, lane.groups) {
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
	message := c.nativeMessage(runtime, source, messageID, body)
	if target.attachment != nil {
		return c.deliverPreparedMessage(ctx, *target.attachment, message)
	}
	if target.lane == nil {
		return errors.New("target disappeared")
	}
	return c.deliverLaneMessage(ctx, target.lane, message)
}

func publicLocalTarget(target localPeerTarget) map[string]any {
	if target.attachment != nil {
		return map[string]any{
			"id": target.attachment.ID, "session_id": target.attachment.NativeSessionID,
			"name": target.name, "product": target.attachment.Product, "status": "live",
			"cwd": target.attachment.Cwd, "groups": append([]string(nil), target.attachment.Groups...),
			"permission_mode": target.attachment.PermissionMode, "info": livepresence.CloneInfo(target.attachment.Info),
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
		"permission_mode": target.lane.permission, "kind": "lane", "info": map[string]string{},
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
			candidate.Groups = candidateGroups
			visible = append(visible, candidate)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	return visible, nil
}

func (c *hostCoordinator) deliverPreparedMessage(
	ctx context.Context,
	target daemonpkg.ManagedAttachment,
	message productruntime.NativeMessage,
) error {
	c.mu.Lock()
	presence := c.presence
	c.mu.Unlock()
	if presence == nil {
		return errors.New("live session channel is unavailable")
	}
	_, err := presence.Call(ctx, target.ID, message.ID, "message.deliver", message)
	return err
}

func (c *hostCoordinator) nativeMessage(runtime *daemonpkg.Runtime, source daemonpkg.ManagedAttachment, messageID, body string) productruntime.NativeMessage {
	uuid := source.NativeSessionID
	if uuid == "" {
		uuid = source.ID
	}
	return productruntime.NativeMessage{
		ID: messageID, Body: body,
		From: productruntime.NativeMessageSource{
			UUID: uuid, Name: c.attachmentDisplayName(runtime, source), Product: source.Product,
			Groups: append([]string(nil), source.Groups...),
		},
	}
}

func requestedLocalSelector(args map[string]any) ([]string, string, error) {
	_, singlePresent := args["target"]
	_, targetsPresent := args["targets"]
	_, groupPresent := args["group"]
	if boolCount(singlePresent, targetsPresent, groupPresent) != 1 {
		return nil, "", errors.New("use exactly one of target, targets, or group")
	}
	single := strings.TrimSpace(mapString(args, "target"))
	group := strings.TrimSpace(mapString(args, "group"))
	var targets []string
	if targetsPresent {
		switch raw := args["targets"].(type) {
		case []string:
			for _, target := range raw {
				if strings.TrimSpace(target) == "" {
					return nil, "", errors.New("targets must be non-empty strings")
				}
				targets = append(targets, strings.TrimSpace(target))
			}
		case []any:
			for _, value := range raw {
				text, ok := value.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return nil, "", errors.New("targets must be non-empty strings")
				}
				targets = append(targets, strings.TrimSpace(text))
			}
		default:
			return nil, "", errors.New("targets must be an array")
		}
	}
	if singlePresent {
		if single == "" {
			return nil, "", errors.New("target must be non-empty")
		}
		targets = []string{single}
	}
	if targetsPresent && len(targets) == 0 {
		return nil, "", errors.New("targets must not be empty")
	}
	if groupPresent && group == "" {
		return nil, "", errors.New("group must be non-empty")
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target] {
			return nil, "", errors.New("targets contain duplicates")
		}
		seen[target] = true
	}
	return targets, group, nil
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func publicAttachment(attachment daemonpkg.ManagedAttachment, name string) map[string]any {
	return map[string]any{
		"id": attachment.ID, "session_id": attachment.NativeSessionID,
		"name": name, "product": attachment.Product, "status": "live",
		"cwd": attachment.Cwd, "groups": append([]string(nil), attachment.Groups...),
		"permission_mode": attachment.PermissionMode, "info": livepresence.CloneInfo(attachment.Info),
	}
}

func (c *hostCoordinator) attachmentDisplayName(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) string {
	return c.attachmentDisplayNames(runtime, []daemonpkg.ManagedAttachment{attachment})[attachment.ID]
}

func (c *hostCoordinator) attachmentDisplayNames(
	runtime *daemonpkg.Runtime,
	attachments []daemonpkg.ManagedAttachment,
) map[string]string {
	names := make(map[string]string, len(attachments))
	byProduct := make(map[string][]daemonpkg.ManagedAttachment)
	c.mu.Lock()
	for _, attachment := range attachments {
		if lane := c.lanes[attachment.ID]; lane != nil && strings.TrimSpace(lane.name) != "" {
			names[attachment.ID] = lane.name
		}
	}
	c.mu.Unlock()
	for _, attachment := range attachments {
		if _, resolved := names[attachment.ID]; resolved {
			continue
		}
		if c.liveTitleResolvers[attachment.Product] != nil {
			byProduct[attachment.Product] = append(byProduct[attachment.Product], attachment)
		}
	}
	for product, productAttachments := range byProduct {
		for id, title := range c.liveTitleResolvers[product](productAttachments) {
			if strings.TrimSpace(title) != "" {
				names[id] = title
			}
		}
	}
	for _, attachment := range attachments {
		if _, resolved := names[attachment.ID]; resolved {
			continue
		}
		if runtime != nil {
			if title, observed, err := runtime.Attachments().LiveNativeTitle(attachment.ID); err == nil && observed && title != "" {
				names[attachment.ID] = title
				continue
			}
		}
		names[attachment.ID] = attachment.ID
	}
	return names
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
