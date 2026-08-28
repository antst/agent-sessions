package launcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/daemon"
)

type claudePeerPlan struct {
	sessionID     string
	attachmentID  string
	resumeTarget  string
	resume        bool
	peerName      string
	context       peerLaunchContext
	args          []string
	informational bool
	alwaysApprove bool
	yoloSpecified bool
}

// RunClaudePeer launches one native Claude session through the per-user host
// daemon. Bare `claude` remains the Agent Sessions opt-out and host settings
// are never modified.
func RunClaudePeer(args []string) error {
	return runClaudePeerWithDaemon(args, productionDaemonPeerDependencies())
}

func runClaudePeerWithDaemon(args []string, dependencies daemonPeerDependencies) error {
	plan, err := parseClaudePeerArgs(args)
	if err != nil {
		return err
	}
	if plan.informational {
		claude, executableErr := claudeExecutable()
		if executableErr != nil {
			return executableErr
		}
		return dependencies.exec(claude, plan.args, nil)
	}
	if dependencies.prepare == nil {
		return errors.New("claude peer daemon client is unavailable")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve Claude working directory: %w", err)
	}
	source, err := claudeprofile.CurrentSource()
	if err != nil {
		return err
	}
	permissionMode := "default"
	if plan.alwaysApprove {
		permissionMode = "bypassPermissions"
	}
	selector := plan.sessionID
	mode := "fresh"
	if plan.resume {
		mode, selector = "resume", plan.resumeTarget
	}
	prepared, err := dependencies.prepare(context.Background(), daemon.AttachmentPrepareRequest{
		Product: "claude", Kind: "interactive",
		ProfileIdentity: map[string]any{
			"profile": source.ConfigRoot, "config_env_set": source.ConfigEnvSet,
			"config_env_value": source.ConfigEnvValue, "secure_env_set": source.SecureEnvSet,
			"secure_config": source.SecureConfig,
		},
		Cwd: cwd, Name: plan.peerName, NameSource: "launch",
		Groups: append([]string(nil), plan.context.groups...), PermissionMode: permissionMode,
		Intent: daemon.InteractiveLaunchIntent{
			Mode: mode, Selector: selector, SelectorIsName: selector != "" && !threadIDPattern.MatchString(selector),
			NativeArguments: append([]string(nil), plan.args...), PermissionExplicit: plan.yoloSpecified,
		},
	})
	if err != nil {
		return fmt.Errorf("prepare Claude attachment: %w", err)
	}
	return executeDaemonPreparedPeer(context.Background(), "claude", prepared, dependencies)
}

//nolint:gocyclo // CLI parsing preserves native Claude flags while extracting the shared peer layer.
func parseClaudePeerArgs(args []string) (claudePeerPlan, error) {
	contextArgs, context, err := extractPeerLaunchContext(args, claudeOptionConsumesNext)
	if err != nil {
		return claudePeerPlan{}, err
	}
	for index, argument := range contextArgs {
		if argument == "--" {
			break
		}
		if argument == "--yolo" {
			contextArgs[index] = "--dangerously-skip-permissions"
		}
	}
	forwarded, peerName, err := extractPeerNameArgs(contextArgs)
	if err != nil {
		return claudePeerPlan{}, err
	}
	plan := claudePeerPlan{peerName: peerName, context: context, args: forwarded}
	for _, argument := range beforeDoubleDash(forwarded) {
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			plan.informational = true
			return plan, nil
		}
	}
	scanned := beforeDoubleDash(forwarded)
	for index := 0; index < len(scanned); index++ {
		argument := scanned[index]
		switch {
		case argument == "--bare":
			return claudePeerPlan{}, usageError("claude-peer requires native messaging; use bare claude to opt out")
		case argument == "--allow-dangerously-skip-permissions" || strings.HasPrefix(argument, "--allow-dangerously-skip-permissions="):
			return claudePeerPlan{}, usageError("claude-peer cannot attest mutable in-session bypass; launch with --dangerously-skip-permissions instead")
		case argument == "--session-id":
			if index+1 >= len(scanned) {
				return claudePeerPlan{}, usageError("--session-id requires a value")
			}
			plan.sessionID = scanned[index+1]
			index++
		case strings.HasPrefix(argument, "--session-id="):
			plan.sessionID = strings.TrimPrefix(argument, "--session-id=")
		case argument == "--resume" || argument == "-r":
			if index+1 >= len(scanned) {
				return claudePeerPlan{}, usageError(argument + " requires a native Claude resume target")
			}
			plan.resumeTarget, plan.resume = scanned[index+1], true
			index++
		case strings.HasPrefix(argument, "--resume="):
			plan.resumeTarget, plan.resume = strings.TrimPrefix(argument, "--resume="), true
		case argument == "--dangerously-skip-permissions":
			plan.alwaysApprove, plan.yoloSpecified = true, true
		case argument == "--permission-mode" && index+1 < len(scanned):
			plan.alwaysApprove = scanned[index+1] == "bypassPermissions"
			plan.yoloSpecified = true
			index++
		case strings.HasPrefix(argument, "--permission-mode="):
			plan.alwaysApprove = strings.TrimPrefix(argument, "--permission-mode=") == "bypassPermissions"
			plan.yoloSpecified = true
		}
	}
	if context.forceNoYolo {
		if plan.alwaysApprove {
			return claudePeerPlan{}, usageError("--no-yolo conflicts with Claude bypass permissions")
		}
		plan.yoloSpecified = true
	}
	switch {
	case plan.resume:
		if strings.TrimSpace(plan.resumeTarget) == "" {
			return claudePeerPlan{}, usageError("--resume requires a native Claude resume target")
		}
		if threadIDPattern.MatchString(plan.resumeTarget) {
			plan.sessionID = plan.resumeTarget
			plan.attachmentID = plan.sessionID
		} else {
			plan.sessionID = ""
			plan.attachmentID, err = newClaudePeerSessionID()
			if err != nil {
				return claudePeerPlan{}, err
			}
		}
	case plan.sessionID == "":
		plan.sessionID, err = newClaudePeerSessionID()
		if err != nil {
			return claudePeerPlan{}, err
		}
		plan.attachmentID = plan.sessionID
		plan.args = insertClaudeManagedArgs(plan.args, "--session-id", plan.sessionID)
	case !threadIDPattern.MatchString(plan.sessionID):
		return claudePeerPlan{}, usageError("--session-id requires an exact session UUID")
	default:
		plan.attachmentID = plan.sessionID
	}
	if peerName != "" {
		plan.args = insertClaudeManagedArgs(plan.args, "--name", peerName)
	}
	if !claudePeerHasChromeMode(plan.args) {
		plan.args = insertClaudeManagedArgs(plan.args, "--no-chrome")
	}
	return plan, nil
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

func claudeOptionConsumesNext(option string) bool {
	name := strings.SplitN(option, "=", 2)[0]
	switch name {
	case "--add-dir", "--agent", "--agents", "--allowedTools", "--append-system-prompt", "--betas",
		"--debug-file", "--disallowedTools", "--effort", "--fallback-model", "--ide", "--input-format",
		"--json-schema", "--max-budget-usd", "--max-turns", "--mcp-config", "--model", "--name",
		"--output-format", "--permission-mode", "--permission-prompt-tool", "--plugin-dir", "--resume", "-r",
		"--session-id", "--settings", "--system-prompt", "--tools":
		return !strings.Contains(option, "=")
	default:
		return false
	}
}

func claudePeerHasChromeMode(args []string) bool {
	for _, argument := range beforeDoubleDash(args) {
		if argument == "--chrome" || argument == "--no-chrome" || strings.HasPrefix(argument, "--chrome=") || strings.HasPrefix(argument, "--no-chrome=") {
			return true
		}
	}
	return false
}

func claudeExecutable() (string, error) {
	return productExecutable("CLAUDE_PEER_CLAUDE_BIN", "claude")
}

func newClaudePeerSessionID() (string, error) {
	body := make([]byte, 16)
	if _, err := rand.Read(body); err != nil {
		return "", fmt.Errorf("generate Claude session ID: %w", err)
	}
	body[6] = body[6]&0x0f | 0x40
	body[8] = body[8]&0x3f | 0x80
	hexID := hex.EncodeToString(body)
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:], nil
}
