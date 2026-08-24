package federator

import (
	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/procinfo"
)

func peerPermissionMode(pid int, _ string) string {
	args, err := procinfo.Args(pid)
	if err != nil {
		return "default"
	}
	return permissionModeFromProcessArgs(args)
}

func permissionModeFromProcessArgs(args []string) string {
	return permissionmode.FromArgs(args)
}
