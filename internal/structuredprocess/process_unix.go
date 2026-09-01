//go:build linux || darwin

package structuredprocess

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func configureOwnedProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func sendProcessGroupSignal(processGroup int, signal productruntime.ProcessSignal) error {
	native, err := nativeSignal(signal)
	if err != nil {
		return err
	}
	if err := syscall.Kill(-processGroup, native); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func nativeSignal(signal productruntime.ProcessSignal) (syscall.Signal, error) {
	switch signal {
	case productruntime.ProcessInterrupt:
		return syscall.SIGINT, nil
	case productruntime.ProcessTerminate:
		return syscall.SIGTERM, nil
	case productruntime.ProcessKill:
		return syscall.SIGKILL, nil
	default:
		return 0, errors.New("structured process signal is unsupported")
	}
}

func classifyProcessExit(state *os.ProcessState, waitErr error) (productruntime.ProcessExit, error) {
	if state == nil {
		return productruntime.ProcessExit{}, waitErr
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return productruntime.ProcessExit{}, errors.New("structured process exit status is unavailable")
	}
	if status.Signaled() {
		signal := status.Signal()
		return productruntime.ProcessExit{ExitLike: 128 + int(signal), Signal: stableSignalName(signal)}, nil
	}
	if waitErr == nil || isExitError(waitErr) {
		return productruntime.ProcessExit{ExitLike: status.ExitStatus()}, nil
	}
	return productruntime.ProcessExit{}, waitErr
}

func isExitError(err error) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError)
}

func stableSignalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGINT:
		return string(productruntime.ProcessInterrupt)
	case syscall.SIGTERM:
		return string(productruntime.ProcessTerminate)
	case syscall.SIGKILL:
		return string(productruntime.ProcessKill)
	default:
		return signal.String()
	}
}
