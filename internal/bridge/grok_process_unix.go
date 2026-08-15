//go:build linux || darwin

package bridge

import (
	"os/exec"
	"syscall"
	"time"
)

func configureGrokChildProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

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
	// Never signal a recycled PID. Setsid makes pid the private process-group
	// id, so the signal also reaches Grok descendants without touching a caller.
	if exactProcessIdentityStatus(pid, process.procStart).Status != processIdentityMatches {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
		return
	case <-timer.C:
	}
	if exactProcessIdentityStatus(pid, process.procStart).Status == processIdentityMatches {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	<-process.done
}

func stopStaleGrokProcess(pid int, procStart string) {
	if pid <= 1 || exactProcessIdentityStatus(pid, procStart).Status != processIdentityMatches {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exactProcessIdentityStatus(pid, procStart).Status == processIdentityStale {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if exactProcessIdentityStatus(pid, procStart).Status == processIdentityMatches {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
