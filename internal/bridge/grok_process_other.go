//go:build !linux && !darwin

package bridge

import (
	"os"
	"os/exec"
	"time"
)

func configureGrokChildProcess(_ *exec.Cmd) {}

func stopGrokManagedProcess(process *grokManagedProcess, timeout time.Duration) {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return
	}
	select {
	case <-process.done:
		return
	default:
	}
	pid := process.cmd.Process.Pid
	if exactProcessIdentityStatus(pid, process.procStart).Status != processIdentityMatches {
		return
	}
	_ = process.cmd.Process.Kill()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
	case <-timer.C:
	}
}

func stopStaleGrokProcess(pid int, procStart string) {
	if pid <= 1 || exactProcessIdentityStatus(pid, procStart).Status != processIdentityMatches {
		return
	}
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Kill()
	}
}
