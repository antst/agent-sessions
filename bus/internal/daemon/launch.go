package daemon

import (
	"os"
	"os/exec"
	"time"

	"github.com/antst/agent-sessions/bus/internal/protocol"
	"github.com/antst/agent-sessions/bus/internal/structuredprocess"
)

const spawnTransactionTimeout = 60 * time.Second

type launch struct {
	token    string
	product  string
	entry    *entry
	owner    *session
	claimed  chan struct{}
	timer    *time.Timer
	reply    chan answer
	describe bool
	fresh    bool
}

type processEvent struct {
	launch *launch
	child  *structuredprocess.Process
	exited bool
	err    error
}

type spawnTimeout struct{ launch *launch }

func newLaunch(product string, describe, fresh bool) *launch {
	return &launch{token: randomID("token"), product: product, claimed: make(chan struct{}, 1), timer: time.NewTimer(spawnTransactionTimeout),
		reply: make(chan answer, 1), describe: describe, fresh: fresh}
}

func (d *Daemon) startProduct(start *launch) {
	go func() {
		defer d.group.Done()
		path, err := exec.LookPath(start.product)
		if err != nil {
			d.launchFailed(start, answer{code: protocol.UnknownProduct}, processEvent{launch: start, err: err, exited: true})
			return
		}
		environment := structuredprocess.Environment(os.Environ(), map[string]string{"AGENTBUS_LAUNCH_TOKEN": start.token, "AGENTBUS_SOCKET": d.config.SocketPath})
		child, err := structuredprocess.Start(path, environment)
		if err != nil {
			d.launchFailed(start, answer{code: protocol.SpawnFailed, data: failure(nil, err.Error())}, processEvent{launch: start, err: err, exited: true})
			return
		}
		started := processEvent{launch: start, child: child}
		select {
		case <-start.claimed:
			start.owner.inbox <- started
			d.followProcess(start, child)
		case <-child.Done():
			if d.directory.releaseLaunch(start, true) {
				start.reply <- answer{code: protocol.SpawnFailed, data: failure(child, "worker exited before hello")}
			} else {
				start.owner.inbox <- started
				start.owner.inbox <- processEvent{launch: start, child: child, exited: true}
			}
		case <-start.timer.C:
			if d.directory.releaseLaunch(start, true) {
				child.Stop(0)
				start.reply <- answer{code: protocol.Timeout}
			} else {
				start.owner.inbox <- started
				start.owner.inbox <- spawnTimeout{launch: start}
				d.waitProcess(start, child)
			}
		case <-d.shutdown:
			if d.directory.releaseLaunch(start, true) {
				child.Stop(0)
				start.reply <- answer{code: protocol.Internal}
			} else {
				start.owner.inbox <- started
				d.waitProcess(start, child)
			}
		}
	}()
}

func (d *Daemon) followProcess(start *launch, child *structuredprocess.Process) {
	select {
	case <-child.Done():
		start.owner.inbox <- processEvent{launch: start, child: child, exited: true}
	case <-start.timer.C:
		start.owner.inbox <- spawnTimeout{launch: start}
		d.waitProcess(start, child)
	case <-d.shutdown:
		d.waitProcess(start, child)
	}
}

func (d *Daemon) waitProcess(start *launch, child *structuredprocess.Process) {
	<-child.Done()
	start.owner.inbox <- processEvent{launch: start, child: child, exited: true}
}

func (d *Daemon) launchFailed(start *launch, unclaimed answer, claimed processEvent) {
	if d.directory.releaseLaunch(start, true) {
		start.reply <- unclaimed
	} else {
		start.owner.inbox <- claimed
	}
}

func failure(child *structuredprocess.Process, message string) protocol.SpawnFailedData {
	data := protocol.SpawnFailedData{StderrTail: []string{message}}
	if child == nil {
		return data
	}
	stderr, exit := child.Details()
	data.StderrTail = append(stderr, message)
	if exit >= 0 {
		data.ExitCode = &exit
	}
	return data
}
