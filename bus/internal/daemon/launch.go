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
			if !d.finishUnclaimed(start, nil, answer{code: protocol.UnknownProduct}) {
				start.owner.inbox <- processEvent{launch: start, err: err, exited: true}
			}
			return
		}
		environment := structuredprocess.Environment(os.Environ(), map[string]string{"AGENTBUS_LAUNCH_TOKEN": start.token, "AGENTBUS_SOCKET": d.config.SocketPath})
		child, err := structuredprocess.Start(path, environment)
		if err != nil {
			if !d.finishUnclaimed(start, nil, answer{code: protocol.SpawnFailed, data: failure(nil, err.Error())}) {
				start.owner.inbox <- processEvent{launch: start, err: err, exited: true}
			}
			return
		}
		select {
		case <-start.claimed:
			d.finishClaimed(start, child, false)
		case <-child.Done():
			if !d.finishUnclaimed(start, child, answer{code: protocol.SpawnFailed, data: failure(child, "worker exited before hello")}) {
				d.finishClaimed(start, child, false)
			}
		case <-start.timer.C:
			if !d.finishUnclaimed(start, child, answer{code: protocol.Timeout}) {
				d.finishClaimed(start, child, true)
			}
		case <-d.shutdown:
			if !d.finishUnclaimed(start, child, answer{code: protocol.Internal}) {
				d.finishClaimed(start, child, false)
			}
		}
	}()
}

func (d *Daemon) finishClaimed(start *launch, child *structuredprocess.Process, timedOut bool) {
	start.owner.inbox <- processEvent{launch: start, child: child}
	if timedOut {
		start.owner.inbox <- spawnTimeout{launch: start}
	}
	<-child.Done()
	child.Stop()
	start.owner.inbox <- processEvent{launch: start, child: child, exited: true}
}

func (d *Daemon) finishUnclaimed(start *launch, child *structuredprocess.Process, result answer) bool {
	if !d.directory.revokeLaunch(start) {
		return false
	}
	if child != nil {
		child.Stop()
	}
	d.directory.releaseLaunch(start)
	start.reply <- result
	return true
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
