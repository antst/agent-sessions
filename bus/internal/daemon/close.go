package daemon

import (
	"errors"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

const closeBound = 10 * time.Second

func (d *Daemon) closeSession(connection *live, request *rpc.Request, caller source) {
	input := request.Params.(*protocol.SessionCloseRequest)
	item, ambiguous, code := d.resolve(input.SessionID, d.visibleSessions(caller))
	if code != 0 || item == nil || ambiguous || item.kind != "lane" {
		if code == 0 {
			code = protocol.UnknownSession
		}
		go d.fail(connection, request, code, nil)
		return
	}
	lock := d.rowLock(item.id)
	lock.Lock()
	value, ok := d.table.get(item.id)
	if !ok || !shares(caller.groups, value.Groups) {
		lock.Unlock()
		go d.fail(connection, request, protocol.UnknownSession, nil)
		return
	}
	d.mapsMutex.Lock()
	target := d.connections[value.SessionID]
	d.mapsMutex.Unlock()
	if target == nil {
		lock.Unlock()
		go d.fail(connection, request, protocol.NotConnected, nil)
		return
	}
	var output struct{}
	call, code := d.enqueue(target, "session.close", input, &output, nil)
	if code != 0 {
		lock.Unlock()
		go d.fail(connection, request, protocol.Busy, nil)
		return
	}
	go d.finishClose(connection, request, caller, value, target, call, output, input.Forget, lock, time.Now().Add(closeBound))
}

func (d *Daemon) finishClose(connection *live, request *rpc.Request, caller source, value row, target *live, call *workerCall, output struct{}, forget bool, lock *sync.Mutex, deadline time.Time) {
	defer lock.Unlock()
	timer := time.NewTimer(max(0, time.Until(deadline)))
	var err error
	select {
	case err = <-call.done:
	case <-timer.C:
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	_ = target.wire.Close()
	target.child.Stop(max(0, time.Until(deadline)))
	d.mapsMutex.Lock()
	if d.connections[value.SessionID] == target {
		delete(d.connections, value.SessionID)
		delete(d.pending, value.SessionID)
	}
	d.mapsMutex.Unlock()
	if forget {
		if deleteErr := d.table.delete(value.SessionID); deleteErr != nil {
			d.fail(connection, request, protocol.Internal, nil)
			return
		}
	}
	if caller.ctx.Err() != nil {
		return
	}
	if code, data, ok := relayData(err); ok && code == protocol.Internal {
		d.fail(connection, request, code, data)
	} else if err != nil && !errors.Is(err, rpc.ErrClosed) {
		d.fail(connection, request, protocol.Internal, nil)
	} else {
		d.result(connection, request, output)
	}
}
