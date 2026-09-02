package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

type launcherHeldChild struct {
	role    string
	command *exec.Cmd
	primary bool
	ready   func(context.Context) error
	done    chan struct{}
}

type launcherHeldIdentity struct {
	report liveSessionReport
	call   func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	watch  func(context.Context, *liveSessionClient, liveSessionReport)
}

type launcherHeldExit struct {
	index int
	err   error
}

// runLauncherHeldPeer is the one topology for product sessions whose terminal
// launcher proves liveness. The two product inputs are the children to own and
// the product-native identity confirmation performed after they start.
func runLauncherHeldPeer(
	ctx context.Context,
	children []launcherHeldChild,
	confirm func(context.Context) (launcherHeldIdentity, error),
) error {
	if ctx == nil || confirm == nil || len(children) == 0 {
		return errors.New("launcher-held peer dependencies are incomplete")
	}
	primary := -1
	for index, child := range children {
		if child.command == nil {
			return errors.New("launcher-held peer child is unavailable")
		}
		if child.primary {
			if primary >= 0 {
				return errors.New("launcher-held peer has multiple terminal children")
			}
			primary = index
		}
	}
	if primary < 0 {
		return errors.New("launcher-held peer has no terminal child")
	}
	runCtx, cancel := context.WithCancel(ctx)

	exits := make(chan launcherHeldExit, len(children))
	started := 0
	defer func() {
		cancel()
		stopLauncherHeldChildren(children[:started])
	}()
	for index := range children {
		child := &children[index]
		if err := child.command.Start(); err != nil {
			return fmt.Errorf("start %s: %w", child.role, err)
		}
		started++
		child.done = make(chan struct{})
		go func(index int, child *launcherHeldChild) {
			err := child.command.Wait()
			close(child.done)
			exits <- launcherHeldExit{index: index, err: err}
		}(index, child)
		if child.ready == nil {
			continue
		}
		ready := make(chan error, 1)
		go func(check func(context.Context) error) { ready <- check(runCtx) }(child.ready)
		select {
		case err := <-ready:
			if err != nil {
				return fmt.Errorf("wait for %s: %w", child.role, err)
			}
		case exited := <-exits:
			return launcherHeldChildError(children[exited.index].role, exited.err)
		case <-runCtx.Done():
			return runCtx.Err()
		}
	}

	confirmed := make(chan struct {
		identity launcherHeldIdentity
		err      error
	}, 1)
	go func() {
		identity, err := confirm(runCtx)
		confirmed <- struct {
			identity launcherHeldIdentity
			err      error
		}{identity: identity, err: err}
	}()
	var identity launcherHeldIdentity
	select {
	case result := <-confirmed:
		if result.err != nil {
			return result.err
		}
		identity = result.identity
	case exited := <-exits:
		return launcherHeldChildError(children[exited.index].role, exited.err)
	case <-runCtx.Done():
		return runCtx.Err()
	}
	if !validLiveSessionReport(identity.report) {
		return errors.New("product returned an invalid live session identity")
	}

	client := startLiveSessionClient(runCtx, livePresenceEndpoint(defaultStateRoot()), identity.report, identity.call)
	if identity.watch != nil {
		identity.watch(runCtx, client, identity.report)
	}
	defer func() {
		cancel()
		client.mu.Lock()
		if client.current != nil {
			_ = client.current.connection.Close()
		}
		client.mu.Unlock()
	}()
	for {
		select {
		case exited := <-exits:
			if exited.index == primary {
				return exited.err
			}
			return launcherHeldChildError(children[exited.index].role, exited.err)
		case <-runCtx.Done():
			return runCtx.Err()
		}
	}
}

func launcherHeldChildError(role string, err error) error {
	if err == nil {
		err = errors.New("process exited")
	}
	return fmt.Errorf("%s: %w", role, err)
}

func stopLauncherHeldChildren(children []launcherHeldChild) {
	for index := len(children) - 1; index >= 0; index-- {
		command := children[index].command
		if command == nil || command.Process == nil || launcherHeldChildExited(children[index]) {
			continue
		}
		_ = command.Process.Signal(syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allExited := true
		for _, child := range children {
			if child.command != nil && child.command.Process != nil && !launcherHeldChildExited(child) {
				allExited = false
				break
			}
		}
		if allExited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, child := range children {
		command := child.command
		if command != nil && command.Process != nil && !launcherHeldChildExited(child) {
			_ = command.Process.Kill()
		}
	}
}

func launcherHeldChildExited(child launcherHeldChild) bool {
	if child.done == nil {
		return false
	}
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}
