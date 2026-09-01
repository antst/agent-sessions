package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const (
	qwenCapabilityEnv    = "AGENT_SESSIONS_QWEN_CAPABILITY"
	qwenReadinessTimeout = 45 * time.Second
)

type qwenPeerMode string

const (
	qwenPeerModeFresh       qwenPeerMode = "fresh"
	qwenPeerModeResume      qwenPeerMode = "resume"
	qwenPeerModePassthrough qwenPeerMode = "passthrough"
)

type qwenLaunchPreference string

const (
	qwenLaunchNativeDefault qwenLaunchPreference = "native_default"
	qwenLaunchNonYolo       qwenLaunchPreference = "non_yolo"
	qwenLaunchYolo          qwenLaunchPreference = "yolo"
)

type qwenPeerPlan struct {
	mode                qwenPeerMode
	peerName            string
	requestedCwd        string
	sessionID           string
	resumeTarget        string
	launchPreference    qwenLaunchPreference
	expectedInitialMode string
	permissionSpecified bool
	profile             qwenprofile.Identity
	peerContext         peerLaunchContext
	agentRuntimeDir     string
	stateDir            string
	nativeArgs          []string
	originalArgs        []string
	informationalPass   bool
}

// QwenDaemonPrepareRequest is the exact presence-sensitive profile and native
// launch intent submitted before the client execs Qwen.
type QwenDaemonPrepareRequest struct {
	SessionID              string
	ResumeTarget           string
	Resume                 bool
	Name                   string
	NameSpecified          bool
	Cwd                    string
	LaunchPreference       string
	ExpectedInitialMode    string
	PermissionSpecified    bool
	Profile                qwenprofile.Identity
	QwenBin                string
	Owner                  procinfo.Identity
	Groups                 []string
	GroupsSpecified        bool
	ParentSession          string
	ParentSpecified        bool
	InheritParentGroups    bool
	InheritGroupsSpecified bool
}

// QwenDaemonPrepareResult returns the daemon-owned dual-output artifacts and
// launch capability required by Qwen's established integration protocol.
type QwenDaemonPrepareResult struct {
	SessionID           string
	Cwd                 string
	InputPath           string
	EventsPath          string
	Capability          string
	LaunchPreference    string
	ExpectedInitialMode string
}

// QwenDaemonPrepare submits one native Qwen launch intent.
type QwenDaemonPrepare func(context.Context, QwenDaemonPrepareRequest) (QwenDaemonPrepareResult, error)

type qwenPeerDependencies struct {
	readiness func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error)
	exec      func(string, []string, []string) error
	capture   func(int) (procinfo.Identity, error)
}

// RunQwenPeerWithDaemon preserves native profile selection, readiness,
// session identity, dual-output recording, and approval-mode argv while the
// one daemon owns lifecycle and cleanup.
//
//nolint:gocyclo // Native argv, profile, resume, attachment handoff, exec, and rollback are one launch transaction.
func RunQwenPeerWithDaemon(ctx context.Context, args []string, prepare QwenDaemonPrepare) error {
	return runQwenPeerWithDaemon(ctx, args, prepare, qwenPeerDependencies{
		readiness: qwenreadiness.Check,
		exec:      Exec,
		capture:   procinfo.CaptureIdentity,
	})
}

func runQwenPeerWithDaemon(
	ctx context.Context,
	args []string,
	prepare QwenDaemonPrepare,
	dependencies qwenPeerDependencies,
) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(qwenPeerUsage())
		return nil
	}
	if dependencies.readiness == nil || dependencies.exec == nil || dependencies.capture == nil {
		return errors.New("qwen peer dependencies are incomplete")
	}
	cwd, err := canonicalQwenCwd()
	if err != nil {
		return err
	}
	plan, err := parseQwenPeerArgs(args, cwd, os.LookupEnv)
	if err != nil {
		return err
	}
	qwen, err := qwenExecutable()
	if err != nil {
		return err
	}
	if plan.mode == qwenPeerModePassthrough || plan.informationalPass {
		return dependencies.exec(qwen, plan.nativeArgs, qwenprofile.ApplyEnvironment(os.Environ(), plan.profile))
	}
	if prepare == nil {
		return errors.New("qwen daemon preparation is unavailable")
	}
	readinessContext, cancel := context.WithTimeout(ctx, qwenReadinessTimeout)
	report, readinessErr := dependencies.readiness(readinessContext, qwenreadiness.Request{
		Executable: qwen, Workspace: plan.requestedCwd, Profile: plan.profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(os.Environ()),
	})
	cancel()
	if readinessErr != nil {
		return fmt.Errorf("check Qwen readiness: %w", readinessErr)
	}
	if !report.Ready {
		return qwenReadinessError(report)
	}
	if plan.mode == qwenPeerModeResume {
		resolvedID, resolvedName, resolveErr := resolveQwenProductSession(
			plan.profile, plan.requestedCwd, plan.resumeTarget,
		)
		if resolveErr != nil {
			return resolveErr
		}
		plan.sessionID = resolvedID
		plan.resumeTarget = resolvedID
		plan.nativeArgs = replaceQwenResumeTarget(plan.nativeArgs, resolvedID)
		if plan.peerName == "" {
			plan.peerName = resolvedName
		}
	}
	owner, err := dependencies.capture(os.Getpid())
	if err != nil {
		return fmt.Errorf("capture Qwen launcher process identity: %w", err)
	}
	result, err := prepare(ctx, QwenDaemonPrepareRequest{
		SessionID: plan.sessionID, ResumeTarget: plan.resumeTarget, Resume: plan.mode == qwenPeerModeResume,
		Name: plan.peerName, NameSpecified: strings.TrimSpace(plan.peerName) != "",
		Cwd: plan.requestedCwd, LaunchPreference: string(plan.launchPreference),
		ExpectedInitialMode: qwenInitialModeRequest(plan), PermissionSpecified: plan.permissionSpecified,
		Profile: plan.profile, QwenBin: qwen, Owner: owner,
		Groups: append([]string(nil), plan.peerContext.groups...), GroupsSpecified: plan.peerContext.groupsSpecified,
		ParentSession: plan.peerContext.parentSession, ParentSpecified: plan.peerContext.parentSpecified,
		InheritParentGroups:    plan.peerContext.inheritParentGroups,
		InheritGroupsSpecified: plan.peerContext.inheritGroupsSpecified,
	})
	if err != nil {
		return err
	}
	if !threadIDPattern.MatchString(result.SessionID) || !filepath.IsAbs(result.InputPath) ||
		!filepath.IsAbs(result.EventsPath) || strings.TrimSpace(result.Capability) == "" {
		return errors.New("daemon returned an invalid Qwen launch boundary")
	}
	nativeArgs := insertQwenManagedArgs(plan.nativeArgs,
		"--chat-recording=true", "--input-file", result.InputPath, "--json-file", result.EventsPath,
	)
	if plan.mode == qwenPeerModeFresh {
		nativeArgs = insertQwenManagedArgs(nativeArgs, "--session-id", result.SessionID)
	} else {
		nativeArgs = replaceQwenResumeTarget(nativeArgs, result.SessionID)
	}
	if plan.mode == qwenPeerModeResume && !plan.permissionSpecified && result.ExpectedInitialMode != "" {
		nativeArgs = insertQwenManagedArgs(nativeArgs, "--approval-mode", result.ExpectedInitialMode)
	}
	environment := qwenprofile.ApplyEnvironment(os.Environ(), plan.profile)
	environment = daemonPeerEnvironment(environment, result.SessionID, "qwen")
	environment = envutil.Set(environment, qwenCapabilityEnv, result.Capability)
	return dependencies.exec(qwen, nativeArgs, environment)
}

func qwenExecutable() (string, error) {
	return productExecutable("QWEN_PEER_QWEN_BIN", "qwen")
}

func canonicalQwenCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	canonical, err := pathidentity.ExistingDirectory(cwd)
	if err != nil {
		return "", fmt.Errorf("canonicalize Qwen working directory: %w", err)
	}
	return canonical, nil
}

// resolveQwenProductSession leaves exact native UUIDs to Qwen and resolves a
// human selector only against Qwen's own transcript titles. Agent Sessions
// keeps no name index or session record of its own.
func resolveQwenProductSession(
	profile qwenprofile.Identity,
	cwd, selector string,
) (string, string, error) {
	selector = strings.TrimSpace(selector)
	if threadIDPattern.MatchString(selector) {
		return selector, "", nil
	}
	if selector == "" {
		return "", "", errors.New("Qwen resume selector is empty")
	}
	home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
	if err != nil {
		return "", "", fmt.Errorf("resolve Qwen product profile: %w", err)
	}
	projects, err := os.ReadDir(filepath.Join(home, "projects"))
	if err != nil {
		return "", "", fmt.Errorf("list Qwen product sessions: %w", err)
	}
	type match struct{ id, title string }
	matches := make([]match, 0, 1)
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		chats, readErr := os.ReadDir(filepath.Join(home, "projects", project.Name(), "chats"))
		if readErr != nil {
			continue
		}
		for _, chat := range chats {
			if chat.IsDir() || filepath.Ext(chat.Name()) != ".jsonl" {
				continue
			}
			id := strings.TrimSuffix(chat.Name(), ".jsonl")
			if !threadIDPattern.MatchString(id) {
				continue
			}
			path := filepath.Join(home, "projects", project.Name(), "chats", chat.Name())
			title, transcriptCwd, ok := qwenProductTranscriptTitle(path, id)
			if !ok || !strings.EqualFold(title, selector) ||
				filepath.Clean(transcriptCwd) != filepath.Clean(cwd) {
				continue
			}
			matches = append(matches, match{id: id, title: title})
		}
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("Qwen has no session named %q in %s", selector, cwd)
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("Qwen session name %q is ambiguous; use an exact session UUID", selector)
	}
	return matches[0].id, matches[0].title, nil
}

func qwenProductTranscriptTitle(path, sessionID string) (string, string, bool) {
	file, err := os.Open(path) //nolint:gosec // Path is read from Qwen's own selected-profile session inventory.
	if err != nil {
		return "", "", false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	first, cwd, latest := true, "", ""
	for scanner.Scan() {
		var event struct {
			SessionID     string `json:"sessionId"`
			Cwd           string `json:"cwd"`
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			SystemPayload struct {
				CustomTitle string `json:"customTitle"`
				TitleSource string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != sessionID {
			return "", "", false
		}
		if first {
			first, cwd = false, event.Cwd
		}
		if event.Type == "system" && event.Subtype == "custom_title" &&
			strings.TrimSpace(event.SystemPayload.TitleSource) != "auto" {
			latest = strings.TrimSpace(event.SystemPayload.CustomTitle)
		}
	}
	return latest, cwd, scanner.Err() == nil && !first && latest != ""
}

func qwenReadinessError(report qwenreadiness.Report) error {
	if !report.IntegrationReady {
		return fmt.Errorf("qwen Agent Sessions integration is not ready in the selected profile; run make install-qwen for that exact profile")
	}
	messages := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		messages = append(messages, issue.Code+": "+issue.Message)
	}
	return fmt.Errorf("qwen is not ready: %s", strings.Join(messages, "; "))
}

func qwenInitialModeRequest(plan qwenPeerPlan) string {
	if plan.expectedInitialMode != "" {
		return plan.expectedInitialMode
	}
	return "native_default"
}

var qwenPassthroughCommands = map[string]struct{}{
	"auth": {}, "channel": {}, "extensions": {}, "hooks": {}, "mcp": {},
	"review": {}, "serve": {}, "sessions": {}, "update": {},
}

// parseQwenPeerArgs extracts only Agent Sessions-owned options. Qwen remains
// the parsing authority for every forwarded native argument.
//
//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func parseQwenPeerArgs(args []string, cwd string, lookup qwenprofile.LookupEnv) (qwenPeerPlan, error) {
	plan := qwenPeerPlan{
		mode: qwenPeerModeFresh, requestedCwd: cwd,
		launchPreference: qwenLaunchNativeDefault,
	}
	if lookup == nil {
		return qwenPeerPlan{}, usageError("Qwen profile environment lookup is unavailable")
	}
	noYoloCount := countQwenOption(args, "--no-yolo")
	contextArgs, context, err := scanPeerWrapperOptions("qwen", args)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	plan.peerContext = context
	if noYoloCount > 1 {
		return qwenPeerPlan{}, usageError("Qwen wrapper permission option was specified more than once")
	}

	forwarded, wrapper, err := extractQwenWrapperOptions(contextArgs)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	plan.peerName = wrapper.name
	plan.agentRuntimeDir = wrapper.runtimeDir
	plan.stateDir = wrapper.stateDir
	plan.originalArgs = append([]string(nil), forwarded...)

	profileLookup := lookup
	if wrapper.qwenHomeSet {
		if !filepath.IsAbs(wrapper.qwenHome) {
			return qwenPeerPlan{}, usageError("--qwen-home requires a non-empty absolute path")
		}
		profileLookup = func(name string) (string, bool) {
			if name == "QWEN_HOME" {
				return wrapper.qwenHome, true
			}
			return lookup(name)
		}
	}
	plan.profile, err = qwenprofile.ResolveEnvironment(profileLookup)
	if err != nil {
		return qwenPeerPlan{}, usageError(err.Error())
	}

	mode, informational, err := classifyQwenPeerMode(forwarded)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	plan.mode, plan.informationalPass = mode, informational
	if mode == qwenPeerModePassthrough || informational {
		if wrapper.hasManagedInteractiveOption() || contextHasManagedQwenValues(context) {
			return qwenPeerPlan{}, usageError("managed Qwen peer options apply only to an interactive session")
		}
		plan.nativeArgs = append([]string(nil), forwarded...)
		return plan, nil
	}

	managed, resumeTarget, nativeMode, nativeModeCount, err := inspectManagedQwenArgs(forwarded)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	plan.nativeArgs = managed
	if resumeTarget != "" {
		plan.mode, plan.resumeTarget = qwenPeerModeResume, resumeTarget
	} else {
		plan.sessionID, err = newGrokSessionID()
		if err != nil {
			return qwenPeerPlan{}, fmt.Errorf("generate Qwen session ID: %w", err)
		}
	}

	yoloCount := countQwenOption(forwarded, "--yolo")
	if yoloCount > 1 || noYoloCount > 1 {
		return qwenPeerPlan{}, usageError("Qwen wrapper permission option was specified more than once")
	}
	if yoloCount > 0 && noYoloCount > 0 {
		return qwenPeerPlan{}, usageError("--yolo conflicts with --no-yolo")
	}
	if (yoloCount > 0 || noYoloCount > 0) && nativeModeCount > 0 {
		return qwenPeerPlan{}, usageError("Qwen wrapper permission option conflicts with native --approval-mode")
	}
	if nativeModeCount > 1 {
		return qwenPeerPlan{}, usageError("native --approval-mode was specified more than once")
	}
	plan.permissionSpecified = yoloCount > 0 || noYoloCount > 0 || nativeModeCount > 0
	switch {
	case yoloCount > 0:
		plan.launchPreference, plan.expectedInitialMode = qwenLaunchYolo, "yolo"
		plan.nativeArgs = replaceQwenWrapperYolo(plan.nativeArgs, "yolo")
	case noYoloCount > 0:
		plan.launchPreference, plan.expectedInitialMode = qwenLaunchNonYolo, "default"
		plan.nativeArgs = insertQwenManagedArgs(plan.nativeArgs, "--approval-mode", "default")
	case nativeModeCount == 1:
		plan.launchPreference = qwenLaunchPreference("native:" + nativeMode)
		plan.expectedInitialMode = nativeMode
	}
	return plan, nil
}

type qwenWrapperOptions struct {
	name, qwenHome, runtimeDir, stateDir       string
	qwenHomeSet, nameSet, runtimeSet, stateSet bool
}

func (o qwenWrapperOptions) hasManagedInteractiveOption() bool {
	return o.nameSet || o.qwenHomeSet || o.runtimeSet || o.stateSet
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func extractQwenWrapperOptions(args []string) ([]string, qwenWrapperOptions, error) {
	forwarded := make([]string, 0, len(args))
	var options qwenWrapperOptions
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			forwarded = append(forwarded, args[index:]...)
			break
		}
		consume := func(label string) (string, error) {
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return "", usageError(label + " requires a non-empty value")
			}
			index++
			return args[index], nil
		}
		switch {
		case argument == "-n" || argument == "--name":
			if options.nameSet {
				return nil, qwenWrapperOptions{}, usageError("-n/--name was specified more than once")
			}
			value, err := consume("-n/--name")
			if err != nil {
				return nil, qwenWrapperOptions{}, err
			}
			options.name, options.nameSet = value, true
		case strings.HasPrefix(argument, "-n=") || strings.HasPrefix(argument, "--name="):
			if options.nameSet {
				return nil, qwenWrapperOptions{}, usageError("-n/--name was specified more than once")
			}
			_, options.name, _ = strings.Cut(argument, "=")
			if strings.TrimSpace(options.name) == "" {
				return nil, qwenWrapperOptions{}, usageError("-n/--name requires a non-empty value")
			}
			options.nameSet = true
		case argument == "--qwen-home":
			if options.qwenHomeSet {
				return nil, qwenWrapperOptions{}, usageError("--qwen-home was specified more than once")
			}
			value, err := consume("--qwen-home")
			if err != nil {
				return nil, qwenWrapperOptions{}, err
			}
			options.qwenHome, options.qwenHomeSet = value, true
		case strings.HasPrefix(argument, "--qwen-home="):
			if options.qwenHomeSet {
				return nil, qwenWrapperOptions{}, usageError("--qwen-home was specified more than once")
			}
			options.qwenHome = strings.TrimPrefix(argument, "--qwen-home=")
			if strings.TrimSpace(options.qwenHome) == "" {
				return nil, qwenWrapperOptions{}, usageError("--qwen-home requires a non-empty absolute path")
			}
			options.qwenHomeSet = true
		case argument == "--runtime-dir":
			if options.runtimeSet {
				return nil, qwenWrapperOptions{}, usageError("--runtime-dir was specified more than once")
			}
			value, err := consume("--runtime-dir")
			if err != nil {
				return nil, qwenWrapperOptions{}, err
			}
			options.runtimeDir, options.runtimeSet = value, true
		case strings.HasPrefix(argument, "--runtime-dir="):
			if options.runtimeSet {
				return nil, qwenWrapperOptions{}, usageError("--runtime-dir was specified more than once")
			}
			options.runtimeDir = strings.TrimPrefix(argument, "--runtime-dir=")
			if strings.TrimSpace(options.runtimeDir) == "" {
				return nil, qwenWrapperOptions{}, usageError("--runtime-dir requires a non-empty value")
			}
			options.runtimeSet = true
		case argument == "--state-dir":
			if options.stateSet {
				return nil, qwenWrapperOptions{}, usageError("--state-dir was specified more than once")
			}
			value, err := consume("--state-dir")
			if err != nil {
				return nil, qwenWrapperOptions{}, err
			}
			options.stateDir, options.stateSet = value, true
		case strings.HasPrefix(argument, "--state-dir="):
			if options.stateSet {
				return nil, qwenWrapperOptions{}, usageError("--state-dir was specified more than once")
			}
			options.stateDir = strings.TrimPrefix(argument, "--state-dir=")
			if strings.TrimSpace(options.stateDir) == "" {
				return nil, qwenWrapperOptions{}, usageError("--state-dir requires a non-empty value")
			}
			options.stateSet = true
		default:
			forwarded = append(forwarded, argument)
			if qwenOptionConsumesNext(argument) && index+1 < len(args) {
				forwarded = append(forwarded, args[index+1])
				index++
			}
		}
	}
	return forwarded, options, nil
}

func classifyQwenPeerMode(args []string) (qwenPeerMode, bool, error) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return qwenPeerModeFresh, false, nil
		}
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			return qwenPeerModePassthrough, true, nil
		}
		if strings.HasPrefix(argument, "-") {
			if qwenOptionConsumesNext(argument) {
				if index+1 >= len(args) {
					return "", false, usageError(argument + " requires a value")
				}
				index++
			}
			continue
		}
		if _, ok := qwenPassthroughCommands[argument]; ok {
			return qwenPeerModePassthrough, false, nil
		}
	}
	return qwenPeerModeFresh, false, nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func inspectManagedQwenArgs(args []string) ([]string, string, string, int, error) {
	forwarded := make([]string, 0, len(args))
	resumeTarget := ""
	nativeMode := ""
	nativeModeCount := 0
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			forwarded = append(forwarded, args[index:]...)
			break
		}
		switch {
		case argument == "--continue" || argument == "-c":
			return nil, "", "", 0, usageError("--continue cannot identify an exact managed Qwen session; use qwen-peer --resume UUID or native qwen")
		case argument == "--fork-session" || strings.HasPrefix(argument, "--fork-session="):
			return nil, "", "", 0, usageError("--fork-session is not owner-attested; use native qwen or start a fresh peer")
		case argument == "--session-id" || strings.HasPrefix(argument, "--session-id="):
			return nil, "", "", 0, usageError("caller-controlled --session-id is incompatible with a managed Qwen peer")
		case argument == "--resume" || argument == "-r":
			if resumeTarget != "" {
				return nil, "", "", 0, usageError("Qwen resume target was specified more than once")
			}
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "-") {
				return nil, "", "", 0, usageError("--resume requires an exact UUID or unique managed session name")
			}
			resumeTarget = args[index+1]
			forwarded = append(forwarded, argument, resumeTarget)
			index++
		case strings.HasPrefix(argument, "--resume=") || strings.HasPrefix(argument, "-r="):
			if resumeTarget != "" {
				return nil, "", "", 0, usageError("Qwen resume target was specified more than once")
			}
			_, resumeTarget, _ = strings.Cut(argument, "=")
			if strings.TrimSpace(resumeTarget) == "" {
				return nil, "", "", 0, usageError("--resume requires an exact UUID or unique managed session name")
			}
			forwarded = append(forwarded, argument)
		case argument == "--approval-mode":
			nativeModeCount++
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return nil, "", "", 0, usageError("--approval-mode requires a non-empty value")
			}
			nativeMode = args[index+1]
			forwarded = append(forwarded, argument, nativeMode)
			index++
		case strings.HasPrefix(argument, "--approval-mode="):
			nativeModeCount++
			nativeMode = strings.TrimPrefix(argument, "--approval-mode=")
			if strings.TrimSpace(nativeMode) == "" {
				return nil, "", "", 0, usageError("--approval-mode requires a non-empty value")
			}
			forwarded = append(forwarded, argument)
		default:
			forwarded = append(forwarded, argument)
			if qwenOptionConsumesNext(argument) && index+1 < len(args) {
				forwarded = append(forwarded, args[index+1])
				index++
			}
		}
	}
	return forwarded, resumeTarget, nativeMode, nativeModeCount, nil
}

func replaceQwenWrapperYolo(args []string, mode string) []string {
	result := make([]string, 0, len(args)+1)
	for index, argument := range args {
		if argument == "--" {
			return append(result, args[index:]...)
		}
		if argument == "--yolo" {
			result = append(result, "--approval-mode", mode)
			continue
		}
		result = append(result, argument)
	}
	return result
}

func insertQwenManagedArgs(args []string, managed ...string) []string {
	for index, argument := range args {
		if argument == "--" {
			result := append([]string(nil), args[:index]...)
			result = append(result, managed...)
			return append(result, args[index:]...)
		}
	}
	return append(append([]string(nil), managed...), args...)
}

func countQwenOption(args []string, name string) int {
	count := 0
	for _, argument := range beforeDoubleDash(args) {
		if argument == name || strings.HasPrefix(argument, name+"=") {
			count++
		}
	}
	return count
}

func contextHasManagedQwenValues(context peerLaunchContext) bool {
	return context.groupsSpecified || context.inheritGroupsSpecified || context.forceNoYolo
}

// qwenOptionConsumesNext is an arity table only. It protects native option
// values from being interpreted as wrapper options; it does not remove them.
func qwenOptionConsumesNext(argument string) bool {
	return productOptionConsumesNext("qwen", argument)
}

func qwenPeerUsage() string {
	return `Usage: qwen-peer [WRAPPER_OPTIONS...] [NATIVE_QWEN_OPTIONS...]
       qwen-peer --resume UUID_OR_UNIQUE_NAME [WRAPPER_OPTIONS...] [NATIVE_QWEN_OPTIONS...]

Wrapper options:
  -n, --name NAME             stable Agent Sessions name
  -g, --group GROUP           repeatable explicit group
      --inherit-groups        inherit immediate-parent groups
      --no-inherit-groups     do not inherit immediate-parent groups
      --yolo                  launch with native --approval-mode yolo
      --no-yolo               launch with native --approval-mode default
      --qwen-home PATH        explicit absolute native Qwen profile
      --runtime-dir PATH      Agent Sessions runtime selection
      --state-dir PATH        Agent Sessions state selection
      --resume TARGET         exact UUID or unique managed name
  -h, --help                  show this help

Native boundary:
  Native --approval-mode MODE passes through only when --yolo/--no-yolo is absent.
  Arguments after -- are caller content and are never interpreted by qwen-peer.
`
}

func replaceQwenResumeTarget(args []string, sessionID string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return append(result, args[index:]...)
		}
		switch {
		case argument == "--resume" || argument == "-r":
			result = append(result, "--resume", sessionID)
			if index+1 < len(args) {
				index++
			}
		case strings.HasPrefix(argument, "--resume=") || strings.HasPrefix(argument, "-r="):
			result = append(result, "--resume", sessionID)
		default:
			result = append(result, argument)
		}
	}
	return result
}
