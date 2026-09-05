package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

const supersedeWriteBound = time.Second

func (d *Daemon) handle(_ context.Context, connection *live, request *rpc.Request) {
	if request.Method == "session.hello" {
		d.mapsMutex.Lock()
		stale := !connection.worker && connection.sessionID != "" && d.connections[connection.sessionID] != connection
		d.mapsMutex.Unlock()
		if stale {
			d.fail(connection, request, protocol.Superseded, nil)
			_ = connection.wire.Close()
			return
		}
		d.hello(connection, request)
		return
	}
	caller, code := d.caller(connection)
	if code != 0 {
		go d.fail(connection, request, code, nil)
		return
	}
	switch request.Method {
	case "session.list":
		go d.list(connection, request, caller)
	case "message.send":
		d.sendMessage(connection, request, caller)
	case "lane.describe":
		go d.describe(connection, request, caller)
	case "lane.spawn":
		go d.spawn(connection, request, caller)
	case "turn.run":
		d.run(connection, request, caller)
	case "turn.interrupt":
		d.interrupt(connection, request, caller)
	case "session.close":
		d.closeSession(connection, request, caller)
	default:
		d.fail(connection, request, protocol.InvalidFrame, nil)
		_ = connection.wire.Close()
	}
}

func (d *Daemon) hello(connection *live, request *rpc.Request) {
	switch hello := request.Params.(type) {
	case *protocol.PeerHello:
		d.peerHello(connection, request, hello)
	case *protocol.WorkerHello:
		d.workerHello(connection, request, hello)
	default:
		d.invalidHello(connection, request)
	}
}

func (d *Daemon) peerHello(connection *live, request *rpc.Request, hello *protocol.PeerHello) {
	if !validPart(hello.SessionID) || !validPart(hello.Name) || !validHost(hello.Product) {
		d.invalidHello(connection, request)
		return
	}
	id, name := qualify(hello.SessionID, d.host), qualify(hello.Name, d.host)
	declared := append([]string(nil), hello.Groups...)
	d.mapsMutex.Lock()
	_, lane := d.table.get(id)
	again := connection.sessionID != ""
	if connection.worker || lane {
		d.mapsMutex.Unlock()
		d.invalidHello(connection, request)
		return
	}
	oldID := connection.sessionID
	if again {
		if id == oldID && (connection.product != hello.Product || !slices.Equal(connection.declaredGroups, declared)) {
			d.mapsMutex.Unlock()
			d.invalidHello(connection, request)
			return
		}
		if id == oldID {
			connection.name, connection.info = name, hello.Info
			d.mapsMutex.Unlock()
			go d.result(connection, request, struct{}{})
			return
		}
		if d.connections[oldID] == connection {
			delete(d.connections, oldID)
		}
	}
	displaced := d.connections[id]
	if displaced != nil && displaced.worker {
		d.mapsMutex.Unlock()
		d.invalidHello(connection, request)
		return
	}
	oldCancel := connection.cancelIdentity
	if oldCancel != nil {
		oldCancel()
	}
	if displaced != nil && displaced != connection && displaced.cancelIdentity != nil {
		displaced.cancelIdentity()
	}
	connection.sessionID, connection.name = id, name
	connection.product, connection.declaredGroups = hello.Product, declared
	connection.privateGroup = "session:" + id
	connection.groups, connection.info = normalized(append(declared, connection.privateGroup)), hello.Info
	connection.identityCtx, connection.cancelIdentity = context.WithCancel(connection.wire.Context())
	d.connections[id] = connection
	d.mapsMutex.Unlock()
	if displaced != nil && displaced != connection {
		go d.supersede(displaced)
	}
	go d.result(connection, request, struct{}{})
}

func (d *Daemon) workerHello(connection *live, request *rpc.Request, hello *protocol.WorkerHello) {
	d.mapsMutex.Lock()
	reservation := d.reservations[hello.LaunchToken]
	if reservation != nil && reservation.product == hello.Product && connection.sessionID == "" && !connection.worker {
		reservation.product = ""
		connection.worker, connection.product = true, hello.Product
		connection.identityCtx, connection.cancelIdentity = context.WithCancel(connection.wire.Context())
		connection.startSender()
	} else {
		reservation = nil
	}
	d.mapsMutex.Unlock()
	if reservation == nil {
		d.invalidHello(connection, request)
		return
	}
	go func() {
		if connection.wire.Result(request, struct{}{}) != nil {
			_ = connection.wire.Close()
			return
		}
		reservation.hello <- workerStart{connection: connection, hello: hello.HelloDescription}
	}()
}

func (d *Daemon) caller(connection *live) (source, int) {
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	if connection.sessionID == "" && !connection.worker {
		return source{}, protocol.InvalidHello
	}
	if connection.worker && !connection.committed {
		return source{}, protocol.NotCommitted
	}
	current := d.connections[connection.sessionID] == connection
	if !current {
		return source{}, protocol.Superseded
	}
	return source{sessionID: connection.sessionID, name: connection.name, product: connection.product,
		groups: append([]string(nil), connection.groups...), private: connection.privateGroup, ctx: connection.identityCtx}, 0
}

func (d *Daemon) supersede(connection *live) {
	_ = connection.fd.SetWriteDeadline(time.Now().Add(supersedeWriteBound))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = connection.wire.Call(ctx, "session.superseded", struct{}{}, &struct{}{})
	_ = connection.wire.Close()
}

func (d *Daemon) invalidHello(connection *live, request *rpc.Request) {
	d.fail(connection, request, protocol.InvalidHello, nil)
	_ = connection.wire.Close()
}

func (d *Daemon) result(connection *live, request *rpc.Request, value any) {
	if connection.wire.Result(request, value) != nil {
		_ = connection.wire.Close()
	}
}

func (d *Daemon) fail(connection *live, request *rpc.Request, code int, data any) {
	if connection.wire.Error(request, code, data) != nil {
		_ = connection.wire.Close()
	}
}

func relayData(err error) (int, any, bool) {
	value, ok := err.(*protocol.RPCError)
	if !ok {
		return 0, nil, false
	}
	var data any
	if len(value.Data) != 0 {
		_ = json.Unmarshal(value.Data, &data)
	}
	return value.Code, data, true
}
