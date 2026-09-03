package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/bridge"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	federationpkg "github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func (c *hostCoordinator) newFederationHost(runtime *daemonpkg.Runtime) (*daemonpkg.Federation, error) {
	hostID := strings.TrimSpace(runtime.HostID())
	hostName := daemonSetting("AGENT_SESSIONS_HOST_NAME")
	if hostName == "" {
		hostName = hostID
	}
	hub := daemonSetting("AGENT_SESSIONS_HUB")
	capabilities := c.federationCapabilities()
	federationpkg.RuntimeVersion = version
	return daemonpkg.NewFederation(federationpkg.EmbeddedHostOptions{
		Hub: hub, HostID: hostID, HostName: hostName, Capabilities: capabilities,
		Build:  version,
		Logger: log.New(os.Stderr, "agent-sessions federation: ", log.LstdFlags|log.Lmicroseconds),
		Snapshot: func(_ context.Context) ([]federationpkg.Peer, error) {
			return c.federationSnapshot(runtime, hostID, hostName)
		},
		Deliver: func(ctx context.Context, source, target federationpkg.Peer, frame federationpkg.AgentFrame) error {
			return c.deliverFederated(ctx, runtime, source, target, frame)
		},
		RunLane: func(ctx context.Context, request federationpkg.RemoteLaneRequest) (federationpkg.RemoteLaneResult, error) {
			return c.runFederatedLane(ctx, runtime, request)
		},
	})
}

func (c *hostCoordinator) federationCapabilities() []string {
	capabilities := make([]string, 0, len(productcatalog.All()))
	for _, descriptor := range productcatalog.All() {
		report, err := doctorLane(c.ctx, descriptor.ID, "")
		if err != nil {
			continue
		}
		ready, _ := report["ready"].(bool)
		if !ready {
			continue
		}
		capabilities = append(capabilities, descriptor.FederationCapabilities...)
	}
	return capabilities
}

func (c *hostCoordinator) federationSnapshot(runtime *daemonpkg.Runtime, hostID, hostName string) ([]federationpkg.Peer, error) {
	if err := c.ensureLaneActors(runtime); err != nil {
		return nil, err
	}
	attachments, err := runtime.Attachments().ListActive()
	if err != nil {
		return nil, err
	}
	peers := make([]federationpkg.Peer, 0, len(attachments))
	for _, attachment := range attachments {
		groups := uniqueStrings(append(append([]string(nil), attachment.Groups...), "session:"+hostID+"/"+attachment.ID))
		instance := attachmentFederationInstance(attachment)
		peer, buildErr := federationpkg.BuildPeer(
			hostID, hostName, attachment.ID, c.attachmentDisplayName(runtime, attachment), "idle", attachment.Cwd,
			attachment.Product, attachment.PermissionMode, instance, "", groups,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		peers = append(peers, peer)
	}
	c.mu.Lock()
	lanes := make([]laneActor, 0, len(c.lanes))
	for _, actor := range c.lanes {
		if actor.state == "archived" || actor.state == "retiring" {
			continue
		}
		lanes = append(lanes, *actor)
	}
	c.mu.Unlock()
	for _, actor := range lanes {
		status := "idle"
		if actor.state == "preparing" || actor.state == "running" || actor.state == "interrupting" {
			status = "busy"
		}
		peer, buildErr := federationpkg.BuildPeer(
			hostID, hostName, actor.id, actor.name, status, actor.cwd, actor.product,
			actor.permission, actor.product+":"+actor.id, actor.parentID, actor.groups,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

func (c *hostCoordinator) localFederationPeer(runtime *daemonpkg.Runtime, attachment daemonpkg.ManagedAttachment) (federationpkg.Peer, error) {
	hostID := strings.TrimSpace(runtime.HostID())
	hostName := daemonSetting("AGENT_SESSIONS_HOST_NAME")
	if hostName == "" {
		hostName = hostID
	}
	groups, err := c.attachmentVisibilityGroups(runtime, attachment)
	if err != nil {
		return federationpkg.Peer{}, err
	}
	instance := attachmentFederationInstance(attachment)
	return federationpkg.BuildPeer(
		hostID, hostName, attachment.ID, c.attachmentDisplayName(runtime, attachment), "idle", attachment.Cwd,
		attachment.Product, attachment.PermissionMode, instance, "", groups,
	)
}

func daemonSetting(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	configRoot := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configRoot == "" {
		configRoot = filepath.Join(home, ".config")
	}
	// The filename is fixed; only the standard per-user config root is variable.
	file, err := os.Open(filepath.Join(configRoot, "agent-sessions", "service.env")) //nolint:gosec
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

func attachmentFederationInstance(attachment daemonpkg.ManagedAttachment) string {
	instance := attachment.Product + ":" + attachment.ID
	if attachment.Evidence.Process.StrongStart != "" {
		instance += ":" + attachment.Evidence.Process.StrongStart
	}
	return instance
}

func (c *hostCoordinator) localTargetByFederationID(runtime *daemonpkg.Runtime, sessionID string) (localPeerTarget, error) {
	if attachment, ok, err := runtime.Attachments().ActiveAttachment(sessionID); err != nil {
		return localPeerTarget{}, err
	} else if ok {
		return localPeerTarget{attachment: &attachment}, nil
	}
	c.mu.Lock()
	actor := c.lanes[sessionID]
	if actor == nil || actor.state == "archived" || actor.state == "retiring" {
		c.mu.Unlock()
		return localPeerTarget{}, errors.New("federated target is no longer local and live")
	}
	c.mu.Unlock()
	return localPeerTarget{lane: actor}, nil
}

func (c *hostCoordinator) deliverFederated(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	source, target federationpkg.Peer,
	frame federationpkg.AgentFrame,
) error {
	local, err := c.localTargetByFederationID(runtime, target.SessionID)
	if err != nil {
		return err
	}
	request := frame
	request.Type = "send"
	request.Source = nil
	request.SourceSessionID = ""
	request.Targets = []string{target.ID}
	result, err := daemonpkg.RouteDelivery(ctx, request, source, []federationpkg.Peer{target}, func(
		callCtx context.Context,
		_, admittedTarget federationpkg.Peer,
		deliveryID string,
		delivered federationpkg.AgentFrame,
	) error {
		if admittedTarget.ID != target.ID {
			return errors.New("admitted federated target changed")
		}
		mode := "prompting"
		if source.PermissionMode == "bypassPermissions" || source.PermissionMode == "bypass" {
			mode = "bypass"
		}
		name := source.DisplayName
		if strings.TrimSpace(name) == "" {
			name = source.Name
		}
		message := bridge.WrapPeerMessage(
			source.Entrypoint, "session:"+source.GlobalID, source.SessionID, name, mode,
			deliveryID, delivered.SentAt, delivered.Content,
		)
		if local.attachment != nil {
			return c.deliverPreparedMessage(callCtx, *local.attachment, deliveryID, message)
		}
		if local.lane == nil {
			return errors.New("federated target disappeared")
		}
		return c.deliverLaneMessage(callCtx, local.lane, deliveryID, message)
	})
	if err != nil {
		return err
	}
	if err := requireAcceptedDeliveries(result.Deliveries); err != nil {
		return err
	}
	return nil
}

func (c *hostCoordinator) handleRemoteLaneCommand(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	parent daemonpkg.ManagedAttachment,
	envelope laneCommandEnvelope,
	inputID string,
) (json.RawMessage, error) {
	c.mu.Lock()
	host := c.federation
	c.mu.Unlock()
	if host == nil {
		return nil, errors.New("daemon federation component is unavailable")
	}
	source, err := c.localFederationPeer(runtime, parent)
	if err != nil {
		return nil, err
	}
	capability, err := localFederationCapability(envelope.Product)
	if err != nil {
		return nil, err
	}
	result, err := host.RunRemoteLane(ctx, federationpkg.RemoteLaneRequest{
		Source: source,
		Parent: federationpkg.ParentContext{
			HostID: source.HostID, SessionID: source.SessionID, Product: source.Entrypoint,
			InstanceID: source.InstanceID, Groups: append([]string(nil), source.Groups...),
			PermissionMode: source.PermissionMode,
		},
		TargetHostID: strings.TrimSpace(envelope.Host), Product: envelope.Product, Capability: capability,
		Arguments: append([]string(nil), envelope.Arguments...), Input: []byte(envelope.Input), IdempotencyKey: inputID,
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("remote lane exited %d: %s", result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	body := bytes.TrimSpace(result.Stdout)
	if !json.Valid(body) {
		return nil, errors.New("remote lane returned invalid structured output")
	}
	return json.RawMessage(append([]byte(nil), body...)), nil
}

func (c *hostCoordinator) runFederatedLane(
	ctx context.Context,
	runtime *daemonpkg.Runtime,
	request federationpkg.RemoteLaneRequest,
) (federationpkg.RemoteLaneResult, error) {
	if err := authorizeFederatedLane(ctx, request.Product, request.Capability); err != nil {
		return federationpkg.RemoteLaneResult{}, err
	}
	parsed, err := parseUnifiedLaneCommand(request.Arguments)
	if err != nil {
		return federationpkg.RemoteLaneResult{}, err
	}
	if parsed.command == "run" || parsed.command == "start" || parsed.command == "resume" {
		parsed.persistent, parsed.persistentSet = true, true
	}
	destinationCwd, err := os.Getwd()
	if err != nil {
		return federationpkg.RemoteLaneResult{}, fmt.Errorf("resolve destination lane cwd: %w", err)
	}
	parent := daemonpkg.ManagedAttachment{
		ID: request.Source.ID, Product: request.Source.Entrypoint,
		NativeSessionID: request.Source.SessionID,
		// A source peer's cwd belongs to another host and may use another OS's
		// path namespace. An omitted -C inherits the destination service cwd;
		// an explicit -C is resolved by startLane against that local base.
		Cwd: destinationCwd, Groups: append([]string(nil), request.Parent.Groups...),
		PermissionMode: request.Parent.PermissionMode, State: "attached",
	}
	raw, err := c.dispatchLaneCommand(ctx, runtime, parent, request.Product, parsed, string(request.Input))
	if err != nil {
		return federationpkg.RemoteLaneResult{}, err
	}
	return federationpkg.RemoteLaneResult{Stdout: append(append([]byte(nil), raw...), '\n')}, nil
}

func localFederationCapability(product string) (string, error) {
	descriptor, ok := productcatalog.ByID(product)
	if !ok || len(descriptor.FederationCapabilities) != 1 {
		return "", fmt.Errorf("remote lane product %q is unsupported", product)
	}
	return descriptor.FederationCapabilities[0], nil
}

func authorizeFederatedLane(ctx context.Context, product, capability string) error {
	descriptor, ok := productcatalog.ByID(product)
	if !ok {
		return fmt.Errorf("remote lane product %q is unsupported", product)
	}
	if !containsString(descriptor.FederationCapabilities, capability) {
		return fmt.Errorf("remote lane capability %q does not match product %q", capability, product)
	}
	report, err := doctorLane(ctx, product, "")
	if err != nil {
		return fmt.Errorf("remote lane product %q is unavailable: %w", product, err)
	}
	ready, _ := report["ready"].(bool)
	if !ready {
		reason, _ := report["readiness_error"].(string)
		if strings.TrimSpace(reason) == "" {
			reason = "lane doctor did not report ready"
		}
		return fmt.Errorf("remote lane product %q is unavailable: %s", product, reason)
	}
	return nil
}
