package daemon

import (
	"errors"
	"syscall"
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
		d.fail(connection, request, code, nil)
		return
	}
	value, ok := d.table.get(item.id)
	if !ok {
		d.fail(connection, request, protocol.UnknownSession, nil)
		return
	}
	lock := d.rowLock(value.SessionID)
	lock.Lock()
	defer lock.Unlock()
	d.mapsMutex.Lock()
	target := d.connections[value.SessionID]
	d.mapsMutex.Unlock()
	if target == nil {
		d.fail(connection, request, protocol.NotConnected, nil)
		return
	}
	var output struct{}
	call, queued := target.enqueue("session.close", input, &output, nil)
	if !queued {
		d.fail(connection, request, protocol.Busy, nil)
		return
	}
	timer := time.NewTimer(closeBound)
	defer timer.Stop()
	var err error
	expired := false
	select {
	case started := <-call.started:
		select {
		case err = <-started:
		case <-timer.C:
			expired = true
		}
	case <-timer.C:
		expired = true
	}
	_ = target.wire.Close()
	if !expired {
		target.child.Signal(syscall.SIGTERM)
		select {
		case <-target.child.Done():
		case <-timer.C:
			expired = true
		}
	}
	if expired {
		target.child.KillAndWait()
	}
	d.mapsMutex.Lock()
	if d.connections[value.SessionID] == target {
		delete(d.connections, value.SessionID)
		delete(d.pending, value.SessionID)
	}
	d.mapsMutex.Unlock()
	if input.Forget {
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
	} else if err != nil && !expired && !errors.Is(err, rpc.ErrClosed) {
		d.fail(connection, request, protocol.Internal, nil)
	} else {
		d.result(connection, request, output)
	}
}
