//go:build linux || darwin

package launcher

import (
	"os/exec"
	"syscall"
)

func configureGrokHostProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
