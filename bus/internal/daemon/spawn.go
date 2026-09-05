package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
)

const spawnTransactionTimeout = 60 * time.Second

type launchResult struct {
	connection *live
	hello      protocol.HelloDescription
	process    *process
	err        error
	code       int
	data       any
}

func (d *Daemon) describe(connection *live, request *rpc.Request, caller source) {
	input := request.Params.(*protocol.LaneDescribeRequest)
	if code := d.localProduct(input.Product, input.Host); code != 0 {
		d.fail(connection, request, code, nil)
		return
	}
	result, code, data := d.launch(input.Product, nil, nil)
	if code != 0 {
		d.fail(connection, request, code, data)
		return
	}
	d.stopProbe(result)
	if caller.ctx.Err() == nil {
		d.result(connection, request, result.hello)
	}
}

func (d *Daemon) spawn(connection *live, request *rpc.Request, caller source) {
	input := request.Params.(*protocol.LaneSpawnRequest)
	value, fresh, code := d.spawnRow(caller, input)
	if code != 0 {
		d.fail(connection, request, code, nil)
		return
	}
	key := value.Name
	if !fresh {
		key = value.SessionID
	}
	lock := d.rowLock(key)
	lock.Lock()
	defer lock.Unlock()
	if fresh {
		if _, exists := d.table.byName(value.Name); exists {
			d.fail(connection, request, protocol.NameTaken, nil)
			return
		}
	} else {
		current, exists := d.table.get(value.SessionID)
		if !exists || !shares(caller.groups, current.Groups) {
			d.fail(connection, request, protocol.UnknownSession, nil)
			return
		}
		value = current
		d.mapsMutex.Lock()
		connected := d.connections[value.SessionID] != nil
		d.mapsMutex.Unlock()
		if connected {
			d.fail(connection, request, protocol.AlreadyConnected, nil)
			return
		}
	}
	private := lanePrivate(value)
	resume := ""
	if !fresh {
		resume = unqualify(value.SessionID)
	}
	open := protocol.OpenRequest{Name: value.Name, Groups: value.Groups, ResumeSessionID: resume, Open: value.Open}
	commit := func(connection *live, child *process, opened protocol.OpenResult) (int, any) {
		return d.commitSpawn(&value, private, fresh, resume, connection, child, opened)
	}
	_, code, data := d.launch(value.Product, &open, commit)
	if code != 0 {
		d.fail(connection, request, code, data)
		return
	}
	if caller.ctx.Err() == nil {
		d.result(connection, request, protocol.LaneSpawnResult{SessionID: value.SessionID})
	}
}

func (d *Daemon) commitSpawn(value *row, private string, fresh bool, resume string, connection *live, child *process, opened protocol.OpenResult) (int, any) {
	if !validPart(opened.SessionID) || resume != "" && opened.SessionID != resume {
		return protocol.SpawnFailed, failure(nil, "worker returned an invalid session id")
	}
	value.SessionID = qualify(opened.SessionID, d.host)
	if existing, exists := d.table.get(value.SessionID); exists && (fresh || existing.Name != value.Name) {
		return protocol.SpawnFailed, failure(nil, "session id already exists")
	}
	if fresh {
		value.CreatedAt = time.Now().UTC()
		if err := d.table.insert(*value); err != nil {
			if errors.Is(err, errDuplicateRow) {
				return protocol.SpawnFailed, failure(nil, "session id already exists")
			}
			return protocol.Internal, nil
		}
	}
	d.mapsMutex.Lock()
	connection.committed, connection.sessionID, connection.name = true, value.SessionID, value.Name
	connection.groups, connection.privateGroup, connection.child = append([]string(nil), value.Groups...), private, child
	d.connections[value.SessionID] = connection
	d.mapsMutex.Unlock()
	return 0, nil
}

func (d *Daemon) spawnRow(caller source, input *protocol.LaneSpawnRequest) (row, bool, int) {
	if input.ResumeSessionID != "" {
		id, code := d.canonicalInput(input.ResumeSessionID)
		if code != 0 {
			return row{}, false, code
		}
		return row{SessionID: id}, false, 0
	}
	if code := d.localProduct(input.Product, input.Host); code != 0 {
		return row{}, true, code
	}
	if !validPart(input.Name) {
		return row{}, true, protocol.InvalidFrame
	}
	part := unqualify(caller.name) + "/" + input.Name
	if !validPart(part) {
		return row{}, true, protocol.InvalidFrame
	}
	private := caller.private + "/" + input.Name
	groups := orderedGroups(caller.private, private, input.ExtraGroups)
	return row{Product: input.Product, Name: qualify(part, d.host), Groups: groups, Open: *input.Open}, true, 0
}

func (d *Daemon) launch(product string, open *protocol.OpenRequest, commit func(*live, *process, protocol.OpenResult) (int, any)) (launchResult, int, any) {
	path, err := exec.LookPath(product)
	if err != nil {
		return launchResult{}, protocol.UnknownProduct, nil
	}
	token := randomID("token")
	hellos := make(chan launchResult, 1)
	reservation := &reservation{product: product, hello: hellos}
	d.mapsMutex.Lock()
	d.reservations[token] = reservation
	d.mapsMutex.Unlock()
	defer func() {
		d.mapsMutex.Lock()
		delete(d.reservations, token)
		d.mapsMutex.Unlock()
	}()
	environment := replaceEnvironment(os.Environ(), map[string]string{"AGENTBUS_LAUNCH_TOKEN": token, "AGENTBUS_SOCKET": d.config.SocketPath})
	child, err := startProcess(path, environment, io.Discard)
	if err != nil {
		return launchResult{}, protocol.SpawnFailed, failure(nil, err.Error())
	}
	timer := time.NewTimer(spawnTransactionTimeout)
	defer timer.Stop()
	var hello launchResult
	select {
	case hello = <-hellos:
	case <-child.done:
		return launchResult{}, protocol.SpawnFailed, failure(child, "worker exited before hello")
	case <-timer.C:
		child.killAndWait()
		return launchResult{}, protocol.Timeout, nil
	}
	result := launchResult{connection: hello.connection, hello: hello.hello, process: child}
	if open == nil {
		return result, 0, nil
	}
	if unsupported(open.Open, result.hello.SupportedOpenFields) != "" {
		d.stopProbe(result)
		return launchResult{}, protocol.UnsupportedOpen, nil
	}
	opened := make(chan launchResult, 1)
	go func() {
		var value protocol.OpenResult
		code, data := 0, any(nil)
		seen := func() error {
			code, data = commit(result.connection, child, value)
			if code != 0 {
				return errors.New("open commit failed")
			}
			return nil
		}
		err := result.connection.wire.CallObserved(context.Background(), "session.open", open, &value, seen)
		opened <- launchResult{err: err, code: code, data: data}
	}()
	var event launchResult
	select {
	case event = <-opened:
	case <-child.done:
		event = <-opened
	case <-timer.C:
		d.stopProbe(result)
		return launchResult{}, protocol.Timeout, nil
	}
	if event.err == nil {
		return result, 0, nil
	}
	d.stopProbe(result)
	if event.code != 0 {
		return launchResult{}, event.code, event.data
	}
	if code, data, ok := relayData(event.err); ok {
		return launchResult{}, code, data
	}
	return launchResult{}, protocol.SpawnFailed, failure(child, event.err.Error())
}

func (d *Daemon) stopProbe(value launchResult) {
	if value.connection != nil {
		_ = value.connection.wire.Close()
	}
	value.process.killAndWait()
}

func (d *Daemon) localProduct(product, host string) int {
	if host != "" && host != d.host {
		return protocol.UnknownHost
	}
	if !validHost(product) {
		return protocol.UnknownProduct
	}
	return 0
}

func unsupported(open protocol.OpenOptions, supported []string) string {
	fields := []string{"cwd", "permission_mode", "model", "reasoning_effort", "arguments"}
	present := []bool{open.Cwd != "", open.PermissionMode != "", open.Model != "", open.ReasoningEffort != "", open.Arguments != nil}
	for index, field := range fields {
		if present[index] && !slices.Contains(supported, field) {
			return field
		}
	}
	return ""
}

func failure(child *process, message string) protocol.SpawnFailedData {
	lines := []string{message}
	if child != nil {
		lines = append(child.stderr.lines(), message)
	}
	data := protocol.SpawnFailedData{StderrTail: lines}
	if child != nil && child.exit >= 0 {
		exit := child.exit
		data.ExitCode = &exit
	}
	return data
}

func unqualify(value string) string { return value[:strings.LastIndexByte(value, '@')] }
func lanePrivate(value row) string  { return value.Groups[1] }
func orderedGroups(parent, own string, extras []string) []string {
	return append([]string{parent, own}, slices.DeleteFunc(append([]string(nil), extras...), func(group string) bool { return group == parent || group == own })...)
}
