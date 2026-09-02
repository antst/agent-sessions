package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
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

// CodexDaemonPrepareRequest is the parsed native launch intent sent to the
// already-running user daemon. The launcher remains the terminal-owning
// process and the daemon remains the sole Agent Sessions authority.
type CodexDaemonPrepareRequest struct {
	Mode                   string
	Target                 string
	Cwd                    string
	CwdExplicit            bool
	Name                   string
	NameSource             string
	ApprovalPolicy         string
	Sandbox                string
	PermissionSpecified    bool
	Owner                  procinfo.Identity
	Groups                 []string
	GroupsSpecified        bool
	ParentSession          string
	ParentSpecified        bool
	InheritParentGroups    bool
	InheritGroupsSpecified bool
}

// CodexDaemonPrepareResult is the exact native handoff returned after durable
// daemon preparation and App Server selection.
type CodexDaemonPrepareResult struct {
	ThreadID string
	Name     string
	Cwd      string
}

// CodexResumeCandidate is one product-owned root-thread row offered to the
// terminal-owning wrapper when a name needs disambiguation.
type CodexResumeCandidate struct {
	ID        string
	Name      string
	Cwd       string
	UpdatedAt int64
}

// CodexSessionLister obtains the current product-owned root-thread list from
// the daemon's existing App Server connection. It makes no selection.
type CodexSessionLister func(context.Context) ([]CodexResumeCandidate, error)

// CodexDaemonPrepare submits one parsed Codex launch intent.
type CodexDaemonPrepare func(context.Context, CodexDaemonPrepareRequest) (CodexDaemonPrepareResult, error)

// CodexNativeLaunch is the product-issued thread and native process handoff.
// The caller stays alive as the process parent so its one presence connection
// proves exactly the child lifetime.
type CodexNativeLaunch struct {
	Executable  string
	Arguments   []string
	Environment []string
	ThreadID    string
	Name        string
	Groups      []string
}

// CodexNativeRunner starts the native TUI and holds its live presence stream.
type CodexNativeRunner func(context.Context, CodexNativeLaunch) error

// RunCodexPeerWithDaemon preserves the baseline parser and native exec path
// while replacing the legacy runtime/supervisor transaction with stateless
// product-list selection followed by one exact daemon preparation. It never
// starts or stops Agent Sessions authority.
func RunCodexPeerWithDaemon(ctx context.Context, args []string, list CodexSessionLister, prepare CodexDaemonPrepare, run CodexNativeRunner) error {
	if ctx == nil || list == nil || prepare == nil || run == nil {
		return errors.New("codex daemon launch coordinator is unavailable")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	codex, err := codexExecutable()
	if err != nil {
		return err
	}
	plan, err := projectCodexLaunchPlan(
		ctx, args, cwd, os.Getenv("CLAUDE_PEER_SESSION_NAME"), codex, list, terminalProductSessionChooser,
	)
	if err != nil {
		return err
	}
	if plan.mode == modePassthrough || plan.informationalPass {
		return Exec(codex, plan.originalArgs, nil)
	}
	owner, err := procinfo.CaptureIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture Codex launcher identity: %w", err)
	}
	approval, sandbox := "", ""
	if plan.requestedYolo {
		approval, sandbox = "never", "danger-full-access"
	}
	result, err := prepare(ctx, CodexDaemonPrepareRequest{
		Mode: string(plan.mode), Target: plan.selectionTarget, Cwd: plan.requestedCwd,
		CwdExplicit: plan.cwdExplicit, Name: plan.peerName, NameSource: plan.peerNameSource,
		ApprovalPolicy: approval, Sandbox: sandbox, Owner: owner,
		PermissionSpecified: plan.yoloSpecified,
		Groups:              append([]string(nil), plan.peerContext.groups...),
		GroupsSpecified:     plan.peerContext.groupsSpecified,
		ParentSession:       plan.peerContext.parentSession, ParentSpecified: plan.peerContext.parentSpecified,
		InheritParentGroups:    plan.peerContext.inheritParentGroups,
		InheritGroupsSpecified: plan.peerContext.inheritGroupsSpecified,
	})
	if err != nil {
		return err
	}
	if !threadIDPattern.MatchString(result.ThreadID) || strings.TrimSpace(result.Cwd) == "" {
		return errors.New("daemon returned an invalid Codex native handoff")
	}
	if _, present := os.LookupEnv("CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT"); !present {
		_ = os.Setenv("CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT", "1")
	}
	resetTerminalEnhancement()
	launchArgs := []string{"--remote", "unix://", "resume", result.ThreadID}
	if !plan.cwdExplicit {
		launchArgs = append(launchArgs, "-C", result.Cwd)
	}
	launchArgs = append(launchArgs, plan.interactiveArgs...)
	environment := envutil.Set(os.Environ(), peerSessionIDEnv, result.ThreadID)
	environment = envutil.Set(environment, peerProductEnv, "codex")
	environment = liveReportEnvironment(environment, result.Name, plan.peerContext.groups)
	return run(ctx, CodexNativeLaunch{
		Executable: codex, Arguments: launchArgs, Environment: environment,
		ThreadID: result.ThreadID, Name: result.Name, Groups: append([]string(nil), plan.peerContext.groups...),
	})
}

func projectCodexLaunchPlan(
	ctx context.Context,
	args []string,
	cwd, environmentName, executable string,
	list CodexSessionLister,
	choose productSessionChooser,
) (codexPlan, error) {
	descriptor, ok := productcatalog.ByID("codex")
	if !ok {
		return codexPlan{}, usageError("Codex product descriptor is unavailable")
	}
	projected, err := projectNativeArgumentTranslations(descriptor, args)
	if err != nil {
		return codexPlan{}, err
	}
	plan, err := parseCodexPeerArgs(projected, cwd, environmentName)
	if err != nil || plan.mode != modeResume {
		return plan, err
	}
	listProductRows := func(_, _ string) ([]productSession, error) {
		candidates, listErr := list(ctx)
		if listErr != nil {
			return nil, listErr
		}
		rows := make([]productSession, 0, len(candidates))
		for _, candidate := range candidates {
			rows = append(rows, productSession{
				ID: candidate.ID, Title: candidate.Name, Directory: candidate.Cwd, Updated: candidate.UpdatedAt,
			})
		}
		return rows, nil
	}
	plan.selectionTarget, err = resolveNativeArgumentHandlerValue(
		descriptor, executable, plan.requestedCwd, "resume", plan.selectionTarget,
		map[string]productArgumentHandler{"codex-thread-list": {
			isNativeSelector: func(selector string) bool { return threadIDPattern.MatchString(selector) },
			list:             listProductRows,
		}},
		choose,
	)
	return plan, err
}

func codexExecutable() (string, error) {
	return productExecutable("CODEX_PEER_CODEX_BIN", "codex")
}

func parseCodexPeerArgs(args []string, cwd, environmentName string) (codexPlan, error) {
	plan := codexPlan{mode: modeFresh, peerNameSource: "launch", requestedCwd: cwd}
	contextArgs, peerContext, err := scanPeerWrapperOptions("codex", args)
	if err != nil {
		return codexPlan{}, err
	}
	descriptor, ok := productcatalog.ByID("codex")
	if !ok {
		return codexPlan{}, usageError("Codex product descriptor is unavailable")
	}
	contextArgs, err = projectNativeLaunchPolicy(descriptor, contextArgs, peerContext.forceNoYolo)
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
	return productOptionConsumesNext("codex", argument)
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
