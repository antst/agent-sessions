package daemon

import (
	"slices"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

type requestState struct {
	frame      protocol.Frame
	target     *entry
	held       *answer
	pending    int
	deliveries []protocol.MessageSendDelivery
	messageID  string
}

func (s *session) handleRequest(frame protocol.Frame, params any) {
	if !s.daemon.directory.current(s.identity, s) {
		s.supersede()
		return
	}
	switch frame.Method {
	case "session.list":
		s.list(frame, params.(*protocol.SessionListRequest))
	case "message.send":
		s.send(frame, params.(*protocol.MessageSendRequest))
	case "lane.describe":
		s.describe(frame, params.(*protocol.LaneDescribeRequest))
	case "lane.spawn":
		s.spawn(frame, params.(*protocol.LaneSpawnRequest))
	case "turn.run":
		s.route(frame, params.(*protocol.TurnRunRequest).SessionID, params)
	case "turn.interrupt":
		s.route(frame, params.(*protocol.SessionTarget).SessionID, params)
	case "session.close":
		s.route(frame, params.(*protocol.SessionCloseRequest).SessionID, params)
	default:
		s.reject(frame, protocol.InvalidFrame)
	}
}

func (s *session) peerHello(frame protocol.Frame, hello *protocol.PeerHello) {
	if s.identity != nil && !s.identity.peer || !validPart(hello.SessionID) || !validPart(hello.Name) || !validHost(hello.Product) {
		s.reject(frame, protocol.InvalidHello)
		return
	}
	item, displaced, ended, ok := s.daemon.directory.installPeer(s, hello, s.daemon.host)
	if !ok {
		s.reject(frame, protocol.InvalidHello)
		return
	}
	if ended != nil {
		s.settlePending(protocol.NotConnected)
		s.detachRequests(protocol.Superseded)
	}
	s.identity = item
	if displaced != nil && !displaced.wire.Post(supersedeEvent{}) {
		displaced.wire.Close()
	}
	s.result(frame, struct{}{})
}

func (s *session) list(frame protocol.Frame, input *protocol.SessionListRequest) {
	items := s.daemon.directory.visible(s.identity.row.Groups)
	if input.SessionID != "" {
		canonical, code := canonicalInput(input.SessionID, s.daemon.host)
		if code == protocol.InvalidFrame {
			s.reject(frame, code)
			return
		}
		match, ambiguous := resolveView(items, canonical)
		if code != 0 || match == nil || ambiguous {
			if code == 0 {
				code = protocol.UnknownSession
			}
			s.error(frame, code, nil)
			return
		}
		items = slices.DeleteFunc(items, func(value view) bool { return value.entry != match })
	}
	result := protocol.SessionListResult{Sessions: make([]protocol.SessionSummary, 0, len(items))}
	for _, item := range items {
		result.Sessions = append(result.Sessions, protocol.SessionSummary{
			SessionID: item.id, Kind: item.kind, Product: item.product, Name: item.name,
			Groups: item.groups, Connected: item.connected, Running: item.running, Info: item.info,
		})
	}
	if s.daemon.config.Products != nil {
		result.Hosts = []protocol.HostProducts{{Host: s.daemon.host, Products: append([]string(nil), s.daemon.config.Products...)}}
	}
	s.result(frame, result)
}

func (s *session) route(frame protocol.Frame, label string, params any) {
	reply := make(chan answer, 1)
	request := routedRequest{method: frame.Method, params: params, reply: reply}
	item, ambiguous, code := s.daemon.directory.routeLabel(label, s.daemon.host, s.identity.row.Groups, frame.Method, request)
	if code == protocol.InvalidFrame {
		s.reject(frame, code)
		return
	}
	if item == nil || ambiguous {
		if code == 0 {
			code = protocol.UnknownSession
		}
		s.error(frame, code, nil)
		return
	}
	if code != 0 {
		s.error(frame, code, nil)
		return
	}
	s.requests[frame.ID] = &requestState{frame: frame, target: item, pending: 1}
	s.await(frame.ID, 0, reply, s.identity.done)
}

func (s *session) describe(frame protocol.Frame, input *protocol.LaneDescribeRequest) {
	if input.Host != "" && input.Host != s.daemon.host {
		s.error(frame, protocol.UnknownHost, nil)
		return
	}
	if !validHost(input.Product) {
		s.error(frame, protocol.UnknownProduct, nil)
		return
	}
	start := newLaunch(input.Product, true, false)
	start.entry = &entry{row: row{Product: input.Product}, claimed: true, done: make(chan struct{})}
	if code := s.daemon.directory.reserveDescribe(start); code != 0 {
		start.timer.Stop()
		s.error(frame, code, nil)
		return
	}
	s.launchRequest(frame, start)
}

func (s *session) spawn(frame protocol.Frame, input *protocol.LaneSpawnRequest) {
	if input.Host != "" && input.Host != s.daemon.host {
		s.error(frame, protocol.UnknownHost, nil)
		return
	}
	if input.ResumeSessionID != "" {
		id, code := canonicalInput(input.ResumeSessionID, s.daemon.host)
		if code == protocol.InvalidFrame {
			s.reject(frame, code)
			return
		}
		if code != 0 {
			s.error(frame, code, nil)
			return
		}
		start := newLaunch("", false, false)
		_, code = s.daemon.directory.reserveResume(id, s.identity.row.Groups, start)
		if code != 0 {
			start.timer.Stop()
			s.error(frame, code, nil)
			return
		}
		s.launchRequest(frame, start)
		return
	}
	if !validHost(input.Product) {
		s.error(frame, protocol.UnknownProduct, nil)
		return
	}
	parentName := unqualify(s.identity.row.Name)
	composedName := parentName + "/" + input.Name
	if !validPart(input.Name) || !validPart(composedName) || input.Open == nil {
		s.reject(frame, protocol.InvalidFrame)
		return
	}
	parentGroup := privateGroup(s.identity)
	private := parentGroup + "/" + input.Name
	groups := unique(append([]string{parentGroup, private}, input.ExtraGroups...))
	value := row{Product: input.Product, Name: qualify(composedName, s.daemon.host), Groups: groups, Open: *input.Open}
	start := newLaunch(input.Product, false, true)
	_, code := s.daemon.directory.reserveFresh(value, start)
	if code != 0 {
		start.timer.Stop()
		s.error(frame, code, nil)
		return
	}
	s.launchRequest(frame, start)
}

func (s *session) launchRequest(frame protocol.Frame, start *launch) {
	s.requests[frame.ID] = &requestState{frame: frame, pending: 1}
	s.await(frame.ID, 0, start.reply, s.identity.done)
	s.daemon.startProduct(start)
}

func (s *session) send(frame protocol.Frame, input *protocol.MessageSendRequest) {
	labels := input.Targets
	if input.Target != "" {
		labels = []string{input.Target}
	}
	if input.Group != "" {
		labels = nil
		for _, item := range s.daemon.directory.visible(s.identity.row.Groups) {
			if item.entry != s.identity && slices.Contains(item.groups, input.Group) {
				labels = append(labels, item.id)
			}
		}
	}
	state := &requestState{frame: frame, messageID: randomID("message")}
	items := s.daemon.directory.visible(s.identity.row.Groups)
	targets := make([]*entry, 0, len(labels))
	seen := map[*entry]bool{}
	for _, label := range labels {
		delivery := protocol.MessageSendDelivery{Target: label}
		canonical, code := canonicalInput(label, s.daemon.host)
		if code == protocol.InvalidFrame {
			s.reject(frame, code)
			return
		}
		item, ambiguous := resolveView(items, canonical)
		if ambiguous {
			delivery.Disposition, delivery.Reason = "rejected", "ambiguous"
		} else if code != 0 || item == nil {
			delivery.Disposition, delivery.Reason = "rejected", reason(code, "unknown_session")
		} else if seen[item] {
			continue
		} else {
			seen[item] = true
		}
		state.deliveries = append(state.deliveries, delivery)
		targets = append(targets, item)
	}
	deliveryRequest := protocol.DeliveryRequest{
		MessageID: state.messageID,
		From: protocol.DeliverySource{SessionID: s.identity.row.SessionID, Name: s.identity.row.Name,
			Product: s.identity.row.Product, Groups: append([]string(nil), s.identity.row.Groups...)},
		Body: input.Message,
	}
	for index, item := range targets {
		if item == nil {
			continue
		}
		reply := make(chan answer, 1)
		request := routedRequest{method: "message.deliver", params: deliveryRequest, reply: reply}
		code := s.daemon.directory.route(item, request.method, request)
		if code != 0 {
			state.deliveries[index].Disposition = "rejected"
			state.deliveries[index].Reason = reason(code, "no_receipt")
			continue
		}
		state.deliveries[index].SessionID = item.row.SessionID
		state.deliveries[index].DeliveryID = randomID("delivery")
		state.pending++
		s.await(frame.ID, index, reply, s.identity.done)
	}
	if state.pending == 0 {
		s.result(frame, protocol.MessageSendResult{MessageID: state.messageID, Deliveries: state.deliveries})
		return
	}
	s.requests[frame.ID] = state
}

func resolveView(items []view, canonical string) (*entry, bool) {
	for _, item := range items {
		if item.id == canonical {
			return item.entry, false
		}
	}
	var match *entry
	for _, item := range items {
		if item.name == canonical {
			if match != nil {
				return nil, true
			}
			match = item.entry
		}
	}
	return match, false
}

func (s *session) consumeReply(event replyEvent) {
	if s.owned > 0 {
		s.owned--
	}
	state := s.requests[event.requestID]
	if state == nil {
		return
	}
	if state.frame.Method == "message.send" {
		delivery := &state.deliveries[event.leg]
		if event.answer.code != 0 {
			delivery.Disposition, delivery.Reason = "rejected", reason(event.answer.code, "no_receipt")
		} else if receipt, ok := event.answer.value.(*protocol.DeliveryReceipt); ok {
			delivery.Disposition, delivery.Reason = receipt.Disposition, receipt.Reason
		} else {
			delivery.Disposition, delivery.Reason = "rejected", "no_receipt"
		}
		state.pending--
		if state.pending != 0 {
			return
		}
		delete(s.requests, event.requestID)
		s.result(state.frame, protocol.MessageSendResult{MessageID: state.messageID, Deliveries: state.deliveries})
		return
	}
	if state.frame.Method == "session.close" && s.runPending(state.target) {
		value := event.answer
		state.held = &value
		return
	}
	s.finishRequest(event.requestID, state, event.answer)
}

func (s *session) finishRequest(id int64, state *requestState, result answer) {
	delete(s.requests, id)
	if result.code != 0 {
		s.error(state.frame, result.code, result.data)
	} else {
		s.result(state.frame, result.value)
	}
	if state.frame.Method != "turn.run" {
		return
	}
	for closeID, closeState := range s.requests {
		if closeState.frame.Method == "session.close" && closeState.target == state.target && closeState.held != nil {
			value := *closeState.held
			s.finishRequest(closeID, closeState, value)
			return
		}
	}
}

func (s *session) runPending(target *entry) bool {
	for _, state := range s.requests {
		if state.frame.Method == "turn.run" && state.target == target {
			return true
		}
	}
	return false
}

func (s *session) detachRequests(code int) {
	for id, state := range s.requests {
		delete(s.requests, id)
		s.error(state.frame, code, nil)
	}
}

func reason(code int, fallback string) string {
	switch code {
	case protocol.UnknownHost:
		return "unknown_host"
	case protocol.Busy:
		return "busy"
	case protocol.NotConnected:
		return "no_receipt"
	case protocol.UnknownSession:
		return "unknown_session"
	default:
		return fallback
	}
}

func unique(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}
