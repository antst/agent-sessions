package daemon

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/rpc"
	"github.com/antst/agent-sessions/bus/internal/structuredprocess"
)

const spawnTransactionTimeout = 60 * time.Second

type workerStart struct {
	connection *live
	hello      protocol.HelloDescription
	process    *structuredprocess.Process
}

type launchOutcome struct {
	code int
	data any
}

type launchDecision struct {
	sync.Mutex
	done    bool
	outcome launchOutcome
}

func (d *launchDecision) claim(makeOutcome func() launchOutcome) (launchOutcome, bool) {
	d.Lock()
	defer d.Unlock()
	if d.done {
		return d.outcome, false
	}
	d.done, d.outcome = true, makeOutcome()
	return d.outcome, true
}

func (d *Daemon) beginLaunch() bool {
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	if d.closing {
		return false
	}
	d.spawnWG.Add(1)
	return true
}

func (d *Daemon) describe(connection *live, request *rpc.Request, caller source) {
	if !d.beginLaunch() {
		return
	}
	defer d.spawnWG.Done()
	input := request.Params.(*protocol.LaneDescribeRequest)
	if code := d.localProduct(input.Product, input.Host); code != 0 {
		d.fail(connection, request, code, nil)
		return
	}
	timer := time.NewTimer(spawnTransactionTimeout)
	defer timer.Stop()
	worker, outcome := d.startWorker(input.Product, timer.C)
	if outcome.code != 0 {
		d.fail(connection, request, outcome.code, outcome.data)
		return
	}
	d.stopWorker(worker)
	if caller.ctx.Err() == nil {
		d.result(connection, request, worker.hello)
	}
}

func (d *Daemon) spawn(connection *live, request *rpc.Request, caller source) {
	if !d.beginLaunch() {
		return
	}
	defer d.spawnWG.Done()
	value, fresh, code := d.spawnRow(caller, request.Params.(*protocol.LaneSpawnRequest))
	if code != 0 {
		d.fail(connection, request, code, nil)
		return
	}
	var rowLock *sync.Mutex
	if fresh {
		if !d.reserveName(value.Name) {
			d.fail(connection, request, protocol.NameTaken, nil)
			return
		}
		defer d.releaseName(value.Name)
	} else {
		rowLock = d.rowLock(value.SessionID)
		rowLock.Lock()
		current, exists := d.table.get(value.SessionID)
		d.mapsMutex.Lock()
		connected := d.connections[value.SessionID] != nil
		d.mapsMutex.Unlock()
		if !exists || !shares(caller.groups, current.Groups) {
			rowLock.Unlock()
			d.fail(connection, request, protocol.UnknownSession, nil)
			return
		}
		if connected {
			rowLock.Unlock()
			d.fail(connection, request, protocol.AlreadyConnected, nil)
			return
		}
		value = current
	}
	private, resume := lanePrivate(value), ""
	if !fresh {
		resume = unqualify(value.SessionID)
	}
	open := protocol.OpenRequest{Name: value.Name, Groups: value.Groups, ResumeSessionID: resume, Open: value.Open}
	timer := time.NewTimer(spawnTransactionTimeout)
	defer timer.Stop()
	worker, outcome := d.startWorker(value.Product, timer.C)
	if outcome.code != 0 {
		if rowLock != nil {
			rowLock.Unlock()
		}
		d.fail(connection, request, outcome.code, outcome.data)
		return
	}
	if unsupported(open.Open, worker.hello.SupportedOpenFields) != "" {
		if rowLock != nil {
			rowLock.Unlock()
		}
		d.stopWorker(worker)
		d.fail(connection, request, protocol.UnsupportedOpen, nil)
		return
	}
	if rowLock != nil {
		d.mapsMutex.Lock()
		if d.closing || d.connections[value.SessionID] != nil {
			d.mapsMutex.Unlock()
			rowLock.Unlock()
			d.stopWorker(worker)
			d.fail(connection, request, protocol.AlreadyConnected, nil)
			return
		}
		worker.connection.sessionID, worker.connection.name = value.SessionID, value.Name
		worker.connection.groups, worker.connection.privateGroup = append([]string(nil), value.Groups...), private
		d.connections[value.SessionID] = worker.connection
		d.mapsMutex.Unlock()
		rowLock.Unlock()
	}
	decision := &launchDecision{}
	var opened protocol.OpenResult
	seen := func() error {
		result, won := decision.claim(func() launchOutcome {
			lock := rowLock
			if fresh && validPart(opened.SessionID) {
				lock = d.rowLock(qualify(opened.SessionID, d.host))
			}
			if lock == nil {
				return launchOutcome{code: protocol.SpawnFailed, data: failure(nil, "worker returned an invalid session id")}
			}
			lock.Lock()
			defer lock.Unlock()
			code, data := d.commitSpawn(&value, private, fresh, resume, worker, opened)
			return launchOutcome{code: code, data: data}
		})
		if !won || result.code != 0 {
			return errors.New("open commit failed")
		}
		return nil
	}
	call, queued := worker.connection.enqueue("session.open", open, &opened, seen)
	if !queued {
		outcome = launchOutcome{code: protocol.SpawnFailed, data: failure(worker.process, "worker closed before open")}
	} else {
		responded := make(chan error, 1)
		go func() { responded <- call.wait(worker.connection) }()
		candidate := launchOutcome{}
		select {
		case err := <-responded:
			if code, data, ok := relayData(err); ok {
				candidate = launchOutcome{code: code, data: data}
			} else if err != nil {
				candidate = launchOutcome{code: protocol.SpawnFailed, data: failure(worker.process, err.Error())}
			}
		case <-worker.process.Done():
			select {
			case err := <-responded:
				if code, data, ok := relayData(err); ok {
					candidate = launchOutcome{code: code, data: data}
				} else if err != nil {
					candidate = launchOutcome{code: protocol.SpawnFailed, data: failure(worker.process, err.Error())}
				}
			case <-worker.connection.wire.Done():
				candidate = launchOutcome{code: protocol.SpawnFailed, data: failure(worker.process, "worker exited before open")}
			}
		case <-timer.C:
			candidate.code = protocol.Timeout
		case <-d.shutdown:
			candidate.code = protocol.Internal
		}
		outcome, _ = decision.claim(func() launchOutcome { return candidate })
	}
	if outcome.code != 0 {
		if !fresh {
			d.mapsMutex.Lock()
			if d.connections[value.SessionID] == worker.connection {
				delete(d.connections, value.SessionID)
			}
			d.mapsMutex.Unlock()
		}
		d.stopWorker(worker)
		d.fail(connection, request, outcome.code, outcome.data)
		return
	}
	if caller.ctx.Err() == nil {
		d.result(connection, request, protocol.LaneSpawnResult{SessionID: value.SessionID})
	}
}

func (d *Daemon) commitSpawn(value *row, private string, fresh bool, resume string, worker workerStart, opened protocol.OpenResult) (int, any) {
	if !validPart(opened.SessionID) || resume != "" && opened.SessionID != resume {
		return protocol.SpawnFailed, failure(nil, "worker returned an invalid session id")
	}
	value.SessionID = qualify(opened.SessionID, d.host)
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	if d.closing {
		return protocol.Internal, nil
	}
	if claimed := d.connections[value.SessionID]; claimed != nil && claimed != worker.connection {
		return protocol.SpawnFailed, failure(nil, "session id already exists")
	}
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
	worker.connection.committed, worker.connection.sessionID, worker.connection.name = true, value.SessionID, value.Name
	worker.connection.groups, worker.connection.privateGroup = append([]string(nil), value.Groups...), private
	d.connections[value.SessionID] = worker.connection
	return 0, nil
}

func (d *Daemon) spawnRow(caller source, input *protocol.LaneSpawnRequest) (row, bool, int) {
	if input.ResumeSessionID != "" {
		id, code := d.canonicalInput(input.ResumeSessionID)
		return row{SessionID: id}, false, code
	}
	if code := d.localProduct(input.Product, input.Host); code != 0 {
		return row{}, true, code
	}
	part := unqualify(caller.name) + "/" + input.Name
	if !validPart(input.Name) || !validPart(part) {
		return row{}, true, protocol.InvalidFrame
	}
	private := caller.private + "/" + input.Name
	groups := orderedGroups(caller.private, private, input.ExtraGroups)
	return row{Product: input.Product, Name: qualify(part, d.host), Groups: groups, Open: *input.Open}, true, 0
}

func (d *Daemon) startWorker(product string, timeout <-chan time.Time) (workerStart, launchOutcome) {
	path, err := exec.LookPath(product)
	if err != nil {
		return workerStart{}, launchOutcome{code: protocol.UnknownProduct}
	}
	token := randomID("token")
	hellos := make(chan workerStart, 1)
	reservation := &reservation{product: product, hello: hellos}
	d.mapsMutex.Lock()
	if d.closing {
		d.mapsMutex.Unlock()
		return workerStart{}, launchOutcome{code: protocol.Internal}
	}
	d.reservations[token] = reservation
	d.mapsMutex.Unlock()
	defer func() {
		d.mapsMutex.Lock()
		delete(d.reservations, token)
		d.mapsMutex.Unlock()
	}()
	environment := structuredprocess.Environment(os.Environ(), map[string]string{"AGENTBUS_LAUNCH_TOKEN": token, "AGENTBUS_SOCKET": d.config.SocketPath})
	child, err := structuredprocess.Start(path, environment)
	if err != nil {
		return workerStart{}, launchOutcome{code: protocol.SpawnFailed, data: failure(nil, err.Error())}
	}
	d.mapsMutex.Lock()
	reservation.process = child
	closing := d.closing
	d.mapsMutex.Unlock()
	if closing {
		child.KillAndWait()
		return workerStart{}, launchOutcome{code: protocol.Internal}
	}
	select {
	case worker := <-hellos:
		worker.process = child
		d.mapsMutex.Lock()
		worker.connection.child = child
		d.mapsMutex.Unlock()
		return worker, launchOutcome{}
	case <-child.Done():
		return workerStart{}, launchOutcome{code: protocol.SpawnFailed, data: failure(child, "worker exited before hello")}
	case <-timeout:
		child.KillAndWait()
		return workerStart{}, launchOutcome{code: protocol.Timeout}
	case <-d.shutdown:
		child.KillAndWait()
		return workerStart{}, launchOutcome{code: protocol.Internal}
	}
}

func (d *Daemon) stopWorker(worker workerStart) {
	if worker.connection != nil {
		_ = worker.connection.wire.Close()
	}
	worker.process.KillAndWait()
}

func (d *Daemon) reserveName(name string) bool {
	d.mapsMutex.Lock()
	defer d.mapsMutex.Unlock()
	if d.reservedNames[name] || d.closing {
		return false
	}
	if _, exists := d.table.byName(name); exists {
		return false
	}
	d.reservedNames[name] = true
	return true
}

func (d *Daemon) releaseName(name string) {
	d.mapsMutex.Lock()
	delete(d.reservedNames, name)
	d.mapsMutex.Unlock()
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

func failure(child *structuredprocess.Process, message string) protocol.SpawnFailedData {
	lines := []string{message}
	if child != nil {
		stderr, exit := child.Details()
		lines = append(stderr, message)
		data := protocol.SpawnFailedData{StderrTail: lines}
		if exit >= 0 {
			data.ExitCode = &exit
		}
		return data
	}
	return protocol.SpawnFailedData{StderrTail: lines}
}

func unqualify(value string) string { return value[:strings.LastIndexByte(value, '@')] }
func lanePrivate(value row) string  { return value.Groups[1] }
func orderedGroups(parent, own string, extras []string) []string {
	return append([]string{parent, own}, slices.DeleteFunc(append([]string(nil), extras...), func(group string) bool { return group == parent || group == own })...)
}
