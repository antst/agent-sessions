package launcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federator"
	"github.com/antst/agent-sessions/internal/pathidentity"
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

type qwenManagedResumeRecord struct {
	SessionID        string
	Name             string
	Product          string
	Cwd              string
	Profile          qwenprofile.Identity
	LaunchPreference qwenLaunchPreference
	Live             bool
}

type qwenPreparedLaunchCallbacks struct {
	Prepare             func() error
	StartAndCorroborate func() error
	Commit              func() error
	Rollback            func() error
}

type qwenPeerDependencies struct {
	readiness func(context.Context, qwenreadiness.Request) (qwenreadiness.Report, error)
	exec      func(string, []string, []string) error
}

// RunQwenPeer starts or resumes one managed native Qwen TUI. Informational and
// administrative native commands pass through without adopting managed state.
func RunQwenPeer(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(qwenPeerUsage())
		return nil
	}
	return runQwenPeer(args, qwenPeerDependencies{readiness: qwenreadiness.Check, exec: Exec})
}

//nolint:gocyclo // Readiness, resume, durable preparation, and exec rollback are intentionally separate gates.
func runQwenPeer(args []string, dependencies qwenPeerDependencies) error {
	if dependencies.readiness == nil || dependencies.exec == nil {
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
	agentRuntime := strings.TrimSpace(plan.agentRuntimeDir)
	if agentRuntime == "" {
		agentRuntime = agentRuntimeDir()
	}
	agentStatus, err := federator.ReadAgentStatus(agentRuntime)
	if err != nil {
		return fmt.Errorf("read Qwen host-agent status: %w", err)
	}
	if plan.stateDir != "" && filepath.Clean(plan.stateDir) != filepath.Clean(agentStatus.StateDir) {
		return errors.New("--state-dir does not match the running Agent Sessions host agent")
	}
	stateDir := agentStatus.StateDir
	if !filepath.IsAbs(stateDir) {
		return errors.New("qwen host-agent state directory is not absolute")
	}
	runtimePath, err := grokRuntimeExecutable()
	if err != nil {
		return err
	}
	readinessContext, cancelReadiness := context.WithTimeout(context.Background(), qwenReadinessTimeout)
	report, readinessErr := dependencies.readiness(readinessContext, qwenreadiness.Request{
		Executable: qwen, Workspace: plan.requestedCwd, Profile: plan.profile,
		ExpectedIntegrationVersion: qwenreadiness.IntegrationVersion,
		Source:                     qwenreadiness.NewNativeSource(os.Environ()),
	})
	cancelReadiness()
	if readinessErr != nil {
		return fmt.Errorf("check Qwen readiness: %w", readinessErr)
	}
	if !report.Ready {
		return qwenReadinessError(report)
	}
	if plan.mode == qwenPeerModeResume {
		plan, err = resolveQwenResumeFromAgent(plan, agentRuntime)
		if err != nil {
			return err
		}
	}
	if plan.peerName == "" {
		plan.peerName = filepath.Base(plan.requestedCwd)
		if plan.peerName == "." || plan.peerName == string(filepath.Separator) || plan.peerName == "" {
			plan.peerName = "qwen"
		}
	}
	return launchPreparedQwenPeer(plan, qwen, report.Version, runtimePath, agentRuntime, stateDir, dependencies.exec)
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

func resolveQwenResumeFromAgent(plan qwenPeerPlan, runtimeDir string) (qwenPeerPlan, error) {
	selector := plan.resumeTarget
	sessionID := selector
	if !threadIDPattern.MatchString(selector) {
		resolved, err := federator.ResolveSessionName(runtimeDir, "qwen", selector)
		if err != nil {
			return qwenPeerPlan{}, fmt.Errorf("resolve managed Qwen resume name: %w", err)
		}
		sessionID = resolved
	}
	record, err := federator.LookupManagedSession(runtimeDir, sessionID)
	if err != nil {
		return qwenPeerPlan{}, fmt.Errorf("lookup managed Qwen resume target: %w", err)
	}
	if record.Preference.Qwen == nil {
		return qwenPeerPlan{}, errors.New("managed Qwen resume target has no durable native launch context")
	}
	metadata := record.Preference.Qwen
	return resolveQwenManagedResume(plan, []qwenManagedResumeRecord{{
		SessionID: record.Preference.SessionID, Name: record.Name, Product: record.Preference.Product,
		Cwd: metadata.Cwd, Profile: qwenProfileFromFederator(metadata.Profile),
		LaunchPreference: qwenLaunchPreference(metadata.LaunchPreference), Live: record.Live,
	}})
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func launchPreparedQwenPeer(
	plan qwenPeerPlan,
	qwen, qwenVersion, runtimePath, agentRuntime, stateDir string,
	execCommand func(string, []string, []string) error,
) error {
	lifecycleRoot := federator.PeerLifecycleRootInState(stateDir, "qwen", plan.sessionID)
	productRoot := filepath.Join(stateDir, "qwen-peers")
	if err := os.MkdirAll(productRoot, 0o700); err != nil {
		return fmt.Errorf("create Qwen lifecycle namespace: %w", err)
	}
	productInfo, err := os.Lstat(productRoot)
	if err != nil || !productInfo.IsDir() || productInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("qwen lifecycle namespace is not a real directory")
	}
	if err := os.Mkdir(filepath.Dir(lifecycleRoot), 0o700); err != nil {
		return fmt.Errorf("create private Qwen session root: %w", err)
	}
	if err := os.Mkdir(lifecycleRoot, 0o700); err != nil {
		_ = os.Remove(filepath.Dir(lifecycleRoot))
		return fmt.Errorf("create private Qwen lifecycle root: %w", err)
	}
	inputPath := filepath.Join(lifecycleRoot, "input.jsonl")
	eventsPath := filepath.Join(lifecycleRoot, "events.jsonl")
	for _, path := range []string{inputPath, eventsPath} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // exact private lifecycle child.
		if err != nil {
			_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
			return fmt.Errorf("create private Qwen protocol file: %w", err)
		}
		_ = file.Close()
	}
	inputAttestation, err := federator.QwenArtifactAttestationForPath(inputPath)
	if err != nil {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return err
	}
	eventsAttestation, err := federator.QwenArtifactAttestationForPath(eventsPath)
	if err != nil {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return err
	}
	capability, capabilityDigest, err := newQwenLaunchCapability()
	if err != nil {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return err
	}
	pid := os.Getpid()
	procStart := federator.ProcessStart(pid)
	if pid <= 1 || procStart == "" {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return errors.New("capture Qwen launcher process identity")
	}
	profile := qwenProfileToFederator(plan.profile)
	registration := federator.PeerRegistration{
		Version: federator.GroupProtocolVersion, SessionID: plan.sessionID, Product: "qwen",
		// The public interactive dual-output protocol does not expose the live
		// approval mode. Keep the shared current-mode field empty instead of
		// presenting the durable launch preference as a live observation.
		Name: plan.peerName, PermissionMode: "", Cwd: plan.requestedCwd,
		PID: pid, ProcStart: procStart, LifecyclePID: pid, LifecycleProcStart: procStart,
		LifecycleRoot:        lifecycleRoot,
		QwenCapabilityDigest: capabilityDigest,
		QwenPreparation: &federator.QwenPreparationPayload{
			Version: 1, Profile: profile, CanonicalCwd: plan.requestedCwd,
			LaunchPreference: string(plan.launchPreference), InitialModeRequest: qwenInitialModeRequest(plan),
			Input:               inputAttestation,
			Events:              eventsAttestation,
			MCPCapabilityDigest: capabilityDigest,
		},
	}
	metadata := &federator.QwenSessionMetadata{
		Cwd: plan.requestedCwd, Profile: profile, LaunchPreference: string(plan.launchPreference),
		InitialModeRequest: qwenInitialModeRequest(plan),
	}
	preferenceRequest := qwenPreferenceRequest(plan, metadata)
	expected, err := federator.PreviewSessionPreferences(agentRuntime, preferenceRequest)
	if err != nil {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return fmt.Errorf("preview Qwen peer preferences: %w", err)
	}
	if _, err := federator.PreparePeerLaunch(agentRuntime, registration, preferenceRequest, expected.Preference); err != nil {
		_ = cleanupQwenLaunchPaths(lifecycleRoot, inputPath, eventsPath)
		return fmt.Errorf("prepare managed Qwen peer: %w", err)
	}
	nativeArgs := insertQwenManagedArgs(plan.nativeArgs,
		"--chat-recording=true", "--input-file", inputPath, "--json-file", eventsPath,
	)
	if plan.mode == qwenPeerModeFresh {
		nativeArgs = insertQwenManagedArgs(nativeArgs, "--session-id", plan.sessionID)
	}
	registrationBody, err := json.Marshal(registration)
	if err != nil {
		cleanupErr := cleanupPreparedQwenLaunchPaths(lifecycleRoot, inputAttestation, eventsAttestation)
		var rollbackErr error
		if cleanupErr == nil {
			rollbackErr = federator.CancelPeerPreparation(agentRuntime, registration)
		}
		return errors.Join(err, cleanupErr, rollbackErr)
	}
	hostArgs := make([]string, 0, 12+len(nativeArgs))
	hostArgs = append(hostArgs,
		"qwen-host", "--qwen", qwen, "--version", qwenVersion,
		"--runtime-dir", runtimeDirForQwenHost(), "--agent-runtime-dir", agentRuntime,
		"--registration-json", string(registrationBody), "--",
	)
	hostArgs = append(hostArgs, nativeArgs...)
	environment := qwenprofile.ApplyEnvironment(os.Environ(), plan.profile)
	environment = peerEnvironment(environment, plan.sessionID, "qwen")
	environment = envutil.Set(environment, qwenCapabilityEnv, capability)
	if err := execCommand(runtimePath, hostArgs, environment); err != nil {
		cleanupErr := cleanupPreparedQwenLaunchPaths(lifecycleRoot, inputAttestation, eventsAttestation)
		var rollbackErr error
		if cleanupErr == nil {
			rollbackErr = federator.CancelPeerPreparation(agentRuntime, registration)
		}
		return errors.Join(err, cleanupErr, rollbackErr)
	}
	return nil
}

func qwenPreferenceRequest(plan qwenPeerPlan, metadata *federator.QwenSessionMetadata) federator.ResolvePreferencesRequest {
	return federator.ResolvePreferencesRequest{
		SessionID: plan.sessionID, Product: "qwen", Kind: federator.SessionKindInteractive,
		Groups: plan.peerContext.groups, GroupsSpecified: plan.peerContext.groupsSpecified,
		ParentSessionID: plan.peerContext.parentSession, ParentSpecified: plan.peerContext.parentSpecified,
		InheritParentGroups:    plan.peerContext.inheritParentGroups,
		InheritGroupsSpecified: plan.peerContext.inheritGroupsSpecified,
		AlwaysApprove:          plan.launchPreference == qwenLaunchYolo,
		AlwaysApproveSpecified: plan.permissionSpecified, Qwen: metadata,
	}
}

func qwenInitialModeRequest(plan qwenPeerPlan) string {
	if plan.expectedInitialMode != "" {
		return plan.expectedInitialMode
	}
	return "native_default"
}

func qwenProfileToFederator(profile qwenprofile.Identity) federator.QwenProfileIdentity {
	return federator.QwenProfileIdentity{
		QwenHomeSet: profile.QwenHomeSet, QwenHome: profile.QwenHome,
		QwenRuntimeSet: profile.QwenRuntimeSet, QwenRuntimeDir: profile.QwenRuntimeDir,
		Fingerprint: profile.Fingerprint,
	}
}

func qwenProfileFromFederator(profile federator.QwenProfileIdentity) qwenprofile.Identity {
	return qwenprofile.Identity{
		QwenHomeSet: profile.QwenHomeSet, QwenHome: profile.QwenHome,
		QwenRuntimeSet: profile.QwenRuntimeSet, QwenRuntimeDir: profile.QwenRuntimeDir,
		Fingerprint: profile.Fingerprint,
	}
}

func newQwenLaunchCapability() (string, string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", "", fmt.Errorf("generate Qwen MCP capability: %w", err)
	}
	token := hex.EncodeToString(value)
	digest := sha256.Sum256([]byte(token))
	return token, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cleanupQwenLaunchPaths(root string, paths ...string) error {
	var result error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, err)
	}
	if err := os.Remove(filepath.Dir(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, err)
	}
	return result
}

func cleanupPreparedQwenLaunchPaths(root string, artifacts ...federator.QwenArtifactAttestation) error {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0o700 {
		return errors.New("qwen launch cleanup ownership root changed")
	}
	for _, artifact := range artifacts {
		if filepath.Dir(artifact.Path) != root || !federator.QwenArtifactIdentityMatches(artifact) {
			return fmt.Errorf("qwen launch cleanup retained changed artifact %s", artifact.Path)
		}
	}
	paths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		paths = append(paths, artifact.Path)
	}
	return cleanupQwenLaunchPaths(root, paths...)
}

func runtimeDirForQwenHost() string {
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "agent-sessions-"+strconv.Itoa(os.Getuid()))
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
	noYoloCount := countQwenOption(args, "--no-yolo")
	contextArgs, context, err := extractPeerLaunchContext(args, qwenOptionConsumesNext)
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
	if strings.Contains(argument, "=") {
		return false
	}
	switch argument {
	case "-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive",
		"-o", "--output-format", "-r", "--resume", "--approval-mode", "--session-id",
		"--json-file", "--json-fd", "--input-file", "--mcp-config", "--include-directories",
		"--allowed-mcp-server-names", "--theme", "-n", "--name", "--qwen-home",
		"--runtime-dir", "--state-dir":
		return true
	default:
		return false
	}
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

func resolveQwenManagedResume(plan qwenPeerPlan, candidates []qwenManagedResumeRecord) (qwenPeerPlan, error) {
	if plan.mode != qwenPeerModeResume || strings.TrimSpace(plan.resumeTarget) == "" {
		return qwenPeerPlan{}, errors.New("qwen managed resume requires a selector")
	}
	if len(candidates) == 0 {
		return qwenPeerPlan{}, fmt.Errorf("no managed Qwen session matches %q", plan.resumeTarget)
	}
	if len(candidates) > 1 {
		return qwenPeerPlan{}, fmt.Errorf("managed Qwen session %q is ambiguous; use an exact session UUID", plan.resumeTarget)
	}
	record := candidates[0]
	if record.Product != "qwen" {
		return qwenPeerPlan{}, fmt.Errorf("managed resume target belongs to %s, not Qwen", record.Product)
	}
	if record.Live {
		return qwenPeerPlan{}, fmt.Errorf("managed Qwen session %s is already live", record.SessionID)
	}
	if err := qwenprofile.MatchResume(record.Profile, plan.profile); err != nil {
		return qwenPeerPlan{}, err
	}
	plan.sessionID = record.SessionID
	plan.resumeTarget = record.SessionID
	plan.requestedCwd = record.Cwd
	if plan.peerName == "" {
		plan.peerName = record.Name
	}
	plan.nativeArgs = replaceQwenResumeTarget(plan.nativeArgs, record.SessionID)
	if !plan.permissionSpecified {
		plan.launchPreference = record.LaunchPreference
		mode := qwenModeForLaunchPreference(record.LaunchPreference)
		plan.expectedInitialMode = mode
		if mode != "" {
			plan.nativeArgs = insertQwenManagedArgs(plan.nativeArgs, "--approval-mode", mode)
		}
	}
	return plan, nil
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

func qwenModeForLaunchPreference(preference qwenLaunchPreference) string {
	switch preference {
	case qwenLaunchNativeDefault:
		return ""
	case qwenLaunchNonYolo:
		return "default"
	case qwenLaunchYolo:
		return "yolo"
	default:
		if mode, ok := strings.CutPrefix(string(preference), "native:"); ok {
			return mode
		}
		return ""
	}
}

func runQwenPreparedLaunch(callbacks qwenPreparedLaunchCallbacks) error {
	if callbacks.Prepare == nil || callbacks.StartAndCorroborate == nil || callbacks.Commit == nil || callbacks.Rollback == nil {
		return errors.New("qwen prepared launch callbacks are incomplete")
	}
	if err := callbacks.Prepare(); err != nil {
		return err
	}
	if err := callbacks.StartAndCorroborate(); err != nil {
		if rollbackErr := callbacks.Rollback(); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback Qwen launch: %w", rollbackErr))
		}
		return err
	}
	if err := callbacks.Commit(); err != nil {
		if rollbackErr := callbacks.Rollback(); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback Qwen launch: %w", rollbackErr))
		}
		return err
	}
	return nil
}
