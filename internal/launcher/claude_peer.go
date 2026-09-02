package launcher

import "os"

type claudePeerPlan struct {
	peerName string
	context  peerLaunchContext
	args     []string
}

// RunClaudePeer is a stateless argv adapter. Claude owns session creation,
// naming, lookup, resume, history, and cleanup; Agent Sessions removes only
// its product-neutral wrapper flags and then replaces itself with Claude.
func RunClaudePeer(args []string) error {
	plan, err := parseClaudePeerArgs(args)
	if err != nil {
		return err
	}
	claude, err := claudeExecutable()
	if err != nil {
		return err
	}
	environment := liveReportEnvironment(os.Environ(), claudeReportName(plan), plan.context.groups)
	return Exec(claude, plan.args, environment)
}

func claudeReportName(plan claudePeerPlan) string {
	if plan.peerName != "" {
		return plan.peerName
	}
	for index, argument := range plan.args {
		if argument == "--resume" && index+1 < len(plan.args) {
			return plan.args[index+1]
		}
		if len(argument) > len("--resume=") && argument[:len("--resume=")] == "--resume=" {
			return argument[len("--resume="):]
		}
	}
	return ""
}

// parseClaudePeerArgs preserves native Claude argv byte-for-byte except for
// Agent Sessions wrapper flags. Native --name, --resume, and --session-id are
// deliberately left for Claude to resolve and validate.
func parseClaudePeerArgs(args []string) (claudePeerPlan, error) {
	forwarded, context, err := scanPeerWrapperOptions("claude", args)
	if err != nil {
		return claudePeerPlan{}, err
	}
	for index, argument := range forwarded {
		if argument == "--" {
			break
		}
		if argument == "--yolo" {
			forwarded[index] = "--dangerously-skip-permissions"
		}
	}
	forwarded, peerName, err := extractPeerNameArgs(forwarded)
	if err != nil {
		return claudePeerPlan{}, err
	}
	if peerName != "" {
		forwarded = insertClaudeManagedArgs(forwarded, "--name", peerName)
	}
	if context.forceNoYolo {
		forwarded = insertClaudeManagedArgs(forwarded, "--permission-mode", "default")
	}
	return claudePeerPlan{peerName: peerName, context: context, args: forwarded}, nil
}

func insertClaudeManagedArgs(args []string, managed ...string) []string {
	insert := len(args)
	for index, argument := range args {
		if argument == "--" {
			insert = index
			break
		}
	}
	result := make([]string, 0, len(args)+len(managed))
	result = append(result, args[:insert]...)
	result = append(result, managed...)
	result = append(result, args[insert:]...)
	return result
}

func claudeExecutable() (string, error) {
	return productExecutable("CLAUDE_PEER_CLAUDE_BIN", "claude")
}
