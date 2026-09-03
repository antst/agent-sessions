package launcher

import (
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
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/qwenprofile"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const (
	QwenInputFileEnv     = "AGENT_SESSIONS_QWEN_INPUT_FILE"
	QwenEventsFileEnv    = "AGENT_SESSIONS_QWEN_EVENTS_FILE"
	qwenReadinessTimeout = 45 * time.Second
	qwenRegistryInterval = 100 * time.Millisecond
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
	mode              qwenPeerMode
	peerName          string
	requestedCwd      string
	sessionID         string
	resumeTarget      string
	launchPreference  qwenLaunchPreference
	profile           qwenprofile.Identity
	peerContext       peerLaunchContext
	agentRuntimeDir   string
	stateDir          string
	nativeArgs        []string
	originalArgs      []string
	informationalPass bool
}

type qwenPeerDependencies struct {
	readiness func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error)
	exec      func(string, []string, []string) error
	run       QwenNativeRunner
}

// QwenNativeLaunch is one launcher-owned interactive child and its two native
// live protocol files. Qwen emits the selected session identity into EventsPath.
type QwenNativeLaunch struct {
	Executable  string
	Arguments   []string
	Environment []string
	Cwd         string
	QwenHome    string
	InputPath   string
	EventsPath  string
	Groups      []string
}

// QwenNativeRunner starts Qwen, confirms its product-emitted identity, and
// holds the one live presence stream for exactly the native child's lifetime.
type QwenNativeRunner func(context.Context, QwenNativeLaunch) error

// RunQwenPeer preserves native profile selection, readiness, exact session
// identity, and approval-mode argv. The launcher owns the two Qwen protocol
// files and its one live presence stream for exactly the native child's lifetime.
//
//nolint:gocyclo // Native argv, profile, resume, launch files, and child lifetime form one operation.
func RunQwenPeer(ctx context.Context, args []string, run QwenNativeRunner) error {
	return runQwenPeer(ctx, args, qwenPeerDependencies{
		readiness: qwenreadiness.Check,
		exec:      Exec,
		run:       run,
	})
}

func runQwenPeer(
	ctx context.Context,
	args []string,
	dependencies qwenPeerDependencies,
) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(qwenPeerUsage())
		return nil
	}
	if dependencies.readiness == nil || dependencies.exec == nil || dependencies.run == nil {
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
	qwenHome, err := qwenprofile.EffectiveHome(plan.profile, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("resolve Qwen product profile: %w", err)
	}
	root, inputPath, eventsPath, err := createQwenLaunchFiles()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	nameContext, stopName := context.WithCancel(ctx)
	nameDone := make(chan struct{})
	if plan.mode == qwenPeerModeFresh && strings.TrimSpace(plan.peerName) != "" {
		go func() {
			defer close(nameDone)
			waitAndSubmitQwenNativeName(nameContext, qwenHome, plan.sessionID, inputPath, plan.peerName)
		}()
	} else {
		close(nameDone)
	}
	defer func() {
		stopName()
		<-nameDone
	}()
	nativeArgs := insertQwenManagedArgs(plan.nativeArgs,
		"--chat-recording=true", "--input-file", inputPath, "--json-file", eventsPath,
	)
	if plan.mode == qwenPeerModeFresh {
		nativeArgs = insertQwenManagedArgs(nativeArgs, "--session-id", plan.sessionID)
	}
	environment := qwenprofile.ApplyEnvironment(os.Environ(), plan.profile)
	environment = daemonPeerEnvironment(environment, plan.sessionID, "qwen")
	environment = liveReportEnvironment(environment, plan.peerName, plan.peerContext.groups)
	environment = envutil.Set(environment, QwenInputFileEnv, inputPath)
	environment = envutil.Set(environment, QwenEventsFileEnv, eventsPath)
	return dependencies.run(ctx, QwenNativeLaunch{
		Executable: qwen, Arguments: nativeArgs, Environment: environment,
		Cwd: plan.requestedCwd, QwenHome: qwenHome, InputPath: inputPath, EventsPath: eventsPath,
		Groups: append([]string(nil), plan.peerContext.groups...),
	})
}

func createQwenLaunchFiles() (string, string, string, error) {
	root, err := os.MkdirTemp("", "agent-sessions-qwen-")
	if err != nil {
		return "", "", "", fmt.Errorf("create Qwen launch directory: %w", err)
	}
	inputPath, eventsPath := filepath.Join(root, "input.jsonl"), filepath.Join(root, "events.jsonl")
	for _, path := range []string{inputPath, eventsPath} {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			_ = os.RemoveAll(root)
			return "", "", "", fmt.Errorf("create Qwen launch file: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			_ = os.RemoveAll(root)
			return "", "", "", fmt.Errorf("close Qwen launch file: %w", closeErr)
		}
	}
	return root, inputPath, eventsPath, nil
}

func waitAndSubmitQwenNativeName(ctx context.Context, home, sessionID, inputPath, name string) {
	ticker := time.NewTicker(qwenRegistryInterval)
	defer ticker.Stop()
	for {
		if qwenSessionRegistered(home, sessionID) {
			_ = submitQwenNativeName(inputPath, name)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func submitQwenNativeName(inputPath, name string) error {
	record, err := json.Marshal(map[string]string{"type": "submit", "text": "/rename " + strings.TrimSpace(name)})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(inputPath, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // launcher-owned Qwen input file.
	if err != nil {
		return fmt.Errorf("open Qwen native name input: %w", err)
	}
	record = append(record, '\n')
	_, writeErr := file.Write(record)
	return errors.Join(writeErr, file.Close())
}

func qwenSessionRegistered(home, sessionID string) bool {
	entries, err := os.ReadDir(filepath.Join(home, "sessions"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(home, "sessions", entry.Name())) //nolint:gosec // Qwen owns this selected-profile registry.
		if readErr != nil {
			continue
		}
		var record struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(body, &record) == nil && record.SessionID == sessionID {
			return true
		}
	}
	return false
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
	descriptor, ok := productcatalog.ByID("qwen")
	if !ok {
		return qwenPeerPlan{}, usageError("Qwen product descriptor is unavailable")
	}
	projected, err := projectNativeArgumentTranslations(descriptor, productcatalog.NativeArgumentPeer, args)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	args = projected
	noYoloCount := countQwenOption(args, "--no-yolo")
	contextArgs, context, err := scanPeerWrapperOptionsWithArity(args, func(argument string) bool {
		if argument == "--resume" || argument == "-r" {
			return false
		}
		return qwenOptionConsumesNext(argument)
	})
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

	managed, resume, resumeTarget, nativeMode, nativeModeCount, err := inspectManagedQwenArgs(forwarded)
	if err != nil {
		return qwenPeerPlan{}, err
	}
	plan.nativeArgs = managed
	if resume {
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
	switch {
	case yoloCount > 0:
		plan.launchPreference = qwenLaunchYolo
		plan.nativeArgs, err = projectNativeLaunchPolicy(descriptor, plan.nativeArgs, false)
		if err != nil {
			return qwenPeerPlan{}, err
		}
	case noYoloCount > 0:
		plan.launchPreference = qwenLaunchNonYolo
		plan.nativeArgs = insertQwenManagedArgs(plan.nativeArgs, "--approval-mode", "default")
	case nativeModeCount == 1:
		plan.launchPreference = qwenLaunchPreference("native:" + nativeMode)
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
		if argument == "--resume" || argument == "-r" {
			continue
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
func inspectManagedQwenArgs(args []string) ([]string, bool, string, string, int, error) {
	forwarded := make([]string, 0, len(args))
	resume := false
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
			return nil, false, "", "", 0, usageError("--continue is not the managed resume surface; use qwen-peer --resume or native qwen")
		case argument == "--fork-session" || strings.HasPrefix(argument, "--fork-session="):
			return nil, false, "", "", 0, usageError("--fork-session is not owner-attested; use native qwen or start a fresh peer")
		case argument == "--session-id" || strings.HasPrefix(argument, "--session-id="):
			return nil, false, "", "", 0, usageError("caller-controlled --session-id is incompatible with a managed Qwen peer")
		case argument == "--resume" || argument == "-r":
			if resume {
				return nil, false, "", "", 0, usageError("Qwen resume was specified more than once")
			}
			resume = true
			forwarded = append(forwarded, argument)
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				resumeTarget = args[index+1]
				forwarded = append(forwarded, resumeTarget)
				index++
			}
		case strings.HasPrefix(argument, "--resume=") || strings.HasPrefix(argument, "-r="):
			if resume {
				return nil, false, "", "", 0, usageError("Qwen resume was specified more than once")
			}
			resume = true
			_, resumeTarget, _ = strings.Cut(argument, "=")
			forwarded = append(forwarded, argument)
		case argument == "--approval-mode":
			nativeModeCount++
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return nil, false, "", "", 0, usageError("--approval-mode requires a non-empty value")
			}
			nativeMode = args[index+1]
			forwarded = append(forwarded, argument, nativeMode)
			index++
		case strings.HasPrefix(argument, "--approval-mode="):
			nativeModeCount++
			nativeMode = strings.TrimPrefix(argument, "--approval-mode=")
			if strings.TrimSpace(nativeMode) == "" {
				return nil, false, "", "", 0, usageError("--approval-mode requires a non-empty value")
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
	return forwarded, resume, resumeTarget, nativeMode, nativeModeCount, nil
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
       qwen-peer --resume [NATIVE_SELECTOR] [WRAPPER_OPTIONS...] [NATIVE_QWEN_OPTIONS...]

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
      --resume [SELECTOR]     pass the optional selector to Qwen verbatim
  -h, --help                  show this help

Native boundary:
  Native --approval-mode MODE passes through only when --yolo/--no-yolo is absent.
  Arguments after -- are caller content and are never interpreted by qwen-peer.
`
}
