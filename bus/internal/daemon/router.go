package daemon

import (
	"context"
	"crypto/rand"
	"regexp"
	"slices"
	"strings"
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
	ctx                     context.Context
	running                 bool
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
	seen := map[string]bool{}
	for _, label := range labels {
		item, ambiguous, code := d.resolve(label, visible)
		if code != 0 {
			d.fail(connection, request, code, nil)
			return
		}
		if ambiguous {
			result.Deliveries = append(result.Deliveries, protocol.MessageSendDelivery{Target: label, Disposition: "rejected", Reason: "ambiguous"})
			continue
		}
		if item == nil {
			d.fail(connection, request, protocol.UnknownSession, nil)
			return
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
			var receipt protocol.DeliveryReceipt
			if err := item.connection.wire.Call(item.ctx, "message.deliver", params, &receipt); err != nil {
				delivery.Disposition, delivery.Reason = "rejected", "no_receipt"
			} else {
				delivery.Disposition, delivery.Reason = receipt.Disposition, receipt.Reason
			}
		}
		result.Deliveries = append(result.Deliveries, delivery)
	}
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
	pending := &pendingRun{target: target.connection, caller: caller.live, done: make(chan struct{})}
	d.pending[target.id] = pending
	d.mapsMutex.Unlock()
	go d.finishRun(connection, request, caller, target, pending)
}

func (d *Daemon) finishRun(connection *live, request *rpc.Request, caller source, target *session, pending *pendingRun) {
	defer close(pending.done)
	var output protocol.TurnResult
	err := target.connection.wire.Call(target.ctx, "turn.run", request.Params, &output)
	d.mapsMutex.Lock()
	if d.pending[target.id] == pending {
		delete(d.pending, target.id)
	}
	d.mapsMutex.Unlock()
	if caller.ctx.Err() != nil {
		return
	}
	if code, data, ok := relayData(err); ok {
		d.fail(connection, request, code, data)
	} else if err != nil {
		d.fail(connection, request, protocol.NotConnected, nil)
	} else {
		d.result(connection, request, output)
	}
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
	go func() {
		var output struct{}
		err := target.connection.wire.Call(target.ctx, "turn.interrupt", request.Params, &output)
		if code, data, ok := relayData(err); ok {
			d.fail(connection, request, code, data)
		} else if err != nil {
			d.fail(connection, request, protocol.NotConnected, nil)
		} else {
			d.result(connection, request, output)
		}
	}()
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
	rows := d.table.list()
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	var sessions []session
	for _, value := range rows {
		connection := d.connections[value.SessionID]
		item := session{}
		if connection == nil {
			item = session{id: value.SessionID, name: value.Name, kind: "lane", product: value.Product, groups: value.Groups}
		} else {
			item = sessionOf(connection, d.pending[value.SessionID] != nil)
			item.kind = "lane"
		}
		if shares(caller.groups, item.groups) {
			sessions = append(sessions, item)
		}
	}
	for id, connection := range d.connections {
		item := sessionOf(connection, d.pending[id] != nil)
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
		host := value[index+1:]
		if !validHost(host) || host != d.host {
			return "", protocol.UnknownHost
		}
		return value, 0
	}
	return qualify(value, d.host), 0
}

func sessionOf(connection *live, running bool) session {
	return session{id: connection.sessionID, name: connection.name, kind: connection.role, product: connection.product,
		groups: append([]string(nil), connection.groups...), info: connection.info, connection: connection, ctx: connection.identityCtx, running: running}
}

func validPart(value string) bool {
	length := utf8.RuneCountInString(value)
	return length > 0 && length <= 128 && !strings.ContainsFunc(value, func(character rune) bool {
		return !unicode.IsPrint(character) || unicode.IsSpace(character) || unicode.IsControl(character)
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
