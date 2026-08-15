package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const agyLaunchTokenEnv = "AGENT_SESSIONS_AGY_LAUNCH_TOKEN"

const agyCLIProbeTimeout = 5 * time.Second

var agyCLIHelpMarkers = []string{
	"Usage of agy:",
	"--conversation",
	"--prompt-interactive",
	"--output-format",
	"Available subcommands:",
}

type agyPlan struct {
	peerName        string
	forwarded       []string
	passthroughOnly bool
	permissionMode  string
}

// RunAgyPeer starts an Antigravity process whose hooks may publish only the
// exact live launcher process as a peer. Native informational commands and
// subcommands remain transparent passthroughs.
func RunAgyPeer(args []string) error {
	plan, err := parseAgyPeerArgs(args)
	if err != nil {
		return err
	}
	agy, err := agyExecutable()
	if err != nil {
		return err
	}
	if plan.passthroughOnly {
		return Exec(agy, plan.forwarded, nil)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	runtimePath, err := peerRuntimeExecutable()
	if err != nil {
		return err
	}
	ownerPID := os.Getpid()
	ownerStart, err := capture(runtimePath, "launch", "proc-start", strconv.Itoa(ownerPID))
	if err != nil {
		return fmt.Errorf("capture launcher process identity: %w", err)
	}
	prepareArgs := []string{
		"agy-launch", "prepare", "--cwd", cwd,
		"--owner-pid", strconv.Itoa(ownerPID), "--owner-proc-start", strings.TrimSpace(ownerStart),
		"--permission-mode", plan.permissionMode,
	}
	if plan.peerName != "" {
		prepareArgs = append(prepareArgs, "--name", plan.peerName)
	}
	token, err := capture(runtimePath, prepareArgs...)
	if err != nil {
		return errors.New("failed to prepare the Antigravity peer launch")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("native runtime returned an empty Antigravity launch token")
	}
	environment := append([]string{}, os.Environ()...)
	environment = replaceEnvironment(environment, agyLaunchTokenEnv, token)
	environment = prependEnvironmentPath(environment, filepath.Dir(runtimePath))
	return Exec(agy, plan.forwarded, environment)
}

func parseAgyPeerArgs(args []string) (agyPlan, error) {
	plan := agyPlan{permissionMode: "default"}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			plan.forwarded = append(plan.forwarded, args[index:]...)
			index = len(args)
		case argument == "-n" || argument == "--peer-name":
			index++
			if index >= len(args) {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
			plan.peerName = strings.TrimSpace(args[index]) // #nosec G602 -- index is checked against len(args) immediately above.
			if plan.peerName == "" {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
		case strings.HasPrefix(argument, "-n=") || strings.HasPrefix(argument, "--peer-name="):
			plan.peerName = strings.TrimSpace(strings.SplitN(argument, "=", 2)[1])
			if plan.peerName == "" {
				return agyPlan{}, usageError("-n/--peer-name requires a non-empty value")
			}
		default:
			plan.forwarded = append(plan.forwarded, argument)
		}
	}
	plan.passthroughOnly = agyPassthrough(plan.forwarded)
	if plan.passthroughOnly && plan.peerName != "" {
		return agyPlan{}, usageError("-n/--peer-name applies only to an interactive or headless Antigravity session")
	}
	for _, argument := range beforeDoubleDash(plan.forwarded) {
		if agyBypassRequested(argument) {
			plan.permissionMode = "bypassPermissions"
		}
	}
	return plan, nil
}

func agyBypassRequested(argument string) bool {
	for _, name := range []string{"--dangerously-skip-permissions", "-dangerously-skip-permissions"} {
		if argument == name {
			return true
		}
		if strings.HasPrefix(argument, name+"=") {
			enabled, err := strconv.ParseBool(strings.TrimPrefix(argument, name+"="))
			return err == nil && enabled
		}
	}
	return false
}

func agyPassthrough(args []string) bool {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" || argument == "-v" || argument == "--version" {
			return true
		}
		if strings.HasPrefix(argument, "-") {
			if agyOptionConsumesNext(argument) {
				index++
			}
			continue
		}
		switch argument {
		case "agent", "agents", "changelog", "help", "install", "models", "plugin", "plugins", "update":
			return true
		default:
			return false
		}
	}
	return false
}

func agyOptionConsumesNext(argument string) bool {
	options := []string{
		"--add-dir", "--agent", "--conversation", "--effort", "--json-schema", "--log-file",
		"--mode", "--model", "--output-format", "--print", "--print-timeout", "--project",
		"--prompt", "--prompt-interactive",
	}
	for _, option := range options {
		if argument == option {
			return true
		}
		if strings.HasPrefix(argument, option+"=") {
			return false
		}
	}
	if len(argument) > 2 && (strings.HasPrefix(argument, "-p") || strings.HasPrefix(argument, "-i")) {
		return false
	}
	return argument == "-p" || argument == "-i"
}

func agyExecutable() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("AGY_PEER_AGY_BIN")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", &ExitError{Code: 127, Err: fmt.Errorf("AGY_PEER_AGY_BIN is unavailable: %s", configured)}
		}
		if err := validateAgyCLI(path); err != nil {
			return "", &ExitError{Code: 127, Err: fmt.Errorf("AGY_PEER_AGY_BIN is not the headless Antigravity CLI: %s: %w", path, err)}
		}
		return path, nil
	}
	rejected := make([]string, 0)
	for _, path := range agyExecutableCandidates() {
		if err := validateAgyCLI(path); err == nil {
			return path, nil
		}
		rejected = append(rejected, path)
	}
	if len(rejected) == 0 {
		return "", &ExitError{Code: 127, Err: errors.New("headless Antigravity CLI agy was not found on PATH or at ~/.local/bin/agy")}
	}
	return "", &ExitError{Code: 127, Err: fmt.Errorf(
		"no headless Antigravity CLI named agy was found; rejected non-CLI candidates: %s; set AGY_PEER_AGY_BIN to the CLI executable",
		strings.Join(rejected, ", "),
	)}
}

func agyExecutableCandidates() []string {
	seen := make(map[string]struct{})
	candidates := make([]string, 0)
	addCandidate := func(candidate string) {
		candidate, err := filepath.Abs(candidate)
		if err != nil {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		addCandidate(filepath.Join(directory, "agy"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		addCandidate(filepath.Join(home, ".local", "bin", "agy"))
	}
	return candidates
}

func validateAgyCLI(path string) error {
	if isAntigravityGUIExecutable(path) {
		return errors.New("executable is Antigravity's GUI helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), agyCLIProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--help")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("--help probe timed out")
	}
	if err != nil {
		return fmt.Errorf("--help probe failed: %w", err)
	}
	help := string(output)
	for _, marker := range agyCLIHelpMarkers {
		if !strings.Contains(help, marker) {
			return fmt.Errorf("--help output is missing %q", marker)
		}
	}
	return nil
}

func isAntigravityGUIExecutable(path string) bool {
	current := path
	for range 64 {
		if knownAntigravityGUIPath(current) {
			return true
		}
		target, err := os.Readlink(current)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return knownAntigravityGUIPath(resolved)
	}
	return false
}

func knownAntigravityGUIPath(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains(cleaned, "/.antigravity/antigravity/bin/") ||
		strings.Contains(cleaned, "/Antigravity.app/Contents/")
}

func peerRuntimeExecutable() (string, error) {
	if path := os.Getenv("AGY_PEER_NATIVE_RUNTIME"); path != "" {
		return path, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve agy-peer executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve agy-peer symlink: %w", err)
	}
	path := filepath.Join(filepath.Dir(executable), "agent-session-runtime")
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("native runtime is unavailable beside agy-peer: %s", path)
	}
	return path, nil
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func prependEnvironmentPath(environment []string, directory string) []string {
	directory = filepath.Clean(directory)
	current := ""
	for _, item := range environment {
		if strings.HasPrefix(item, "PATH=") {
			current = strings.TrimPrefix(item, "PATH=")
			break
		}
	}
	for _, existing := range filepath.SplitList(current) {
		if filepath.Clean(existing) == directory {
			return environment
		}
	}
	value := directory
	if current != "" {
		value += string(os.PathListSeparator) + current
	}
	return replaceEnvironment(environment, "PATH", value)
}
