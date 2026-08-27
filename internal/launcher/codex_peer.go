package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/pathidentity"
)

var threadIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

type codexMode string

const (
	modeFresh       codexMode = "fresh"
	modeResume      codexMode = "resume"
	modeFork        codexMode = "fork"
	modePassthrough codexMode = "passthrough"
)

type codexPlan struct {
	mode              codexMode
	peerName          string
	peerNameSource    string
	requestedCwd      string
	cwdExplicit       bool
	requestedYolo     bool
	yoloSpecified     bool
	peerContext       peerLaunchContext
	originalArgs      []string
	interactiveArgs   []string
	selectionTarget   string
	informationalPass bool
}

// RunCodexPeer starts or resumes an owner-attested interactive Codex peer.
func RunCodexPeer(args []string) error {
	return runCodexPeer(args, productionDaemonPeerDependencies())
}

func runCodexPeer(args []string, dependencies daemonPeerDependencies) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	plan, err := parseCodexPeerArgs(args, cwd, os.Getenv("CLAUDE_PEER_SESSION_NAME"))
	if err != nil {
		return err
	}
	if plan.mode == modePassthrough || plan.informationalPass {
		codex, executableErr := codexExecutable()
		if executableErr != nil {
			return executableErr
		}
		return Exec(codex, plan.originalArgs, nil)
	}
	if dependencies.prepare == nil {
		return errors.New("codex peer daemon client is unavailable")
	}
	permissionMode := "default"
	if plan.requestedYolo {
		permissionMode = "bypassPermissions"
	}
	profile := map[string]any{}
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		profile["profile"] = codexHome
	}
	prepared, err := dependencies.prepare(context.Background(), daemon.AttachmentPrepareRequest{
		Product: "codex", Kind: "interactive", ProfileIdentity: profile,
		Cwd: plan.requestedCwd, Name: plan.peerName, NameSource: plan.peerNameSource,
		Groups: append([]string(nil), plan.peerContext.groups...), PermissionMode: permissionMode,
		Intent: daemon.InteractiveLaunchIntent{
			Mode: string(plan.mode), Selector: plan.selectionTarget,
			SelectorIsName: plan.selectionTarget != "" && !threadIDPattern.MatchString(plan.selectionTarget),
			CwdExplicit:    plan.cwdExplicit, NativeArguments: append([]string(nil), plan.interactiveArgs...),
			PermissionExplicit: plan.yoloSpecified,
		},
	})
	if err != nil {
		return fmt.Errorf("prepare Codex attachment: %w", err)
	}
	return executeDaemonPreparedPeer(context.Background(), "codex", prepared, dependencies)
}

//nolint:unused // Legacy implementation retained only until T045 moves its App Server primitives into the daemon adapter.
func prepareInteractiveSession(selected Runtime, plan codexPlan) (string, string, error) {
	ownerPID := os.Getpid()
	ownerStart, err := capture(selected.Path, "launch", "proc-start", strconv.Itoa(ownerPID))
	if err != nil {
		return "", "", fmt.Errorf("capture launcher process identity: %w", err)
	}
	ownerStart = strings.TrimSpace(ownerStart)
	if plan.mode == modeFresh {
		threadID, launchErr := prepareFreshThread(selected.Path, plan, ownerPID, ownerStart)
		return validatePreparedThread(threadID, plan.requestedCwd, launchErr)
	}
	threadID, selectedCwd, bindErr := bindResumedThread(selected.Path, plan, ownerPID, ownerStart)
	return validatePreparedThread(threadID, selectedCwd, bindErr)
}

//nolint:unused // Legacy implementation retained only until T045 moves its App Server primitives into the daemon adapter.
func prepareFreshThread(runtimePath string, plan codexPlan, ownerPID int, ownerStart string) (string, error) {
	launchArgs := []string{
		"launch", "start", "--cwd", plan.requestedCwd,
		"--owner-pid", strconv.Itoa(ownerPID), "--owner-proc-start", ownerStart,
	}
	if plan.peerName != "" {
		launchArgs = append(launchArgs, "--name", plan.peerName, "--name-source", plan.peerNameSource)
	}
	contextArgs := plan.peerContext.launchArguments(plan.requestedYolo, plan.yoloSpecified)
	launchArgs = append(launchArgs, contextArgs...)
	if plan.requestedYolo {
		launchArgs = append(launchArgs, "--approval-policy", "never", "--sandbox", "danger-full-access")
	}
	threadID, err := capture(runtimePath, launchArgs...)
	if err != nil {
		return "", errors.New("failed to prepare the interactive session")
	}
	return strings.TrimSpace(threadID), nil
}

//nolint:unused // Legacy implementation retained only until T045 moves its App Server primitives into the daemon adapter.
func bindResumedThread(runtimePath string, plan codexPlan, ownerPID int, ownerStart string) (string, string, error) {
	bindArgs := resumedBindArguments(plan, ownerPID, ownerStart)
	binding, err := capture(runtimePath, bindArgs...)
	if err != nil {
		return "", "", errors.New("failed to bind the resumed session owner")
	}
	parts := strings.SplitN(strings.TrimSuffix(binding, "\n"), "\n", 2)
	if len(parts) != 2 {
		return "", "", errors.New("native runtime returned an incomplete resume binding")
	}
	return parts[0], parts[1], nil
}

func resumedBindArguments(plan codexPlan, ownerPID int, ownerStart string) []string {
	args := []string{
		"launch", "bind", "--target", plan.selectionTarget, "--cwd", plan.requestedCwd,
		"--cwd-explicit", strconv.FormatBool(plan.cwdExplicit),
		"--owner-pid", strconv.Itoa(ownerPID), "--owner-proc-start", ownerStart,
	}
	contextArgs := plan.peerContext.launchArguments(plan.requestedYolo, plan.yoloSpecified)
	args = append(args, contextArgs...)
	if plan.requestedYolo {
		args = append(args, "--approval-policy", "never", "--sandbox", "danger-full-access")
	}
	return args
}

//nolint:unused // Legacy implementation retained only until T045 moves its App Server primitives into the daemon adapter.
func validatePreparedThread(threadID, cwd string, err error) (string, string, error) {
	if err != nil {
		return "", "", err
	}
	if !threadIDPattern.MatchString(threadID) {
		return "", "", errors.New("native runtime returned an invalid thread UUID")
	}
	return threadID, cwd, nil
}

//nolint:unused // Legacy implementation retained only until T045 moves its App Server primitives into the daemon adapter.
func execInteractiveCodex(codex, threadID, cwd string, plan codexPlan) error {
	if _, present := os.LookupEnv("CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT"); !present {
		_ = os.Setenv("CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT", "1")
	}
	resetTerminalEnhancement()
	launchArgs := []string{"--remote", "unix://", "resume", threadID}
	if !plan.cwdExplicit {
		launchArgs = append(launchArgs, "-C", cwd)
	}
	switch plan.mode {
	case modeFresh:
		launchArgs = append(launchArgs, plan.interactiveArgs...)
	case modeResume:
		launchArgs = append(launchArgs, plan.interactiveArgs...)
	case modeFork, modePassthrough:
		return errors.New("internal launcher error: interactive Codex mode is not executable")
	}
	return Exec(codex, launchArgs, peerEnvironment(os.Environ(), threadID, "codex"))
}

func codexExecutable() (string, error) {
	return productExecutable("CODEX_PEER_CODEX_BIN", "codex")
}

func parseCodexPeerArgs(args []string, cwd, environmentName string) (codexPlan, error) {
	plan := codexPlan{mode: modeFresh, peerNameSource: "launch", requestedCwd: cwd}
	contextArgs, peerContext, err := extractPeerLaunchContext(args, codexOptionConsumesNext)
	if err != nil {
		return codexPlan{}, err
	}
	plan.peerContext = peerContext
	codexArgs, peerName, err := extractPeerNameArgs(contextArgs)
	if err != nil {
		return codexPlan{}, err
	}
	plan.peerName = peerName
	plan.originalArgs = append([]string(nil), codexArgs...)
	if informationalArgs(codexArgs) {
		plan.informationalPass, plan.mode = true, modePassthrough
		return plan, nil
	}
	requestedCwd, cwdExplicit, err := inspectWorkingDirectory(codexArgs, cwd)
	if err != nil {
		return codexPlan{}, err
	}
	plan.requestedCwd, plan.cwdExplicit = requestedCwd, cwdExplicit
	mode, commandIndex, err := classifyCodexMode(codexArgs)
	if err != nil {
		return codexPlan{}, err
	}
	plan.mode = mode
	return finalizeCodexPlan(plan, codexArgs, commandIndex, environmentName)
}

func extractPeerNameArgs(args []string) ([]string, string, error) {
	codexArgs := make([]string, 0, len(args))
	peerName := ""
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			codexArgs = append(codexArgs, args[index:]...)
			index = len(args)
		case argument == "-n" || argument == "--peer-name":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return nil, "", usageError("-n/--peer-name requires a non-empty value")
			}
			peerName = args[index+1]
			index++
		case strings.HasPrefix(argument, "-n=") || strings.HasPrefix(argument, "--peer-name="):
			peerName = strings.SplitN(argument, "=", 2)[1]
			if strings.TrimSpace(peerName) == "" {
				return nil, "", usageError("-n/--peer-name requires a non-empty value")
			}
		default:
			codexArgs = append(codexArgs, argument)
		}
	}
	return codexArgs, peerName, nil
}

func informationalArgs(args []string) bool {
	for _, argument := range beforeDoubleDash(args) {
		switch argument {
		case "-h", "--help", "-V", "--version":
			return true
		}
	}
	return false
}

func inspectWorkingDirectory(args []string, cwd string) (string, bool, error) {
	requestedCwd := cwd
	explicit := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		var consumed bool
		var err error
		requestedCwd, consumed, err = cwdOption(argument, args, index, requestedCwd)
		if err != nil {
			return "", false, err
		}
		if consumed {
			if argument == "-C" || argument == "--cd" {
				index++
			}
			explicit = true
			continue
		}
		if codexOptionConsumesNext(argument) {
			if index+1 >= len(args) {
				return "", false, usageError(argument + " requires a value")
			}
			index++
		}
	}
	resolved, err := canonicalDirectory(cwd, requestedCwd)
	return resolved, explicit, err
}

func cwdOption(argument string, args []string, index int, current string) (string, bool, error) {
	switch {
	case argument == "-C" || argument == "--cd":
		if index+1 >= len(args) || args[index+1] == "" {
			return current, false, usageError(argument + " requires a non-empty directory")
		}
		return args[index+1], true, nil
	case strings.HasPrefix(argument, "-C") && argument != "-C":
		return strings.TrimPrefix(argument, "-C"), true, nil
	case strings.HasPrefix(argument, "--cd="):
		value := strings.TrimPrefix(argument, "--cd=")
		if value == "" {
			return current, false, usageError("--cd requires a non-empty directory")
		}
		return value, true, nil
	default:
		return current, false, nil
	}
}

func classifyCodexMode(args []string) (codexMode, int, error) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return modeFresh, index, nil
		}
		if strings.HasPrefix(argument, "-") {
			if codexOptionConsumesNext(argument) {
				if index+1 >= len(args) {
					return "", -1, usageError(argument + " requires a value")
				}
				index++
			}
			continue
		}
		switch argument {
		case "resume":
			return modeResume, index, nil
		case "fork":
			return modeFork, index, nil
		case "agents", "exec", "e", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "remote-control", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "unarchive", "cloud", "app", "exec-server", "features", "help", "migrate-rollouts":
			return modePassthrough, index, nil
		default:
			return modeFresh, index, nil
		}
	}
	return modeFresh, -1, nil
}

func finalizeCodexPlan(plan codexPlan, forwarded []string, commandIndex int, environmentName string) (codexPlan, error) {
	if plan.mode == modePassthrough {
		if plan.peerName != "" {
			return codexPlan{}, usageError("-n/--peer-name applies only to a fresh interactive session")
		}
		return plan, nil
	}
	if plan.mode == modeFork {
		return codexPlan{}, usageError("fork is not owner-attested yet; use native codex fork or start a fresh peer")
	}
	if plan.mode == modeResume && plan.peerName != "" {
		return codexPlan{}, usageError("-n/--peer-name applies only to a fresh interactive session")
	}
	if err := applyEnvironmentName(&plan, environmentName); err != nil {
		return codexPlan{}, err
	}
	if err := configureInteractiveArgs(&plan, forwarded, commandIndex); err != nil {
		return codexPlan{}, err
	}
	if err := validateInteractiveOptions(&plan, forwarded); err != nil {
		return codexPlan{}, err
	}
	return plan, nil
}

func applyEnvironmentName(plan *codexPlan, environmentName string) error {
	if plan.mode != modeFresh || plan.peerName != "" || environmentName == "" {
		return nil
	}
	if strings.TrimSpace(environmentName) == "" {
		return usageError("CLAUDE_PEER_SESSION_NAME requires a non-empty value")
	}
	plan.peerName, plan.peerNameSource = environmentName, "explicit"
	return nil
}

func configureInteractiveArgs(plan *codexPlan, forwarded []string, commandIndex int) error {
	plan.interactiveArgs = append([]string(nil), forwarded...)
	if plan.mode != modeResume {
		return nil
	}
	targetIndex, err := explicitResumeTarget(forwarded, commandIndex)
	if err != nil {
		return err
	}
	if targetIndex < 0 {
		return usageError("resume requires an explicit UUID or session name; picker and --last are unsupported")
	}
	plan.selectionTarget = forwarded[targetIndex]
	plan.interactiveArgs = plan.interactiveArgs[:0]
	plan.interactiveArgs = append(plan.interactiveArgs, forwarded[:commandIndex]...)
	plan.interactiveArgs = append(plan.interactiveArgs, forwarded[commandIndex+1:targetIndex]...)
	plan.interactiveArgs = append(plan.interactiveArgs, forwarded[targetIndex+1:]...)
	return nil
}

func validateInteractiveOptions(plan *codexPlan, forwarded []string) error {
	for index := 0; index < len(forwarded); index++ {
		argument := forwarded[index]
		if argument == "--" {
			break
		}
		if argument == "--remote" || strings.HasPrefix(argument, "--remote=") || argument == "--remote-auth-token-env" || strings.HasPrefix(argument, "--remote-auth-token-env=") {
			return usageError("caller-controlled --remote options are not supported")
		}
		if isYolo(argument) {
			plan.requestedYolo = true
			plan.yoloSpecified = true
		}
		if codexOptionConsumesNext(argument) {
			index++
		}
	}
	if plan.peerContext.forceNoYolo {
		if plan.requestedYolo {
			return usageError("--no-yolo conflicts with a Codex yolo option")
		}
		plan.yoloSpecified = true
	}
	return nil
}

func explicitResumeTarget(args []string, commandIndex int) (int, error) {
	if commandIndex < 0 {
		return -1, nil
	}
	for index := commandIndex + 1; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if index+1 < len(args) {
				return index + 1, nil
			}
			return -1, nil
		}
		if argument == "--last" {
			return -1, usageError("resume requires an explicit UUID or session name; picker and --last are unsupported")
		}
		if strings.HasPrefix(argument, "-") {
			if codexOptionConsumesNext(argument) {
				if index+1 >= len(args) {
					return -1, usageError(argument + " requires a value")
				}
				index++
			}
			continue
		}
		return index, nil
	}
	return -1, nil
}

// codexOptionConsumesNext is deliberately limited to Codex's option arity.
// It helps locate the native subcommand and cwd but never removes, rewrites,
// or rejects the option. Unknown flags remain transparent boolean options;
// their native parser remains the authority.
func codexOptionConsumesNext(argument string) bool {
	valueOptions := []string{
		"--config", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--image",
		"--model", "--local-provider", "--profile", "--sandbox", "--cd", "--add-dir", "--ask-for-approval",
	}
	for _, option := range valueOptions {
		if argument == option {
			return true
		}
		if strings.HasPrefix(argument, option+"=") {
			return false
		}
	}
	if len(argument) > 2 {
		switch argument[:2] {
		case "-c", "-i", "-m", "-p", "-s", "-C", "-a":
			return false
		}
	}
	switch argument {
	case "-c", "-i", "-m", "-p", "-s", "-C", "-a":
		return true
	default:
		return false
	}
}

func canonicalDirectory(base, requested string) (string, error) {
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(base, requested)
	}
	resolved, err := pathidentity.ExistingDirectory(requested)
	if err != nil {
		return "", usageError("working directory does not exist: " + requested)
	}
	return resolved, nil
}

func beforeDoubleDash(args []string) []string {
	for index, argument := range args {
		if argument == "--" {
			return args[:index]
		}
	}
	return args
}

func isYolo(argument string) bool {
	return argument == "--yolo" || argument == "--dangerously-bypass-approvals-and-sandbox"
}

func usageError(message string) error {
	return &ExitError{Code: 2, Err: errors.New(message)}
}

//nolint:unused // Legacy terminal handoff retained only until T045 completes native plan extraction.
func resetTerminalEnhancement() {
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = tty.WriteString("\x1b[<1u\x1b[<u")
	_ = tty.Close()
}
