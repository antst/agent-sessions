package federator

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

const laneWatchKillGrace = 5 * time.Second

// RunLaneWatch supervises one native lane CLI from the internal lane-watch
// command. The liveness descriptor is owned by the parent agent; EOF proves
// that even an abruptly killed agent can no longer supervise the child.
func RunLaneWatch(ctx context.Context, livenessFD uintptr, args []string) (int, error) {
	if livenessFD < 3 || len(args) == 0 {
		return 1, errors.New("lane-watch requires a liveness fd and command")
	}
	liveness := os.NewFile(livenessFD, "agent-sessions-federation-agent-liveness")
	if liveness == nil {
		return 1, errors.New("lane-watch liveness fd is unavailable")
	}
	defer func() { _ = liveness.Close() }()
	return runLaneWatch(ctx, liveness, args, laneWatchKillGrace)
}

func runLaneWatch(ctx context.Context, liveness *os.File, args []string, killGrace time.Duration) (int, error) {
	// #nosec G204 G702 -- argv is the validated agent-configured launcher vector.
	command := exec.Command(args[0], args[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return 1, err
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	ownerGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, liveness)
		close(ownerGone)
	}()
	select {
	case err := <-waited:
		return processExitCode(err), nil
	case <-ctx.Done():
	case <-ownerGone:
	}
	_ = command.Process.Signal(os.Interrupt)
	timer := time.NewTimer(killGrace)
	defer timer.Stop()
	select {
	case err := <-waited:
		return processExitCode(err), nil
	case <-timer.C:
		_ = command.Process.Kill()
		err := <-waited
		return processExitCode(err), nil
	}
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}
