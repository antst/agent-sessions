package federator

import "github.com/antst/agent-sessions/internal/procinfo"

func peerPermissionMode(pid int, _ string) string {
	args, err := procinfo.Args(pid)
	if err != nil {
		return "default"
	}
	return permissionModeFromProcessArgs(args)
}

func permissionModeFromProcessArgs(args []string) string {
	for index, argument := range args {
		if argument == "--" {
			break
		}
		if processArgEnablesBypass(args, index) {
			return "bypassPermissions"
		}
	}
	return "default"
}

func processArgEnablesBypass(args []string, index int) bool {
	argument := args[index]
	switch argument {
	case "--dangerously-skip-permissions", "--permission-mode=bypassPermissions",
		"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--ask-for-approval=never",
		"-a=never", "-anever":
		return true
	case "--permission-mode":
		return index+1 < len(args) && args[index+1] == "bypassPermissions"
	case "-a", "--ask-for-approval":
		return index+1 < len(args) && args[index+1] == "never"
	default:
		return false
	}
}
