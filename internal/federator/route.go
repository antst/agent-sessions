package federator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// AgentFrameVersion is the product-neutral body protocol carried by every adapter.
	AgentFrameVersion      = 1
	maxAgentContent        = 1024 * 1024
	claudeAgentFramePrefix = "AGENT_SESSIONS_FRAME "
)

// AgentFrame is the complete product-neutral protocol body carried by local
// controls, Claude native messages, and federation deliveries.
type AgentFrame struct {
	Version         int      `json:"version"`
	Type            string   `json:"type"`
	MessageID       string   `json:"message_id"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Source          *Peer    `json:"source,omitempty"`
	Targets         []string `json:"targets,omitempty"`
	Group           string   `json:"group,omitempty"`
	Content         string   `json:"content,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	SentAt          string   `json:"sent_at,omitempty"`
}

// DeliveryResult is the per-recipient outcome after one request passed admission.
type DeliveryResult struct {
	Target    string `json:"target"`
	SessionID string `json:"session_id,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// AgentFrameResult is the synchronous local result for discovery or fan-out.
type AgentFrameResult struct {
	Version    int              `json:"version"`
	Type       string           `json:"type"`
	MessageID  string           `json:"message_id,omitempty"`
	Peers      []Peer           `json:"peers,omitempty"`
	Deliveries []DeliveryResult `json:"deliveries,omitempty"`
	Error      string           `json:"error,omitempty"`
}

//nolint:gocyclo // Admission and all three routing operations share one atomic validation boundary.
func (a *agent) handleAgentFrame(sourceSessionID string, frame AgentFrame) (AgentFrameResult, error) {
	if frame.Version != AgentFrameVersion {
		return AgentFrameResult{}, errors.New("agent frame protocol is incompatible")
	}
	if frame.MessageID == "" {
		return AgentFrameResult{}, errors.New("agent frame requires message_id")
	}
	if len(frame.Content) > maxAgentContent {
		return AgentFrameResult{}, errors.New("agent frame content exceeds 1 MiB")
	}
	refresh := a.refreshLocal
	if a.routeRefresh != nil {
		refresh = a.routeRefresh
	}
	if err := refresh(); err != nil {
		return AgentFrameResult{}, err
	}
	a.mu.RLock()
	source, ok := localPeerBySession(a.local, sourceSessionID)
	all := make([]Peer, 0, len(a.local)+len(a.remote))
	for _, peer := range a.local {
		all = append(all, peer.Peer)
	}
	for _, peer := range a.remote {
		all = append(all, peer)
	}
	a.mu.RUnlock()
	if !ok {
		return AgentFrameResult{}, errors.New("source session is not a live registered peer")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	result := AgentFrameResult{Version: AgentFrameVersion, MessageID: frame.MessageID}
	switch frame.Type {
	case "discover":
		if len(frame.Targets) != 0 || frame.Group != "" || frame.Content != "" {
			return AgentFrameResult{}, errors.New("discover frame contains routing fields")
		}
		result.Type = "discover.result"
		for _, peer := range all {
			if peer.ID != source.ID && groupsIntersect(source.Groups, peer.Groups) {
				result.Peers = append(result.Peers, peer)
			}
		}
		return result, nil
	case "send":
		if len(frame.Targets) == 0 || frame.Group != "" || frame.Content == "" {
			return AgentFrameResult{}, errors.New("send requires targets and content")
		}
		if duplicateStrings(frame.Targets) {
			return AgentFrameResult{}, errors.New("send targets contain duplicates")
		}
		visible := make([]Peer, 0, len(all))
		for _, peer := range all {
			if peer.ID != source.ID && groupsIntersect(source.Groups, peer.Groups) {
				visible = append(visible, peer)
			}
		}
		targets := make([]Peer, 0, len(frame.Targets))
		resolvedIDs := map[string]bool{}
		for _, raw := range frame.Targets {
			target, err := resolveGroupedTarget(raw, visible)
			if err != nil {
				return AgentFrameResult{}, err
			}
			if resolvedIDs[target.ID] {
				return AgentFrameResult{}, errors.New("send targets resolve to a duplicate peer")
			}
			resolvedIDs[target.ID] = true
			targets = append(targets, target)
		}
		result.Type = "send.result"
		result.Deliveries = a.deliverAgentFrame(source.Peer, targets, frame)
		return result, nil
	case "broadcast":
		if frame.Group == "" || len(frame.Targets) != 0 || frame.Content == "" {
			return AgentFrameResult{}, errors.New("broadcast requires group and content")
		}
		if !containsStringValue(source.Groups, frame.Group) {
			return AgentFrameResult{}, errors.New("broadcast sender is not a member of the group")
		}
		targets := []Peer{}
		for _, peer := range all {
			if peer.ID != source.ID && containsStringValue(peer.Groups, frame.Group) {
				targets = append(targets, peer)
			}
		}
		result.Type = "broadcast.result"
		result.Deliveries = a.deliverAgentFrame(source.Peer, targets, frame)
		return result, nil
	default:
		return AgentFrameResult{}, fmt.Errorf("unsupported agent frame type %q", frame.Type)
	}
}

//nolint:gocyclo // Durable source reconstruction and ordinary grouped admission remain one auditable transaction.
func (a *agent) routeTerminalNotice(sourceSessionID, requestedTarget string, body json.RawMessage) (AgentFrameResult, error) {
	preference, sourceGroups, ok, err := a.catalog.get(sourceSessionID)
	if err != nil {
		return AgentFrameResult{}, err
	}
	if !ok {
		return AgentFrameResult{}, errors.New("terminal notice source is not in the durable catalog")
	}
	requestedTarget = strings.TrimPrefix(strings.TrimSpace(requestedTarget), "session:")
	if requestedTarget == "" {
		return AgentFrameResult{}, errors.New("orphan terminal notice target is empty")
	}
	var request AgentFrame
	if json.Unmarshal(body, &request) != nil || request.Version != AgentFrameVersion || request.Type != "send" ||
		request.MessageID == "" || request.Content == "" || len(request.Targets) != 0 || request.Group != "" {
		return AgentFrameResult{}, errors.New("invalid orphan terminal notice frame")
	}
	refresh := a.refreshLocal
	if a.routeRefresh != nil {
		refresh = a.routeRefresh
	}
	if err := refresh(); err != nil {
		return AgentFrameResult{}, err
	}
	a.mu.RLock()
	all := make([]Peer, 0, len(a.local)+len(a.remote))
	for _, peer := range a.local {
		all = append(all, peer.Peer)
	}
	for _, peer := range a.remote {
		all = append(all, peer)
	}
	a.mu.RUnlock()
	visible := make([]Peer, 0, len(all))
	for _, peer := range all {
		if groupsIntersect(sourceGroups, peer.Groups) {
			visible = append(visible, peer)
		}
	}
	target, err := resolveGroupedTarget(requestedTarget, visible)
	if err != nil {
		return AgentFrameResult{}, err
	}
	source := Peer{
		ID: parentHostAwarePeerID(a.options.HostID, sourceSessionID), HostID: a.options.HostID,
		HostName: a.options.HostName, SessionID: sourceSessionID, GlobalID: globalSessionID(a.options.HostID, sourceSessionID),
		Name: sourceSessionID, DisplayName: qualifiedName(sourceSessionID, a.options.HostName), Entrypoint: preference.Product,
		PermissionMode: map[bool]string{true: "bypassPermissions", false: "default"}[preference.AlwaysApprove],
		PeerProtocol:   GroupProtocolVersion, Groups: sourceGroups, ParentSessionID: preference.ParentSession,
		InstanceID: "terminal-" + sessionKey(a.options.HostID+"\x00"+sourceSessionID),
	}
	result := AgentFrameResult{Version: AgentFrameVersion, Type: "send.result", MessageID: request.MessageID}
	if target.HostID == a.options.HostID {
		result.Deliveries = a.deliverAgentFrame(source, []Peer{target}, request)
	} else {
		delivery := DeliveryResult{Target: target.ID, SessionID: target.SessionID, Status: "accepted"}
		if err := a.deliverTerminalNoticeRemote(source, target, request); err != nil {
			delivery.Status, delivery.Error = "failed", err.Error()
		}
		result.Deliveries = []DeliveryResult{delivery}
	}
	return result, nil
}

func parentHostAwarePeerID(hostID, sessionID string) string {
	return hostID + "/" + sessionID
}

func (a *agent) handleNativeCarrierFrame(raw json.RawMessage) (AgentFrameResult, error) {
	refresh := a.refreshLocal
	if a.routeRefresh != nil {
		refresh = a.routeRefresh
	}
	if err := refresh(); err != nil {
		return AgentFrameResult{}, err
	}
	a.mu.RLock()
	sourceID, err := sourcePeerID(raw, a.local)
	var source localPeer
	if err == nil {
		source = a.local[sourceID]
	}
	a.mu.RUnlock()
	if err != nil {
		return AgentFrameResult{}, err
	}
	var outer map[string]any
	if json.Unmarshal(raw, &outer) != nil {
		return AgentFrameResult{}, errors.New("invalid Claude carrier frame")
	}
	message, _ := outer["message"].(map[string]any)
	content, _ := message["content"].(string)
	body, err := unwrapClaudeCarrierBody(content)
	if err != nil {
		return AgentFrameResult{}, err
	}
	if !strings.HasPrefix(body, claudeAgentFramePrefix) {
		return AgentFrameResult{}, errors.New("claude carrier body is missing the Agent Sessions frame marker")
	}
	frame, err := DecodeAgentFrameBody(content)
	if err != nil {
		return AgentFrameResult{}, err
	}
	if frame.SourceSessionID != "" && frame.SourceSessionID != source.SessionID {
		return AgentFrameResult{}, errors.New("agent frame source does not match the native sender")
	}
	frame.SourceSessionID = source.SessionID
	result, err := a.handleAgentFrame(source.SessionID, frame)
	if err != nil {
		failure := AgentFrameResult{
			Version: AgentFrameVersion, Type: "error", MessageID: frame.MessageID, Error: err.Error(),
		}
		_ = a.deliverNativeCarrierResult(source, failure)
		return AgentFrameResult{}, err
	}
	if err := a.deliverNativeCarrierResult(source, result); err != nil {
		return AgentFrameResult{}, err
	}
	return result, nil
}

func (a *agent) deliverNativeCarrierResult(target localPeer, result AgentFrameResult) error {
	inner, err := json.Marshal(result)
	if err != nil {
		return err
	}
	content := wrapAgentFrameInClaudeEnvelope(a.controlPath, a.serviceSessionID(), a.serviceName(), inner)
	native, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": result.MessageID, "type": "user", "priority": "next",
		"from":    encodeUDS(a.controlPath),
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return err
	}
	return sendUnixFrame(target.Socket, native, 5*time.Second)
}

func (a *agent) deliverAgentFrame(source Peer, targets []Peer, request AgentFrame) []DeliveryResult {
	delivered := AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: request.MessageID,
		SourceSessionID: source.SessionID, Source: &source, Content: request.Content,
		Group: request.Group, Summary: request.Summary, SentAt: request.SentAt,
	}
	if delivered.SentAt == "" {
		delivered.SentAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	results := make([]DeliveryResult, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		index, target := index, target
		wait.Add(1)
		go func() {
			defer wait.Done()
			result := DeliveryResult{Target: target.ID, SessionID: target.SessionID, Status: "accepted"}
			var err error
			if target.HostID == a.options.HostID {
				a.mu.RLock()
				local, exists := a.local[target.ID]
				a.mu.RUnlock()
				if !exists {
					err = errors.New("local target is no longer live")
				} else {
					err = a.deliverAgentFrameLocal(local, delivered)
				}
			} else {
				err = a.deliverAgentFrameRemote(source, target, delivered)
			}
			if err != nil {
				result.Status, result.Error = "failed", err.Error()
			}
			results[index] = result
		}()
	}
	wait.Wait()
	return results
}

func (a *agent) deliverAgentFrameLocal(target localPeer, frame AgentFrame) error {
	inner, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	content := wrapAgentFrameInClaudeEnvelope(a.controlPath, a.serviceSessionID(), a.serviceName(), inner)
	native, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": frame.MessageID, "type": "user", "priority": "next",
		"from":    encodeUDS(a.controlPath),
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return err
	}
	return sendUnixFrame(target.Socket, native, 5*time.Second)
}

func (a *agent) deliverAgentFrameRemote(source, target Peer, frame AgentFrame) error {
	inner, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	a.mu.RLock()
	wire := a.network
	_, exists := a.remote[target.ID]
	a.mu.RUnlock()
	if wire == nil {
		return errors.New("destination host is disconnected")
	}
	if !exists {
		return errors.New("remote target is no longer live")
	}
	return a.sendRemoteDelivery(wire, Message{Type: "group_deliver", SourceID: source.ID, TargetID: target.ID, Frame: inner})
}

func (a *agent) deliverTerminalNoticeRemote(source, target Peer, request AgentFrame) error {
	delivered := AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: request.MessageID,
		SourceSessionID: source.SessionID, Source: &source, Content: request.Content,
		Summary: request.Summary, SentAt: request.SentAt,
	}
	if delivered.SentAt == "" {
		delivered.SentAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	inner, err := json.Marshal(delivered)
	if err != nil {
		return err
	}
	a.mu.RLock()
	wire := a.network
	_, targetExists := a.remote[target.ID]
	a.mu.RUnlock()
	if wire == nil {
		return errors.New("destination host is disconnected")
	}
	if !targetExists {
		return errors.New("remote parent is no longer live")
	}
	return a.sendRemoteDelivery(wire, Message{Type: "terminal_notice_deliver", SourceID: source.ID, TargetID: target.ID, Frame: inner})
}

func (a *agent) sendRemoteDelivery(wire *wireConn, message Message) error {
	requestID, err := randomLaneRequestID(a.options.HostID)
	if err != nil {
		return err
	}
	message.RequestID = requestID
	result := make(chan error, 1)
	a.deliveryMu.Lock()
	if a.pendingDeliveries == nil {
		a.pendingDeliveries = map[string]chan error{}
	}
	a.pendingDeliveries[requestID] = result
	a.deliveryMu.Unlock()
	if err := wire.Send(message); err != nil {
		a.deliveryMu.Lock()
		delete(a.pendingDeliveries, requestID)
		a.deliveryMu.Unlock()
		return err
	}
	select {
	case err := <-result:
		return err
	case <-time.After(wireWriteTimeout):
		a.deliveryMu.Lock()
		delete(a.pendingDeliveries, requestID)
		a.deliveryMu.Unlock()
		return errors.New("remote delivery acknowledgement timed out")
	}
}

func (a *agent) deliverGroupedLocal(message Message) error {
	if err := a.refreshLocal(); err != nil {
		return err
	}
	a.mu.RLock()
	target, targetExists := a.local[message.TargetID]
	source, sourceExists := a.remote[message.SourceID]
	a.mu.RUnlock()
	if !targetExists || !sourceExists {
		return errors.New("federated source or local target is no longer live")
	}
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" {
		return errors.New("invalid federated agent frame")
	}
	if frame.Source == nil || frame.Source.ID != source.ID || frame.SourceSessionID != source.SessionID {
		return errors.New("federated agent frame source mismatch")
	}
	if err := validateCurrentDeliveryGroups(source, target.Peer, frame); err != nil {
		return err
	}
	frame.Source = &source
	return a.deliverAgentFrameLocal(target, frame)
}

func (a *agent) deliverTerminalNoticeLocal(message Message) error {
	if err := a.refreshLocal(); err != nil {
		return err
	}
	a.mu.RLock()
	target, targetExists := a.local[message.TargetID]
	a.mu.RUnlock()
	if !targetExists {
		return errors.New("terminal notice target is no longer live")
	}
	var frame AgentFrame
	if json.Unmarshal(message.Frame, &frame) != nil || frame.Version != AgentFrameVersion || frame.Type != "delivery" ||
		frame.Source == nil || frame.Source.ID != message.SourceID || frame.SourceSessionID != frame.Source.SessionID {
		return errors.New("invalid federated terminal notice frame")
	}
	if err := validateGroupedSnapshotPeer(*frame.Source, frame.Source.HostID); err != nil {
		return err
	}
	if frame.Source.HostID == a.options.HostID || target.HostID != a.options.HostID {
		return errors.New("federated terminal notice has an invalid host boundary")
	}
	if err := validateCurrentDeliveryGroups(*frame.Source, target.Peer, frame); err != nil {
		return err
	}
	return a.deliverAgentFrameLocal(target, frame)
}

func validateCurrentDeliveryGroups(source, target Peer, frame AgentFrame) error {
	if frame.Group != "" {
		if !containsStringValue(source.Groups, frame.Group) || !containsStringValue(target.Groups, frame.Group) {
			return errors.New("federated broadcast peers are not current members of the named group")
		}
		return nil
	}
	if !groupsIntersect(source.Groups, target.Groups) {
		return errors.New("federated peers do not share a group")
	}
	return nil
}

func localPeerBySession(peers map[string]localPeer, sessionID string) (localPeer, bool) {
	for _, peer := range peers {
		if peer.SessionID == sessionID {
			return peer, true
		}
	}
	return localPeer{}, false
}

func resolveGroupedTarget(raw string, peers []Peer) (Peer, error) {
	matches := []Peer{}
	for _, peer := range peers {
		if raw == peer.ID || raw == peer.GlobalID || raw == peer.SessionID || raw == peer.Name || raw == peer.DisplayName {
			matches = append(matches, peer)
		}
	}
	if len(matches) == 0 {
		return Peer{}, fmt.Errorf("no live peer matches %q", raw)
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, peer := range matches {
			ids = append(ids, peer.ID)
		}
		sort.Strings(ids)
		return Peer{}, fmt.Errorf("peer %q is ambiguous: %s", raw, strings.Join(ids, ", "))
	}
	return matches[0], nil
}

func groupsIntersect(first, second []string) bool {
	seen := make(map[string]bool, len(first))
	for _, group := range first {
		seen[group] = true
	}
	for _, group := range second {
		if seen[group] {
			return true
		}
	}
	return false
}

func duplicateStrings(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func containsStringValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (a *agent) serviceSessionID() string {
	return "agent-" + sessionKey(a.options.HostID)
}

func (a *agent) serviceName() string {
	return "agent-sessions--" + cleanPeerName(a.options.HostName)
}

func wrapAgentFrameInClaudeEnvelope(socket, sessionID, name string, inner []byte) string {
	inner = escapeAgentEnvelopeJSON(inner)
	return `<cross-session-message from="` + safeAttribute(encodeUDS(socket)) + `" from-session="` +
		safeAttribute(sessionID) + `" from-name="` + safeAttribute(name) + `" from-mode="prompting">` +
		"\n" + string(inner) + "\n</cross-session-message>"
}

// DecodeAgentFrameBody extracts one Agent Sessions frame from the body of the
// single Claude-native carrier. Product adapters use the result as their
// trusted, product-neutral delivery input.
func DecodeAgentFrameBody(content string) (AgentFrame, error) {
	body, err := unwrapClaudeCarrierBody(content)
	if err != nil {
		return AgentFrame{}, err
	}
	body = strings.TrimPrefix(body, claudeAgentFramePrefix)
	var frame AgentFrame
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		return AgentFrame{}, fmt.Errorf("decode agent frame: %w", err)
	}
	return frame, nil
}

func unwrapClaudeCarrierBody(content string) (string, error) {
	body := content
	if strings.HasPrefix(body, "<cross-session-message") {
		newline := strings.IndexByte(body, '\n')
		const closeTag = "\n</cross-session-message>"
		if newline < 0 || !strings.HasSuffix(body, closeTag) {
			return "", errors.New("invalid Claude carrier envelope")
		}
		body = body[newline+1 : len(body)-len(closeTag)]
	}
	return body, nil
}

func escapeAgentEnvelopeJSON(body []byte) []byte {
	lower := bytes.ToLower(body)
	needle := []byte("</cross-session-message")
	for {
		index := bytes.Index(lower, needle)
		if index < 0 {
			return body
		}
		body = append(append(append([]byte(nil), body[:index+1]...), '\\'), body[index+1:]...)
		lower = bytes.ToLower(body)
	}
}
