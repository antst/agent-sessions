package daemon

import (
	"context"
	"crypto/rand"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

type session struct {
	id, name, kind, product string
	groups                  []string
	info                    map[string]any
	connection              *live
	running                 bool
	ctx                     context.Context
}

type deliveryCall struct {
	index      int
	connection *live
	call       *workerCall
	receipt    *protocol.DeliveryReceipt
	ctx        context.Context
}

func (d *Daemon) list(connection *live, request *rpc.Request, caller source) {
	query := request.Params.(*protocol.SessionListRequest)
	sessions := d.visibleSessions(caller)
	if query.SessionID != "" {
		resolved, ambiguous, code := d.resolve(query.SessionID, sessions)
		if code == 0 && (resolved == nil || ambiguous) {
			code = protocol.UnknownSession
		}
		if code != 0 {
			d.fail(connection, request, code, nil)
			return
		}
		sessions = []session{*resolved}
	}
	result := protocol.SessionListResult{Sessions: make([]protocol.SessionSummary, 0, len(sessions))}
	for _, item := range sessions {
		result.Sessions = append(result.Sessions, protocol.SessionSummary{SessionID: item.id, Kind: item.kind, Product: item.product,
			Name: item.name, Groups: item.groups, Connected: item.connection != nil, Running: item.running, Info: item.info})
	}
	if d.config.Products != nil {
		result.Hosts = []protocol.HostProducts{{Host: d.host, Products: append([]string(nil), d.config.Products...)}}
	}
	d.result(connection, request, result)
}

func (d *Daemon) sendMessage(connection *live, request *rpc.Request, caller source) {
	input := request.Params.(*protocol.MessageSendRequest)
	visible := d.visibleSessions(caller)
	labels := input.Targets
	if input.Group != "" {
		labels = nil
		for index := range visible {
			if visible[index].connection != nil && slices.Contains(visible[index].groups, input.Group) {
				labels = append(labels, visible[index].id)
			}
		}
	} else if input.Target != "" {
		labels = []string{input.Target}
	}
	messageID := randomID("message")
	result := protocol.MessageSendResult{MessageID: messageID}
	var calls []deliveryCall
	seen := map[string]bool{}
	for _, label := range labels {
		_, code := d.canonicalInput(label)
		if code == protocol.InvalidFrame {
			d.fail(connection, request, code, nil)
			return
		}
	}
	for _, label := range labels {
		item, ambiguous, code := d.resolve(label, visible)
		if code != 0 || item == nil || ambiguous {
			reason := "unknown_session"
			if code == protocol.UnknownHost {
				reason = "unknown_host"
			} else if ambiguous {
				reason = "ambiguous"
			}
			result.Deliveries = append(result.Deliveries, protocol.MessageSendDelivery{Target: label, Disposition: "rejected", Reason: reason})
			continue
		}
		if seen[item.id] {
			continue
		}
		seen[item.id] = true
		delivery := protocol.MessageSendDelivery{Target: label, SessionID: item.id, DeliveryID: randomID("delivery")}
		if item.connection == nil {
			delivery.Disposition, delivery.Reason = "rejected", "not_connected"
		} else {
			params := protocol.DeliveryRequest{MessageID: messageID, From: protocol.DeliverySource{SessionID: caller.sessionID,
				Name: caller.name, Product: caller.product, Groups: caller.groups}, Body: input.Message}
			receipt := &protocol.DeliveryReceipt{}
			call, code := d.enqueue(item.connection, "message.deliver", params, receipt, nil)
			if code != 0 {
				delivery.Disposition, delivery.Reason = "rejected", "no_receipt"
				if code == protocol.Busy {
					delivery.Reason = "busy"
				}
			} else {
				calls = append(calls, deliveryCall{index: len(result.Deliveries), connection: item.connection, call: call, receipt: receipt, ctx: item.ctx})
			}
		}
		result.Deliveries = append(result.Deliveries, delivery)
	}
	go d.finishMessage(connection, request, caller, result, calls)
}

func (d *Daemon) finishMessage(connection *live, request *rpc.Request, caller source, result protocol.MessageSendResult, calls []deliveryCall) {
	var waits sync.WaitGroup
	for _, pending := range calls {
		waits.Add(1)
		go func() {
			defer waits.Done()
			delivery := &result.Deliveries[pending.index]
			if d.waitCallContext(pending.connection, pending.call, pending.ctx) != nil {
				delivery.Disposition, delivery.Reason = "rejected", "no_receipt"
			} else {
				delivery.Disposition, delivery.Reason = pending.receipt.Disposition, pending.receipt.Reason
			}
		}()
	}
	waits.Wait()
	if caller.ctx.Err() == nil {
		d.result(connection, request, result)
	}
}

func (d *Daemon) run(connection *live, request *rpc.Request, caller source) {
	target, code := d.connectedLane(caller, request.Params.(*protocol.TurnRunRequest).SessionID)
	if code != 0 {
		go d.fail(connection, request, code, nil)
		return
	}
	d.mapsMutex.Lock()
	if d.pending[target.id] != nil {
		d.mapsMutex.Unlock()
		go d.fail(connection, request, protocol.Busy, nil)
		return
	}
	pending := &pendingRun{}
	d.pending[target.id] = pending
	var output protocol.TurnResult
	call, code := d.enqueueLocked(target.connection, "turn.run", request.Params, &output, nil)
	if code != 0 {
		delete(d.pending, target.id)
		d.mapsMutex.Unlock()
		go d.fail(connection, request, protocol.Busy, nil)
		return
	}
	d.mapsMutex.Unlock()
	go d.finishRun(connection, request, caller, target, pending, call, &output)
}

func (d *Daemon) finishRun(connection *live, request *rpc.Request, caller source, target *session, pending *pendingRun, call *workerCall, output *protocol.TurnResult) {
	err := d.waitCall(target.connection, call)
	d.mapsMutex.Lock()
	if d.pending[target.id] == pending {
		delete(d.pending, target.id)
	}
	d.mapsMutex.Unlock()
	d.replyCall(connection, request, caller, err, *output)
}

func (d *Daemon) interrupt(connection *live, request *rpc.Request, caller source) {
	target, code := d.connectedLane(caller, request.Params.(*protocol.SessionTarget).SessionID)
	if code != 0 {
		go d.fail(connection, request, code, nil)
		return
	}
	d.mapsMutex.Lock()
	running := d.pending[target.id] != nil
	d.mapsMutex.Unlock()
	if !running {
		go d.fail(connection, request, protocol.NotRunning, nil)
		return
	}
	var output struct{}
	call, code := d.enqueue(target.connection, "turn.interrupt", request.Params, &output, nil)
	if code != 0 {
		go d.fail(connection, request, protocol.Busy, nil)
		return
	}
	go func() {
		err := d.waitCall(target.connection, call)
		d.replyCall(connection, request, caller, err, output)
	}()
}

func (d *Daemon) replyCall(connection *live, request *rpc.Request, caller source, err error, value any) {
	if caller.ctx.Err() != nil {
		return
	}
	if code, data, ok := relayData(err); ok {
		d.fail(connection, request, code, data)
	} else if err != nil {
		d.fail(connection, request, protocol.NotConnected, nil)
	} else {
		d.result(connection, request, value)
	}
}

func (d *Daemon) connectedLane(caller source, value string) (*session, int) {
	item, ambiguous, code := d.resolve(value, d.visibleSessions(caller))
	if code != 0 {
		return nil, code
	}
	if item == nil || ambiguous || item.kind != "lane" {
		return nil, protocol.UnknownSession
	}
	if item.connection == nil {
		return nil, protocol.NotConnected
	}
	return item, 0
}

func (d *Daemon) visibleSessions(caller source) []session {
	d.mapsMutex.Lock()
	liveSessions := make(map[string]session, len(d.connections))
	for id, connection := range d.connections {
		if !connection.worker || connection.committed {
			liveSessions[id] = sessionOf(connection, d.pending[id] != nil)
		}
	}
	rows := d.table.list()
	d.mapsMutex.Unlock()
	var sessions []session
	for _, value := range rows {
		item, connected := liveSessions[value.SessionID]
		if !connected {
			item = session{id: value.SessionID, name: value.Name, kind: "lane", product: value.Product, groups: value.Groups}
		} else {
			item.kind = "lane"
			delete(liveSessions, value.SessionID)
		}
		if shares(caller.groups, item.groups) {
			sessions = append(sessions, item)
		}
	}
	for _, item := range liveSessions {
		if item.kind == "peer" && shares(caller.groups, item.groups) {
			sessions = append(sessions, item)
		}
	}
	slices.SortFunc(sessions, func(a, b session) int { return strings.Compare(a.id, b.id) })
	return sessions
}

func (d *Daemon) resolve(value string, sessions []session) (*session, bool, int) {
	canonical, code := d.canonicalInput(value)
	if code != 0 {
		return nil, false, code
	}
	for index := range sessions {
		if sessions[index].id == canonical {
			return &sessions[index], false, 0
		}
	}
	var found *session
	for index := range sessions {
		if sessions[index].name == canonical {
			if found != nil {
				return nil, true, 0
			}
			found = &sessions[index]
		}
	}
	return found, false, 0
}

func (d *Daemon) canonicalInput(value string) (string, int) {
	if index := strings.LastIndexByte(value, '@'); index >= 0 {
		if !validPart(value[:index]) {
			return "", protocol.InvalidFrame
		}
		host := value[index+1:]
		if !validHost(host) || host != d.host {
			return "", protocol.UnknownHost
		}
		return value, 0
	}
	if !validPart(value) {
		return "", protocol.InvalidFrame
	}
	return qualify(value, d.host), 0
}

func sessionOf(connection *live, running bool) session {
	kind := "peer"
	if connection.worker {
		kind = "lane"
	}
	return session{id: connection.sessionID, name: connection.name, kind: kind, product: connection.product,
		groups: append([]string(nil), connection.groups...), info: connection.info, connection: connection, running: running, ctx: connection.identityCtx}
}

func validPart(value string) bool {
	length := utf8.RuneCountInString(value)
	return length > 0 && length <= 128 && !strings.ContainsFunc(value, func(character rune) bool {
		return !unicode.IsPrint(character) || unicode.IsSpace(character)
	})
}

var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validHost(value string) bool       { return hostPattern.MatchString(value) }
func qualify(value, host string) string { return value + "@" + host }
func shares(left, right []string) bool {
	return slices.ContainsFunc(left, func(value string) bool { return slices.Contains(right, value) })
}
func normalized(values []string) []string {
	copy := append([]string(nil), values...)
	slices.Sort(copy)
	return slices.Compact(copy)
}
func randomID(prefix string) string {
	return prefix + "-" + rand.Text()
}
