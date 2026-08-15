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
	mode := "default"
	dangerousSkip := false
	for index, argument := range args {
		if argument == "--" {
			break
		}
		if selected, recognized := dangerousSkipPermission(argument); recognized {
			dangerousSkip = selected
			continue
		}
		if selected, recognized := processArgPermissionMode(args, index); recognized {
			mode = selected
		}
	}
	if dangerousSkip {
		return "bypassPermissions"
	}
	return mode
}

func dangerousSkipPermission(argument string) (bool, bool) {
	switch argument {
	case "--dangerously-skip-permissions", "--dangerously-skip-permissions=true":
		return true, true
	case "--dangerously-skip-permissions=false":
		return false, true
	default:
		return false, false
	}
}

func processArgPermissionMode(args []string, index int) (string, bool) {
	argument := args[index]
	switch argument {
	case "--always-approve",
		"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--ask-for-approval=never",
		"-a=never", "-anever":
		return "bypassPermissions", true
	case "--permission-mode":
		if index+1 < len(args) {
			return normalizePermissionMode(args[index+1]), true
		}
	case "-a", "--ask-for-approval":
		if index+1 < len(args) {
			return map[bool]string{true: "bypassPermissions", false: "default"}[args[index+1] == "never"], true
		}
	case "--permission-mode=default", "--permission-mode=acceptEdits", "--permission-mode=auto",
		"--permission-mode=always-approve",
		"--permission-mode=dontAsk", "--permission-mode=plan":
		return map[bool]string{true: "bypassPermissions", false: "default"}[argument == "--permission-mode=always-approve"], true
	case "--permission-mode=bypassPermissions":
		return "bypassPermissions", true
	case "--ask-for-approval=untrusted", "--ask-for-approval=on-failure", "--ask-for-approval=on-request":
		return "default", true
	}
	return "", false
}

func normalizePermissionMode(mode string) string {
	if mode == "bypassPermissions" || mode == "always-approve" {
		return "bypassPermissions"
	}
	return "default"
}
