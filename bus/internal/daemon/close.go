package daemon

import (
	"context"
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
	run := d.pending[value.SessionID]
	d.mapsMutex.Unlock()
	if target == nil {
		d.fail(connection, request, protocol.NotConnected, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeBound)
	defer cancel()
	var output struct{}
	err := target.wire.Call(ctx, "session.close", input, &output)
	_ = target.wire.Close()
	if err == nil {
		target.child.signal(syscall.SIGTERM)
	}
	select {
	case <-target.child.done:
	case <-ctx.Done():
		target.child.signal(syscall.SIGKILL)
		<-target.child.done
	}
	d.mapsMutex.Lock()
	if d.connections[value.SessionID] == target {
		delete(d.connections, value.SessionID)
		delete(d.pending, value.SessionID)
	}
	d.mapsMutex.Unlock()
	if run != nil {
		<-run.done
	}
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
	} else if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, rpc.ErrClosed) {
		d.fail(connection, request, protocol.Internal, nil)
	} else {
		d.result(connection, request, output)
	}
}
