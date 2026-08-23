package launcher

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	grokLaunchTokenEnv = "AGENT_SESSIONS_GROK_LAUNCH_TOKEN"
	grokSessionIDEnv   = "AGENT_SESSIONS_GROK_SESSION_ID"
	grokProbeTimeout   = 5 * time.Second
	grokReadyTimeout   = 15 * time.Second
)

type grokMode string

const (
	grokModeFresh       grokMode = "fresh"
	grokModeResume      grokMode = "resume"
	grokModePassthrough grokMode = "passthrough"
)

var grokPassthroughCommands = map[string]struct{}{
	"agent": {}, "completions": {}, "dashboard": {}, "doctor": {},
	"du": {}, "disk-usage": {}, "export": {}, "help": {}, "inspect": {},
	"leader": {}, "login": {}, "logout": {}, "mcp": {}, "memory": {},
	"models": {}, "plugin": {}, "sessions": {}, "setup": {}, "trace": {},
	"update": {}, "version": {}, "v": {}, "worktree": {}, "wrap": {},
}

var grokCLIHelpMarkers = []string{
	"Grok Build TUI",
	"Usage: grok",
	"--leader-socket",
	"Commands:",
	"agent",
	"leader",
}

type grokPlan struct {
	mode                grokMode
	peerName            string
	requestedCwd        string
	cwdExplicit         bool
	sessionID           string
	resumeTarget        string
	lateBoundResume     bool
	permissionMode      string
	permissionSpecified bool
	peerContext         peerLaunchContext
	originalArgs        []string
	interactiveArgs     []string
	informationalPass   bool
}

type grokHostRequest struct {
	SessionID       string
	Cwd             string
	Name            string
	OwnerPID        int
	OwnerProcStart  string
	LaunchToken     string
	PermissionMode  string
	GrokBin         string
	AgentRuntimeDir string
	LateBoundResume bool
	NameSpecified   bool
	PeerContext     peerLaunchContext
	YoloSpecified   bool
}

type grokHostReady struct {
	Ready         bool   `json:"ready"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	LeaderSocket  string `json:"leader_socket"`
	ControlSocket string `json:"control_socket"`
}

type grokHostProcess struct {
	ready   grokHostReady
	process *os.Process
}

type grokHostStarter func(runtimePath string, request grokHostRequest) (grokHostProcess, error)

// RunGrokPeer starts an owner-attested Grok TUI backed by its own leader and
// ACP waker host. Native informational and administrative commands pass
// through without starting the shared Agent Sessions runtime.
func RunGrokPeer(args []string) error {
	return runGrokPeer(args, startGrokHost)
}

//nolint:gocyclo // Parse, preference, ownership, host-readiness, exec, and rollback failures require distinct diagnostics.
func runGrokPeer(args []string, startHost grokHostStarter) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	plan, err := parseGrokPeerArgs(args, cwd)
	if err != nil {
		return err
	}
	grok, err := grokExecutable()
	if err != nil {
		return err
	}
	if plan.mode == grokModePassthrough || plan.informationalPass {
		return Exec(grok, plan.originalArgs, nil)
	}
	runtimePath, err := grokRuntimeExecutable()
	if err != nil {
		return err
	}
	if !plan.lateBoundResume {
		resolved, resolveErr := resolvePeerLaunchContext(
			plan.sessionID, "grok", plan.peerContext,
			plan.permissionMode == "bypassPermissions", plan.permissionSpecified,
		)
		if resolveErr != nil {
			return fmt.Errorf("resolve Agent Sessions peer preferences: %w", resolveErr)
		}
		if resolved.Preference.AlwaysApprove && plan.permissionMode != "bypassPermissions" {
			plan.permissionMode = "bypassPermissions"
			plan.interactiveArgs = append(plan.interactiveArgs, "--always-approve")
		}
	}
	ownerPID := os.Getpid()
	ownerStart, err := capture(runtimePath, "launch", "proc-start", strconv.Itoa(ownerPID))
	if err != nil {
		return fmt.Errorf("capture launcher process identity: %w", err)
	}
	launchToken, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate Grok launch token: %w", err)
	}
	request := grokHostRequest{
		SessionID: plan.sessionID, Cwd: plan.requestedCwd, Name: plan.peerName,
		OwnerPID: ownerPID, OwnerProcStart: strings.TrimSpace(ownerStart),
		LaunchToken: launchToken, PermissionMode: plan.permissionMode, GrokBin: grok,
		AgentRuntimeDir: agentRuntimeDir(), LateBoundResume: plan.lateBoundResume,
		NameSpecified: plan.peerName != "",
		PeerContext:   plan.peerContext, YoloSpecified: plan.permissionSpecified,
	}
	if request.Name == "" && plan.lateBoundResume {
		request.Name = plan.resumeTarget
	}
	host, err := startHost(runtimePath, request)
	if err != nil {
		return err
	}
	managed := grokInteractiveArguments(plan, host.ready)
	environment := replaceGrokLaunchEnvironment(os.Environ(), launchToken, plan.sessionID, runtimePath)
	environment = peerEnvironment(environment, plan.sessionID, "grok")
	if err := Exec(grok, managed, environment); err != nil {
		if host.process != nil {
			_ = host.process.Kill()
		}
		return err
	}
	return nil
}

//nolint:gocyclo // CLI parsing preserves Grok arguments while extracting the shared peer layer.
func parseGrokPeerArgs(args []string, cwd string) (grokPlan, error) {
	plan := grokPlan{mode: grokModeFresh, requestedCwd: cwd, permissionMode: "default"}
	contextArgs, peerContext, err := extractPeerLaunchContext(args, grokOptionConsumesNext)
	if err != nil {
		return grokPlan{}, err
	}
	plan.peerContext = peerContext
	mode, commandIndex, informational, err := classifyGrokMode(contextArgs)
	if err != nil {
		return grokPlan{}, err
	}
	peerLimit := len(contextArgs)
	if mode == grokModePassthrough && commandIndex >= 0 {
		peerLimit = commandIndex
	}
	forwarded, peerName, err := extractGrokPeerName(contextArgs, peerLimit)
	if err != nil {
		return grokPlan{}, err
	}
	plan.mode, plan.peerName, plan.informationalPass = mode, peerName, informational
	plan.originalArgs = append([]string(nil), forwarded...)
	if mode == grokModePassthrough || informational {
		if mode == grokModePassthrough && !informational && peerName != "" {
			return grokPlan{}, usageError("-n/--peer-name applies only to an interactive Grok session")
		}
		return plan, nil
	}
	requestedCwd, explicit, err := inspectGrokWorkingDirectory(forwarded, cwd)
	if err != nil {
		return grokPlan{}, err
	}
	plan.requestedCwd, plan.cwdExplicit = requestedCwd, explicit
	interactive, sessionID, resume, permission, err := inspectManagedGrokArgs(forwarded)
	if err != nil {
		return grokPlan{}, err
	}
	plan.interactiveArgs = interactive
	plan.permissionMode = permission
	plan.permissionSpecified = grokPermissionSpecified(forwarded) || plan.peerContext.forceNoYolo
	if plan.peerContext.forceNoYolo && permission == "bypassPermissions" {
		return grokPlan{}, usageError("--no-yolo conflicts with a Grok always-approve option")
	}
	if resume {
		plan.mode = grokModeResume
		plan.resumeTarget = sessionID
		if threadIDPattern.MatchString(sessionID) {
			plan.sessionID = sessionID
		} else {
			plan.sessionID, err = newGrokSessionID()
			if err != nil {
				return grokPlan{}, fmt.Errorf("generate Grok attachment ID: %w", err)
			}
			plan.lateBoundResume = true
		}
		return plan, nil
	}
	if sessionID == "" {
		sessionID, err = newGrokSessionID()
		if err != nil {
			return grokPlan{}, fmt.Errorf("generate Grok session ID: %w", err)
		}
	}
	plan.sessionID = sessionID
	return plan, nil
}

func classifyGrokMode(args []string) (grokMode, int, bool, error) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return grokModeFresh, -1, false, nil
		}
		if isGrokInformational(argument) {
			return grokModePassthrough, -1, true, nil
		}
		if strings.HasPrefix(argument, "-") {
			if grokOptionConsumesNext(argument) {
				if index+1 >= len(args) {
					return "", -1, false, usageError(argument + " requires a value")
				}
				index++
			}
			continue
		}
		if _, ok := grokPassthroughCommands[argument]; ok {
			return grokModePassthrough, index, false, nil
		}
		return grokModeFresh, -1, false, nil
	}
	return grokModeFresh, -1, false, nil
}

func extractGrokPeerName(args []string, limit int) ([]string, string, error) {
	forwarded := make([]string, 0, len(args))
	peerName := ""
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" || index >= limit {
			forwarded = append(forwarded, args[index:]...)
			break
		}
		switch {
		case argument == "-n" || argument == "--peer-name":
			if index+1 >= limit || strings.TrimSpace(args[index+1]) == "" {
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
			forwarded = append(forwarded, argument)
			// A native option value that happens to equal -n/--peer-name is
			// data, not a launcher option. Keep the pair indivisible.
			if grokOptionConsumesNext(argument) && index+1 < limit {
				forwarded = append(forwarded, args[index+1])
				index++
			}
		}
	}
	return forwarded, peerName, nil
}

func inspectGrokWorkingDirectory(args []string, cwd string) (string, bool, error) {
	requested := cwd
	explicit := false
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		switch {
		case argument == "--cwd":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return "", false, usageError("--cwd requires a non-empty directory")
			}
			requested, explicit = args[index+1], true
			index++
		case strings.HasPrefix(argument, "--cwd="):
			requested, explicit = strings.TrimPrefix(argument, "--cwd="), true
			if strings.TrimSpace(requested) == "" {
				return "", false, usageError("--cwd requires a non-empty directory")
			}
		case grokOptionConsumesNext(argument):
			index++
		}
	}
	resolved, err := canonicalDirectory(cwd, requested)
	return resolved, explicit, err
}

func inspectManagedGrokArgs(args []string) ([]string, string, bool, string, error) {
	forwarded := make([]string, 0, len(args))
	identity := grokManagedIdentity{}
	permissionMode := "default"
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			forwarded = append(forwarded, args[index:]...)
			break
		}
		if err := rejectGrokManagedConflict(argument); err != nil {
			return nil, "", false, "", err
		}
		if handled, next, err := inspectGrokSandbox(args, index); handled {
			if err != nil {
				return nil, "", false, "", err
			}
			forwarded = append(forwarded, args[index:next+1]...)
			index = next
			continue
		}
		if handled, next, mode, err := inspectGrokPermission(args, index); handled {
			if err != nil {
				return nil, "", false, "", err
			}
			permissionMode = mode
			forwarded = append(forwarded, args[index:next+1]...)
			index = next
			continue
		}
		if handled, next, err := inspectGrokIdentity(args, index, &identity); handled {
			if err != nil {
				return nil, "", false, "", err
			}
			index = next
			continue
		}
		forwarded = append(forwarded, argument)
	}
	return forwarded, identity.sessionID, identity.resume, permissionMode, nil
}

func grokPermissionSpecified(args []string) bool {
	for _, argument := range beforeDoubleDash(args) {
		if argument == "--always-approve" || argument == "--yolo" || argument == "--permission-mode" ||
			strings.HasPrefix(argument, "--permission-mode=") {
			return true
		}
	}
	return false
}

type grokManagedIdentity struct {
	sessionID string
	resume    bool
	fresh     bool
}

func rejectGrokManagedConflict(argument string) error {
	switch {
	case argument == "--leader" || strings.HasPrefix(argument, "--leader="):
		return usageError("caller-controlled --leader is not supported by grok-peer")
	case argument == "--no-leader" || strings.HasPrefix(argument, "--no-leader="):
		return usageError("--no-leader is incompatible with managed Grok peers; use native grok for an isolated TUI")
	case argument == "--leader-socket" || strings.HasPrefix(argument, "--leader-socket="):
		return usageError("caller-controlled --leader-socket is not supported by grok-peer")
	case argument == "--fork-session":
		return usageError("--fork-session is not owner-attested yet; use native grok or start a fresh peer")
	case argument == "--continue" || strings.HasPrefix(argument, "--continue=") || argument == "-c":
		return usageError("--continue cannot identify an exact managed session; use grok-peer --resume UUID or native grok --continue")
	default:
		return nil
	}
}

func inspectGrokSandbox(args []string, index int) (bool, int, error) {
	argument := args[index]
	if argument == "--sandbox" {
		if index+1 >= len(args) {
			return true, index, usageError("--sandbox requires a value")
		}
		if args[index+1] != "off" {
			return true, index, usageError("managed Grok peers require --sandbox off")
		}
		return true, index + 1, nil
	}
	if strings.HasPrefix(argument, "--sandbox=") {
		if strings.TrimPrefix(argument, "--sandbox=") != "off" {
			return true, index, usageError("managed Grok peers require --sandbox off")
		}
		return true, index, nil
	}
	return false, index, nil
}

func inspectGrokPermission(args []string, index int) (bool, int, string, error) {
	argument := args[index]
	if argument == "--always-approve" || argument == "--yolo" {
		return true, index, "bypassPermissions", nil
	}
	if argument == "--permission-mode" {
		if index+1 >= len(args) {
			return true, index, "", usageError("--permission-mode requires a value")
		}
		return true, index + 1, grokPublishedPermissionMode(args[index+1]), nil
	}
	if strings.HasPrefix(argument, "--permission-mode=") {
		return true, index, grokPublishedPermissionMode(strings.TrimPrefix(argument, "--permission-mode=")), nil
	}
	return false, index, "", nil
}

func inspectGrokIdentity(args []string, index int, identity *grokManagedIdentity) (bool, int, error) {
	if value, next, matched, err := grokResumeValue(args, index); matched {
		if err != nil {
			return true, next, err
		}
		if identity.resume || identity.fresh {
			return true, next, usageError("Grok resume target was specified more than once")
		}
		identity.sessionID, identity.resume = value, true
		return true, next, nil
	}
	if value, next, matched, err := grokFreshSessionIDValue(args, index); matched {
		if err != nil {
			return true, next, err
		}
		if identity.resume || identity.fresh {
			return true, next, usageError("Grok session ID was specified more than once")
		}
		if !threadIDPattern.MatchString(value) {
			return true, next, usageError("--session-id requires a valid UUID")
		}
		identity.sessionID, identity.fresh = value, true
		return true, next, nil
	}
	return false, index, nil
}

func grokResumeValue(args []string, index int) (string, int, bool, error) {
	argument := args[index]
	if argument == "--resume" || argument == "-r" || argument == "--load" {
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
			return "", index, true, nil
		}
		return args[index+1], index + 1, true, nil
	}
	if strings.HasPrefix(argument, "--resume=") || strings.HasPrefix(argument, "--load=") {
		value := strings.SplitN(argument, "=", 2)[1]
		if strings.TrimSpace(value) == "" {
			return "", index, true, usageError("--resume= requires a native Grok resume target")
		}
		return value, index, true, nil
	}
	if strings.HasPrefix(argument, "-r") && argument != "-r" {
		return strings.TrimPrefix(argument, "-r"), index, true, nil
	}
	return "", index, false, nil
}

func grokFreshSessionIDValue(args []string, index int) (string, int, bool, error) {
	argument := args[index]
	if argument == "--session-id" || argument == "-s" {
		if index+1 >= len(args) {
			return "", index, true, usageError("--session-id requires a value")
		}
		return args[index+1], index + 1, true, nil
	}
	if strings.HasPrefix(argument, "--session-id=") {
		return strings.TrimPrefix(argument, "--session-id="), index, true, nil
	}
	if strings.HasPrefix(argument, "-s") && argument != "-s" {
		return strings.TrimPrefix(argument, "-s"), index, true, nil
	}
	return "", index, false, nil
}

func isGrokInformational(argument string) bool {
	switch argument {
	case "-h", "--help", "-v", "--version":
		return true
	default:
		return false
	}
}

func grokPublishedPermissionMode(mode string) string {
	if mode == "always-approve" || mode == "bypassPermissions" {
		return "bypassPermissions"
	}
	return "default"
}

// grokOptionConsumesNext is deliberately only an arity table. It never
// removes native options; Grok remains the parsing authority for forwarded
// argv. Optional-value --resume and --worktree are handled by managed parsing.
func grokOptionConsumesNext(argument string) bool {
	options := []string{
		"--agent", "--agents", "--allow", "--allowedTools", "--cwd", "--debug-file", "--deny", "--disallowedTools",
		"--disallowed-tools", "--json-schema", "--model", "--output-format",
		"--permission-mode", "--prompt-file", "--prompt-json", "--reasoning-effort",
		"--effort", "--rules", "--session-id", "--sandbox", "--single", "--system-prompt-override",
		"--system-prompt", "--tools", "--worktree-ref", "--ref", "--peer-name", "-n",
		"--compaction-mode", "--compaction-detail",
	}
	for _, option := range options {
		if argument == option {
			return true
		}
		if strings.HasPrefix(argument, option+"=") {
			return false
		}
	}
	if len(argument) > 2 {
		switch argument[:2] {
		case "-m", "-p", "-s":
			return false
		}
	}
	switch argument {
	case "-m", "-p", "-s":
		return true
	default:
		return false
	}
}

func grokExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("GROK_PEER_GROK_BIN")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", &ExitError{Code: 127, Err: fmt.Errorf("GROK_PEER_GROK_BIN is unavailable: %s", configured)}
		}
		if err := validateGrokCLI(path); err != nil {
			return "", &ExitError{Code: 127, Err: fmt.Errorf("GROK_PEER_GROK_BIN is not the headless Grok CLI: %s: %w", path, err)}
		}
		return path, nil
	}
	var rejected []string
	for _, candidate := range grokExecutableCandidates() {
		if err := validateGrokCLI(candidate); err == nil {
			return candidate, nil
		}
		rejected = append(rejected, candidate)
	}
	if len(rejected) == 0 {
		return "", &ExitError{Code: 127, Err: errors.New("grok CLI was not found on PATH or at ~/.grok/bin/grok")}
	}
	return "", &ExitError{Code: 127, Err: fmt.Errorf("no valid Grok CLI was found; rejected: %s", strings.Join(rejected, ", "))}
}

// grokRuntimeExecutable locates this installation's native runtime without
// bootstrapping Codex App Server or the Codex supervisor. Grok owns its own
// private leader lifecycle and must remain usable on a host without Codex.
func grokRuntimeExecutable() (string, error) {
	if err := ensureCodexHome(); err != nil {
		return "", err
	}
	runtimePath := strings.TrimSpace(os.Getenv("GROK_PEER_NATIVE_RUNTIME"))
	if runtimePath == "" {
		runtimePath = strings.TrimSpace(os.Getenv("CODEX_PEER_NATIVE_RUNTIME"))
	}
	if runtimePath == "" {
		pluginRoot := strings.TrimSpace(os.Getenv("GROK_PEER_PLUGIN_ROOT"))
		if pluginRoot == "" {
			pluginRoot = strings.TrimSpace(os.Getenv("CODEX_PEER_PLUGIN_ROOT"))
		}
		if pluginRoot == "" {
			executable, err := os.Executable()
			if err != nil {
				return "", fmt.Errorf("resolve Grok launcher executable: %w", err)
			}
			executable, err = filepath.EvalSymlinks(executable)
			if err != nil {
				return "", fmt.Errorf("resolve Grok launcher symlink: %w", err)
			}
			pluginRoot = filepath.Clean(filepath.Join(filepath.Dir(executable), "..", ".."))
		}
		platform, err := platformKey()
		if err != nil {
			return "", err
		}
		runtimePath = filepath.Join(pluginRoot, "bin", platform, "agent-session-runtime")
	}
	absolute, err := filepath.Abs(runtimePath)
	if err != nil {
		return "", fmt.Errorf("resolve native runtime: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("native runtime is unavailable: %s", absolute)
	}
	return absolute, nil
}

func grokExecutableCandidates() []string {
	seen := make(map[string]struct{})
	var candidates []string
	add := func(path string) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return
		}
		if _, ok := seen[absolute]; ok {
			return
		}
		info, err := os.Stat(absolute)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return
		}
		seen[absolute] = struct{}{}
		candidates = append(candidates, absolute)
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		add(filepath.Join(directory, "grok"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".grok", "bin", "grok"))
	}
	return candidates
}

func validateGrokCLI(path string) error {
	if err := rejectMacOSAppBundleExecutable(path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), grokProbeTimeout)
	defer cancel()
	// #nosec G702 -- The candidate is a resolved local executable and the probe argv is fixed.
	command := exec.CommandContext(ctx, path, "--no-auto-update", "--help")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("--help probe timed out")
	}
	if err != nil {
		return fmt.Errorf("--help probe failed: %w", err)
	}
	help := string(output)
	for _, marker := range grokCLIHelpMarkers {
		if !strings.Contains(help, marker) {
			return fmt.Errorf("--help output is missing %q", marker)
		}
	}
	return nil
}

// rejectMacOSAppBundleExecutable prevents a desktop application helper from
// being executed as part of CLI validation. macOS commonly exposes helpers
// through PATH symlinks, and its default filesystems accept case-variant
// spellings, so both the selected path and its resolved target are matched
// case-insensitively before running even the otherwise inert --help probe.
func rejectMacOSAppBundleExecutable(path string) error {
	if pathInsideMacOSAppContents(path) {
		return errors.New("executable is inside a macOS application bundle, not a standalone Grok CLI")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve Grok executable symlinks: %w", err)
	}
	if pathInsideMacOSAppContents(resolved) {
		return errors.New("executable resolves inside a macOS application bundle, not a standalone Grok CLI")
	}
	return nil
}

func pathInsideMacOSAppContents(path string) bool {
	parts := strings.Split(strings.ToLower(filepath.ToSlash(filepath.Clean(path))), "/")
	for index := 0; index+1 < len(parts); index++ {
		if strings.HasSuffix(parts[index], ".app") && parts[index+1] == "contents" {
			return true
		}
	}
	return false
}

func startGrokHost(runtimePath string, request grokHostRequest) (grokHostProcess, error) {
	command := exec.Command(runtimePath, grokHostArguments(request)...) //nolint:gosec // runtimePath is the validated native runtime from this Agent Sessions install.
	// The launcher is about to exec into the interactive Grok TUI. Keep the
	// cleanup watchdog outside that TUI's foreground process group so a normal
	// /quit cannot terminate it before it removes the private leader and durable
	// launch ownership. The host still observes the exact owner PID/start token
	// and exits as soon as that exec-preserved owner disappears.
	configureGrokHostProcess(command)
	command.Env = replaceGrokLaunchEnvironment(os.Environ(), request.LaunchToken, request.SessionID, runtimePath)
	command.Env = replaceLaneEnvironment(command.Env, agentRuntimeDirEnv, request.AgentRuntimeDir)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return grokHostProcess{}, fmt.Errorf("capture Grok host readiness: %w", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return grokHostProcess{}, fmt.Errorf("start Grok native host: %w", err)
	}
	reader := bufio.NewReader(stdout)
	type readResult struct {
		line string
		err  error
	}
	readiness := make(chan readResult, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		readiness <- readResult{line: line, err: readErr}
	}()
	timer := time.NewTimer(grokReadyTimeout)
	defer timer.Stop()
	var result readResult
	select {
	case result = <-readiness:
	case <-timer.C:
		_ = command.Process.Kill()
		_ = command.Wait()
		return grokHostProcess{}, errors.New("grok native host did not become ready")
	}
	if result.err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return grokHostProcess{}, fmt.Errorf("read Grok native host readiness: %w", result.err)
	}
	ready, err := parseGrokHostReady(result.line, request)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return grokHostProcess{}, err
	}
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		_ = command.Wait()
	}()
	return grokHostProcess{ready: ready, process: command.Process}, nil
}

func grokHostArguments(request grokHostRequest) []string {
	args := []string{
		"grok-host", "--session-id", request.SessionID, "--cwd", request.Cwd,
		"--owner-pid", strconv.Itoa(request.OwnerPID), "--owner-proc-start", request.OwnerProcStart,
		"--permission-mode", request.PermissionMode, "--grok-bin", request.GrokBin,
	}
	if request.AgentRuntimeDir != "" {
		args = append(args, "--agent-runtime-dir", request.AgentRuntimeDir)
	}
	if request.LateBoundResume {
		args = append(args, "--late-bound-resume")
		groupsJSON, _ := json.Marshal(request.PeerContext.groups)
		args = append(args,
			"--groups-json", string(groupsJSON),
			"--groups-specified="+boolString(request.PeerContext.groupsSpecified),
			"--parent-session", request.PeerContext.parentSession,
			"--parent-specified="+boolString(request.PeerContext.parentSpecified),
			"--inherit-parent-groups="+boolString(request.PeerContext.inheritParentGroups),
			"--inherit-groups-specified="+boolString(request.PeerContext.inheritGroupsSpecified),
			"--always-approve="+boolString(request.PermissionMode == "bypassPermissions"),
			"--always-approve-specified="+boolString(request.YoloSpecified),
		)
	}
	args = append(args, "--name-specified="+boolString(request.NameSpecified))
	if request.Name != "" {
		args = append(args, "--name", request.Name)
	}
	return args
}

func grokInteractiveArguments(plan grokPlan, ready grokHostReady) []string {
	managed := []string{"--leader", "--leader-socket", ready.LeaderSocket, "--sandbox", "off"}
	if plan.mode == grokModeFresh {
		managed = append(managed, "--session-id", plan.sessionID)
	} else {
		resumeTarget := plan.sessionID
		if plan.lateBoundResume {
			resumeTarget = plan.resumeTarget
		}
		managed = append(managed, "--resume")
		if resumeTarget != "" {
			managed = append(managed, resumeTarget)
		}
	}
	return append(managed, plan.interactiveArgs...)
}

func parseGrokHostReady(line string, request grokHostRequest) (grokHostReady, error) {
	var ready grokHostReady
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ready); err != nil {
		return grokHostReady{}, fmt.Errorf("decode Grok native host readiness: %w", err)
	}
	if !ready.Ready || ready.SessionID != request.SessionID || ready.Cwd != request.Cwd {
		return grokHostReady{}, errors.New("grok native host returned mismatched readiness identity")
	}
	if !filepath.IsAbs(ready.LeaderSocket) || !filepath.IsAbs(ready.ControlSocket) {
		return grokHostReady{}, errors.New("grok native host returned invalid socket paths")
	}
	return ready, nil
}

func newGrokSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func replaceGrokLaunchEnvironment(environment []string, token, sessionID, runtimePath string) []string {
	prefixes := []string{grokLaunchTokenEnv + "=", grokSessionIDEnv + "=", "GROK_PEER_NATIVE_RUNTIME="}
	updated := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefixes[0]) || strings.HasPrefix(entry, prefixes[1]) || strings.HasPrefix(entry, prefixes[2]) {
			continue
		}
		updated = append(updated, entry)
	}
	return append(updated, prefixes[0]+token, prefixes[1]+sessionID, prefixes[2]+runtimePath)
}
