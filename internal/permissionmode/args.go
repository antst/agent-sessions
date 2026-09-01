// Package permissionmode classifies native product argv into the two durable
// Agent Sessions permission classes. It owns precedence and argument-boundary
// semantics for every caller that corroborates a live process.
package permissionmode

import "fmt"

// Mode is the durable Agent Sessions permission class. Product adapters map
// this shared class to an equal-or-narrower native policy and reject products
// that cannot represent it without widening authority.
type Mode string

const (
	// Default preserves the native product's ordinary approval policy.
	Default Mode = "default"
	// BypassPermissions is the explicit no-approval policy.
	BypassPermissions Mode = "bypassPermissions"
)

// Parse validates one exact durable permission mode.
func Parse(value string) (Mode, error) {
	mode := Mode(value)
	if !mode.Valid() {
		return "", fmt.Errorf("unsupported permission mode %q", value)
	}
	return mode, nil
}

// Valid reports whether the mode is one of the shared durable policies.
func (m Mode) Valid() bool {
	return m == Default || m == BypassPermissions
}

// FromArgs returns default or bypassPermissions for one native argv vector.
// Options after -- are prompt text, not process policy. Native dangerous-skip
// flags have independent last-value semantics and override ordinary modes when
// enabled; all other policy selectors follow their last occurrence.
func FromArgs(args []string) string {
	mode := string(Default)
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
		return string(BypassPermissions)
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
			return normalize(args[index+1]), true
		}
	case "-a", "--ask-for-approval":
		if index+1 < len(args) {
			if args[index+1] == "never" {
				return "bypassPermissions", true
			}
			return "default", true
		}
	case "--permission-mode=default", "--permission-mode=acceptEdits", "--permission-mode=auto",
		"--permission-mode=always-approve", "--permission-mode=dontAsk", "--permission-mode=plan":
		if argument == "--permission-mode=always-approve" {
			return "bypassPermissions", true
		}
		return "default", true
	case "--permission-mode=bypassPermissions":
		return "bypassPermissions", true
	case "--ask-for-approval=untrusted", "--ask-for-approval=on-failure", "--ask-for-approval=on-request":
		return "default", true
	}
	return "", false
}

func normalize(mode string) string {
	if mode == "bypassPermissions" || mode == "always-approve" {
		return "bypassPermissions"
	}
	return "default"
}
